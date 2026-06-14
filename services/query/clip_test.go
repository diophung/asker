package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClipEmbedderEmbedText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embed/text" {
			t.Errorf("path = %q, want /embed/text", r.URL.Path)
		}
		var req struct {
			Inputs []string `json:"inputs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(req.Inputs) != 1 || req.Inputs[0] != "a sunset" {
			t.Errorf("inputs = %v, want [\"a sunset\"]", req.Inputs)
		}
		_ = json.NewEncoder(w).Encode(struct {
			Embeddings [][]float32 `json:"embeddings"`
		}{Embeddings: [][]float32{{0.1, 0.2, 0.3, 0.4}}})
	}))
	defer srv.Close()

	e := newClipEmbedder(srv.URL, 4, time.Second)
	v, err := e.EmbedText(context.Background(), "a sunset")
	if err != nil {
		t.Fatalf("EmbedText: %v", err)
	}
	if len(v) != 4 {
		t.Fatalf("len(v) = %d, want 4", len(v))
	}
}

func TestClipEmbedderDimMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(struct {
			Embeddings [][]float32 `json:"embeddings"`
		}{Embeddings: [][]float32{{0.1, 0.2, 0.3}}}) // 3 != 4
	}))
	defer srv.Close()

	e := newClipEmbedder(srv.URL, 4, time.Second)
	if _, err := e.EmbedText(context.Background(), "x"); !errors.Is(err, errClipDim) {
		t.Errorf("error = %v, want errClipDim", err)
	}
}

func TestClipEmbedderHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "model loading", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	e := newClipEmbedder(srv.URL, 4, time.Second)
	if _, err := e.EmbedText(context.Background(), "x"); err == nil {
		t.Error("EmbedText against a 503 must error")
	}
}

func TestClipEmbedderWrongBatchSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(struct {
			Embeddings [][]float32 `json:"embeddings"`
		}{Embeddings: [][]float32{{0.1, 0.2, 0.3, 0.4}, {0.5, 0.6, 0.7, 0.8}}}) // 2 for 1 input
	}))
	defer srv.Close()

	e := newClipEmbedder(srv.URL, 4, time.Second)
	if _, err := e.EmbedText(context.Background(), "x"); err == nil {
		t.Error("EmbedText with a 2-vector response for 1 input must error")
	}
}

func TestClipEmbedderConnRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // refuse connections

	e := newClipEmbedder(srv.URL, 4, time.Second)
	if _, err := e.EmbedText(context.Background(), "x"); err == nil {
		t.Error("EmbedText against a down service must error")
	}
}
