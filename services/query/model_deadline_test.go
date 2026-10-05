package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestModelClientsForwardRemainingDeadline(t *testing.T) {
	for _, name := range []string{"embed", "rerank", "clip"} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ms, err := strconv.Atoi(r.Header.Get("X-Request-Timeout-Ms"))
				if err != nil || ms <= 0 || ms > 750 {
					t.Errorf("model deadline header = %q, want remaining parent budget <=750ms", r.Header.Get("X-Request-Timeout-Ms"))
				}
				switch name {
				case "embed":
					_, _ = w.Write([]byte(`[[0.1]]`))
				case "rerank":
					_, _ = w.Write([]byte(`{"scores":[0.1]}`))
				case "clip":
					_, _ = w.Write([]byte(`{"embeddings":[[0.1]]}`))
				}
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
			defer cancel()
			var err error
			switch name {
			case "embed":
				_, err = newTEIEmbedder(srv.URL, 1, 2*time.Second).Embed(ctx, "query")
			case "rerank":
				_, err = newReranker(srv.URL, 2*time.Second).Rerank(ctx, "query", []string{"document"})
			case "clip":
				_, err = newClipEmbedder(srv.URL, 1, 2*time.Second).EmbedText(ctx, "query")
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExpiredModelBudgetDoesNotCallService(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, embedErr := newTEIEmbedder(srv.URL, 1, time.Second).Embed(ctx, "query")
	_, rerankErr := newReranker(srv.URL, time.Second).Rerank(ctx, "query", []string{"document"})
	_, clipErr := newClipEmbedder(srv.URL, 1, time.Second).EmbedText(ctx, "query")
	for _, err := range []error{embedErr, rerankErr, clipErr} {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expired model deadline not preserved: %v", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("expired model request made %d calls", calls.Load())
	}
}
