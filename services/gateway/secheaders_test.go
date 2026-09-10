package main

import (
	"net/http"
	"strings"
	"testing"
)

func wantSecurityHeaders(t *testing.T, h http.Header, context string) {
	t.Helper()
	for k, want := range securityHeaders {
		if got := h.Get(k); got != want {
			t.Errorf("%s: %s = %q, want %q", context, k, got, want)
		}
	}
}

// The headers must be on EVERY response, not just successful authenticated
// ones: the 401s, 404s and preflights are exactly what a handler-level
// implementation forgets. Applying the middleware outermost is what buys this.
func TestSecurityHeadersOnAllResponses(t *testing.T) {
	env := newTestEnv(t)

	t.Run("unauthenticated 401", func(t *testing.T) {
		rec := env.do(http.MethodGet, "/v1/search?q=x", nil, http.Header{
			"Authorization": []string{""},
		})
		if rec.Code == http.StatusOK {
			t.Fatalf("expected a non-200 for a missing token, got %d", rec.Code)
		}
		wantSecurityHeaders(t, rec.Header(), "401")
	})

	t.Run("unknown route 404", func(t *testing.T) {
		rec := env.do(http.MethodGet, "/no/such/route", nil, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		wantSecurityHeaders(t, rec.Header(), "404")
	})

	t.Run("unauthenticated health probe", func(t *testing.T) {
		rec := env.do(http.MethodGet, "/healthz", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		wantSecurityHeaders(t, rec.Header(), "healthz")
	})
}

// Clickjacking is the concrete risk: the UI drives OAuth grants and
// DELETE /v1/me/data, so a framed click has real consequence. Assert the
// specific values rather than mere presence.
func TestSecurityHeadersDenyFramingAndSniffing(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(http.MethodGet, "/healthz", nil, nil)

	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q missing %q", csp, want)
		}
	}
}

// HSTS would pin localhost to HTTPS in a developer's browser and break the
// plain-HTTP dev stack; it belongs at the production TLS ingress instead.
func TestNoHSTSFromGateway(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(http.MethodGet, "/healthz", nil, nil)
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("Strict-Transport-Security = %q, want unset at the gateway", got)
	}
}
