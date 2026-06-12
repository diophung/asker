package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCORSPreflight(t *testing.T) {
	env := newTestEnv(t, func(cfg *gatewayConfig, _ *deps) {
		cfg.CORSAllowedOrigins = "http://localhost:3000,https://app.example.com"
	})

	t.Run("allowed origin", func(t *testing.T) {
		for _, origin := range []string{"http://localhost:3000", "https://app.example.com"} {
			req := httptest.NewRequest(http.MethodOptions, "/v1/search", nil)
			req.Header.Set("Origin", origin)
			req.Header.Set("Access-Control-Request-Method", http.MethodGet)
			req.Header.Set("Access-Control-Request-Headers", "authorization")
			rec := httptest.NewRecorder()
			env.handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusNoContent {
				t.Fatalf("preflight status = %d, want 204", rec.Code)
			}
			h := rec.Header()
			if got := h.Get("Access-Control-Allow-Origin"); got != origin {
				t.Errorf("Allow-Origin = %q, want %q (never *)", got, origin)
			}
			if got := h.Get("Access-Control-Allow-Methods"); got != "GET,POST,PUT,DELETE,OPTIONS" {
				t.Errorf("Allow-Methods = %q", got)
			}
			if got := h.Get("Access-Control-Allow-Headers"); got != "Authorization,Content-Type" {
				t.Errorf("Allow-Headers = %q", got)
			}
			if got := h.Get("Access-Control-Max-Age"); got != "600" {
				t.Errorf("Max-Age = %q, want 600", got)
			}
			if got := h.Get("Vary"); got != "Origin" {
				t.Errorf("Vary = %q, want Origin", got)
			}
		}
	})

	t.Run("denied origin gets no CORS headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/v1/search", nil)
		req.Header.Set("Origin", "http://evil.example.com")
		req.Header.Set("Access-Control-Request-Method", http.MethodGet)
		rec := httptest.NewRecorder()
		env.handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("preflight status = %d, want 204", rec.Code)
		}
		for _, h := range []string{
			"Access-Control-Allow-Origin",
			"Access-Control-Allow-Methods",
			"Access-Control-Allow-Headers",
			"Access-Control-Max-Age",
		} {
			if got := rec.Header().Get(h); got != "" {
				t.Errorf("%s = %q, want unset for denied origin", h, got)
			}
		}
		if got := rec.Header().Get("Vary"); got != "Origin" {
			t.Errorf("Vary = %q, want Origin even when denied", got)
		}
	})
}

func TestCORSActualRequests(t *testing.T) {
	env := newTestEnv(t)

	t.Run("allowed origin echoed on real request", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set("Origin", "http://localhost:3000")
		rec := httptest.NewRecorder()
		env.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
			t.Errorf("Allow-Origin = %q, want the exact origin", got)
		}
	})

	t.Run("denied origin still served, no CORS header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set("Origin", "http://evil.example.com")
		rec := httptest.NewRecorder()
		env.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (the server-side response is origin-agnostic)", rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Allow-Origin = %q, want unset", got)
		}
	})

	t.Run("no origin header, no CORS header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rec := httptest.NewRecorder()
		env.handler.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Allow-Origin = %q, want unset", got)
		}
		if got := rec.Header().Get("Vary"); got != "Origin" {
			t.Errorf("Vary = %q, want Origin", got)
		}
	})
}

// TestCORSWildcardNeverEchoed: a misconfigured "*" entry must not enable
// any origin — exact matches only, and "*" itself is never emitted.
func TestCORSWildcardNeverEchoed(t *testing.T) {
	env := newTestEnv(t, func(cfg *gatewayConfig, _ *deps) {
		cfg.CORSAllowedOrigins = "*"
	})
	req := httptest.NewRequest(http.MethodOptions, "/v1/search", nil)
	req.Header.Set("Origin", "http://anything.example.com")
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q, want unset (wildcard is forbidden)", got)
	}
}

// TestPlainOptionsIsNotPreflight: OPTIONS without Access-Control-Request-
// Method falls through to normal routing (405 from the method dispatcher).
func TestPlainOptionsIsNotPreflight(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(http.MethodOptions, "/v1/connectors", nil, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
