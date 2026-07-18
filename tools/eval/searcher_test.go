package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestGatewaySearcherParsesResponse(t *testing.T) {
	var gotQuery url.Values
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hits":[{"doc_id":"d1"},{"doc_id":"d2"}],"total":2,"took_ms":37,"cached":false}`))
	}))
	defer srv.Close()

	s := NewGatewaySearcher(srv.URL, func(string) (string, error) { return "tok123", nil })
	p := Pipeline{Name: "hybrid_rerank", APIMode: "hybrid", Extra: url.Values{"rerank": {"1"}}}
	out, err := s.Search(context.Background(), "alice", "budget review", p, 20)
	if err != nil {
		t.Fatal(err)
	}

	if len(out.DocIDs) != 2 || out.DocIDs[0] != "d1" || out.DocIDs[1] != "d2" {
		t.Fatalf("docIDs=%v want [d1 d2]", out.DocIDs)
	}
	if out.ServerMs != 37 {
		t.Fatalf("serverMs=%d want 37", out.ServerMs)
	}
	if gotPath != "/v1/search" {
		t.Fatalf("path=%q want /v1/search", gotPath)
	}
	if gotAuth != "Bearer tok123" {
		t.Fatalf("auth=%q want Bearer tok123", gotAuth)
	}
	if gotQuery.Get("q") != "budget review" || gotQuery.Get("mode") != "hybrid" ||
		gotQuery.Get("limit") != "20" || gotQuery.Get("rerank") != "1" {
		t.Fatalf("query params wrong: %v", gotQuery)
	}
}

func TestGatewaySearcherNon200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"missing token"}`))
	}))
	defer srv.Close()

	s := NewGatewaySearcher(srv.URL, func(string) (string, error) { return "tok", nil })
	if _, err := s.Search(context.Background(), "alice", "q", Pipeline{APIMode: "hybrid"}, 10); err == nil {
		t.Fatalf("expected error on HTTP 401")
	}
}
