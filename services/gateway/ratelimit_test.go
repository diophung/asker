package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asker/asker/platform/tenancy"
)

func tenantRequest(t *testing.T, tenant string) *http.Request {
	t.Helper()
	tc, err := tenancy.FromHeaderValue(tenant)
	if err != nil {
		t.Fatalf("FromHeaderValue(%q): %v", tenant, err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	return req.WithContext(tenancy.WithContext(req.Context(), tc))
}

func okHandler() (http.Handler, *int) {
	calls := 0
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}), &calls
}

func TestRateLimiterUnderAndOverLimit(t *testing.T) {
	counter := &fakeCounter{}
	rl := newRateLimiter(counter, 2, discardLogger())
	fixed := time.Date(2026, 6, 12, 10, 0, 42, 0, time.UTC) // 42s into the window
	rl.now = func() time.Time { return fixed }
	next, calls := okHandler()
	h := rl.middleware(next)

	for i := 1; i <= 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, tenantRequest(t, "tenant-a"))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, tenantRequest(t, "tenant-a"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("request 3: status = %d, want 429 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.Contains(got, `{"error":"rate limited"}`) {
		t.Errorf("429 body = %q", got)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "18" { // 60 - 42
		t.Errorf("Retry-After = %q, want 18", ra)
	}
	if *calls != 2 {
		t.Errorf("handler calls = %d, want 2", *calls)
	}

	// Another tenant has its own bucket.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, tenantRequest(t, "tenant-b"))
	if rec.Code != http.StatusOK {
		t.Errorf("tenant-b status = %d, want 200 (separate bucket)", rec.Code)
	}

	// Key format and TTL are contractual: rl:<tenant>:<unix-minute>, 90s.
	wantKey := fmt.Sprintf("rl:tenant-a:%d", fixed.Unix()/60)
	counter.mu.Lock()
	defer counter.mu.Unlock()
	if counter.counts[wantKey] != 3 {
		t.Errorf("counts[%s] = %d, want 3 (keys: %v)", wantKey, counter.counts[wantKey], counter.counts)
	}
	if counter.ttls[wantKey] != 90*time.Second {
		t.Errorf("ttl = %v, want 90s", counter.ttls[wantKey])
	}
}

func TestRateLimiterFailsOpenWithThrottledWarn(t *testing.T) {
	counter := &fakeCounter{err: errors.New("connection refused")}
	var buf syncBuffer
	rl := newRateLimiter(counter, 1, slog.New(slog.NewTextHandler(&buf, nil)))
	now := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)
	rl.now = func() time.Time { return now }
	next, calls := okHandler()
	h := rl.middleware(next)

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, tenantRequest(t, "tenant-a"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (fail open)", rec.Code)
		}
	}
	if *calls != 3 {
		t.Errorf("handler calls = %d, want 3 (limit must not apply)", *calls)
	}
	if got := strings.Count(buf.String(), "failing OPEN"); got != 1 {
		t.Errorf("warn count = %d, want exactly 1 within a minute\nlogs: %s", got, buf.String())
	}

	now = now.Add(61 * time.Second)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, tenantRequest(t, "tenant-a"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.Count(buf.String(), "failing OPEN"); got != 2 {
		t.Errorf("warn count after a minute = %d, want 2", got)
	}
}

func TestRateLimiterFailsClosedWithoutTenant(t *testing.T) {
	rl := newRateLimiter(&fakeCounter{}, 1, discardLogger())
	next, calls := okHandler()
	h := rl.middleware(next)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/me", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if *calls != 0 {
		t.Error("handler ran without a tenant")
	}
}

func TestRateLimiterDisabled(t *testing.T) {
	counter := &fakeCounter{}
	rl := newRateLimiter(counter, 0, discardLogger())
	next, _ := okHandler()
	h := rl.middleware(next)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, tenantRequest(t, "tenant-a"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	counter.mu.Lock()
	defer counter.mu.Unlock()
	if counter.calls != 0 {
		t.Errorf("counter calls = %d, want 0 when disabled", counter.calls)
	}
}

// TestRateLimitIntegration drives the full handler chain: auth first, then
// the per-tenant limit from the verified token's tenant.
func TestRateLimitIntegration(t *testing.T) {
	env := newTestEnv(t, func(cfg *gatewayConfig, _ *deps) {
		cfg.RateLimitPerMinute = 1
	})

	rec := env.do(http.MethodGet, "/v1/me", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rec.Code)
	}
	rec = env.do(http.MethodGet, "/v1/me", nil, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After header")
	}

	// A different tenant is not throttled by tenant A's bucket.
	rec = env.do(http.MethodGet, "/v1/me", nil,
		http.Header{"Authorization": []string{env.bearerFor("other-tenant")}})
	if rec.Code != http.StatusOK {
		t.Errorf("other tenant status = %d, want 200", rec.Code)
	}

	// Unauthenticated requests never reach the counter.
	before := env.counter.callCount()
	req, rr := newRawRequest(http.MethodGet, "/v1/me")
	env.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", rr.Code)
	}
	if env.counter.callCount() != before {
		t.Error("rate counter consulted before auth")
	}
}

// syncBuffer is a goroutine-safe bytes.Buffer for log capture.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
