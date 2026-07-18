package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRerankHTTPClientScores(t *testing.T) {
	var gotQuery string
	var gotDocs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string   `json:"query"`
			Documents []string `json:"documents"`
		}
		_ = json.Unmarshal(body, &req)
		gotQuery, gotDocs = req.Query, req.Documents
		scores := make([]float64, len(req.Documents))
		for i := range req.Documents {
			scores[i] = float64(i) // deterministic
		}
		_ = json.NewEncoder(w).Encode(map[string][]float64{"scores": scores})
	}))
	defer srv.Close()

	c := newReranker(srv.URL, 2*time.Second)
	got, err := c.Rerank(context.Background(), "find budget", []string{"doc a", "doc b", "doc c"})
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "find budget" || len(gotDocs) != 3 {
		t.Fatalf("server saw query=%q docs=%v", gotQuery, gotDocs)
	}
	if len(got) != 3 || got[0] != 0 || got[2] != 2 {
		t.Fatalf("scores=%v", got)
	}
}

func TestRerankHTTPClientShapeMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string][]float64{"scores": {1.0}}) // 1 score for 2 docs
	}))
	defer srv.Close()

	c := newReranker(srv.URL, 2*time.Second)
	_, err := c.Rerank(context.Background(), "q", []string{"a", "b"})
	if !errors.Is(err, errRerankShape) {
		t.Fatalf("want errRerankShape, got %v", err)
	}
}

func TestRerankHTTPClientNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"loading"}`))
	}))
	defer srv.Close()

	c := newReranker(srv.URL, 2*time.Second)
	if _, err := c.Rerank(context.Background(), "q", []string{"a"}); err == nil {
		t.Fatalf("expected error on HTTP 503")
	}
}

func TestRerankHTTPClientEmptyDocsNoCall(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()

	c := newReranker(srv.URL, 2*time.Second)
	got, err := c.Rerank(context.Background(), "q", nil)
	if err != nil || got != nil {
		t.Fatalf("empty docs: got=%v err=%v", got, err)
	}
	if called {
		t.Fatalf("client should not call the service for empty documents")
	}
}
