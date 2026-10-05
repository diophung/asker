package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSearchDeadlineCancelsBlockedDependency(t *testing.T) {
	for _, path := range []string{"/v1/search?q=budget", "/v1/search/email?q=budget"} {
		started := time.Now()
		h := searchDeadline(20*time.Millisecond, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
			if r.Context().Err() != context.DeadlineExceeded {
				t.Error("dependency did not observe deadline")
			}
			w.WriteHeader(http.StatusGatewayTimeout)
		}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("blocked search consumed %s", elapsed)
		}
		if rec.Code != http.StatusGatewayTimeout {
			t.Fatalf("status = %d", rec.Code)
		}
	}
}

func TestSearchDeadlinePreservesEarlierCallerAndOtherRoutes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	want, _ := ctx.Deadline()
	h := searchDeadline(5*time.Second, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, ok := r.Context().Deadline()
		if !ok || !got.Equal(want) {
			t.Fatalf("caller deadline changed: %v", got)
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/search", nil).WithContext(ctx))
	h = searchDeadline(time.Second, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if _, ok := r.Context().Deadline(); ok {
			t.Fatal("nonsearch route unexpectedly time limited")
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/upload", nil))
}
