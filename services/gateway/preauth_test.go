package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// countingHandler counts how many requests reach it (i.e. were NOT throttled).
type countingHandler struct{ hits int }

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.hits++
	w.WriteHeader(http.StatusOK)
}

func TestPreAuthGlobalCeilingShedsExcess(t *testing.T) {
	now := time.Now()
	// Global: 1/sec, burst 2. Per-IP disabled.
	l := newPreAuthLimiterClock(0, 1, 2, false, func() time.Time { return now })
	next := &countingHandler{}
	h := l.middleware(next)

	codes := make([]int, 0, 4)
	for i := 0; i < 4; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/search", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
	}
	// First two pass (burst), the rest are 429 (clock frozen, no refill).
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Errorf("burst requests = %v, want first two 200", codes)
	}
	if codes[2] != http.StatusTooManyRequests || codes[3] != http.StatusTooManyRequests {
		t.Errorf("over-ceiling requests = %v, want 429", codes)
	}
	if next.hits != 2 {
		t.Errorf("handler hits = %d, want 2 (rest shed before auth)", next.hits)
	}
}

func TestPreAuthPerIPIsolatesSources(t *testing.T) {
	now := time.Now()
	// Per-IP: 60/min -> burst 60. Global disabled.
	l := newPreAuthLimiterClock(60, 0, 0, false, func() time.Time { return now })
	next := &countingHandler{}
	h := l.middleware(next)

	send := func(ip string) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/search", nil)
		req.RemoteAddr = ip + ":1000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// Exhaust IP a's 60-token burst.
	for i := 0; i < 60; i++ {
		if code := send("10.0.0.1"); code != http.StatusOK {
			t.Fatalf("IP a request %d throttled early: %d", i, code)
		}
	}
	if send("10.0.0.1") != http.StatusTooManyRequests {
		t.Error("IP a not throttled after exhausting its burst")
	}
	// A DIFFERENT IP has its own budget (isolation).
	if send("10.0.0.2") != http.StatusOK {
		t.Error("IP b throttled by IP a's usage (per-IP isolation broken)")
	}
}

func TestPreAuthDisabledIsPassThrough(t *testing.T) {
	l := newPreAuthLimiter(0, 0, 0, false)
	next := &countingHandler{}
	h := l.middleware(next)
	for i := 0; i < 1000; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/search", nil)
		req.RemoteAddr = "10.0.0.1:1"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("disabled limiter throttled request %d: %d", i, rec.Code)
		}
	}
}

func TestPreAuthTrustProxyUsesXFF(t *testing.T) {
	now := time.Now()
	l := newPreAuthLimiterClock(1, 0, 0, true, func() time.Time { return now })
	next := &countingHandler{}
	h := l.middleware(next)
	send := func(xff string) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/search", nil)
		req.RemoteAddr = "10.0.0.99:1" // shared proxy peer
		req.Header.Set("X-Forwarded-For", xff)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	// Same XFF client exhausts its 1-token budget...
	if send("1.2.3.4") != http.StatusOK {
		t.Fatal("first XFF request throttled")
	}
	if send("1.2.3.4") != http.StatusTooManyRequests {
		t.Error("second XFF request from same client not throttled")
	}
	// ...a different XFF client (left-most hop) is independent.
	if send("5.6.7.8, 1.2.3.4") != http.StatusOK {
		t.Error("different XFF client throttled by another's usage")
	}
}
