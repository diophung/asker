package main

import (
	"net/http"
	"testing"
)

// The Content-Type served here traces back to an upload's multipart part
// header, which the client controls. Without these headers a file stored as
// text/html or image/svg+xml renders as a top-level document in the gateway's
// own origin, so any script in it runs with access to same-origin storage —
// stored XSS that yields the bearer token and the whole indexed corpus.
func TestMediaResponseIsNotRenderable(t *testing.T) {
	hub, _ := fakeMediaHub(t, "image/svg+xml",
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
	env := newTestEnv(t, func(_ *gatewayConfig, d *deps) {
		d.hubURL = hub.URL
	})

	rec := env.do(http.MethodGet, "/v1/media?key=user-123/upload/abc", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	for header, want := range map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"Content-Disposition":     "attachment",
		"Content-Security-Policy": "default-src 'none'; sandbox",
		"X-Frame-Options":         "DENY",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}
