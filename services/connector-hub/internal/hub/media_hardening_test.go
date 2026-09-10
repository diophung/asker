package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// PUT /internal/media takes its content_type from a query parameter and stores
// it on the blob, and that stored value becomes the response Content-Type of
// the subsequent GET (and of the gateway's /v1/media proxy). The upload
// connector sanitized its own input, but this second write path did not — so
// the invariant has to hold at the storage boundary, not just at one caller.
func TestMediaPutSanitizesStoredContentType(t *testing.T) {
	store := newFakeMediaStore()
	api := mediaAPIRig(t, store)

	body := strings.NewReader(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	req := httptest.NewRequest(http.MethodPut,
		"/internal/media?key=thumb.svg&content_type=image/svg%2Bxml", body)
	req.Header.Set(tenantHeader, tenantA)
	rec := doRequest(api, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d (body %s), want 201", rec.Code, rec.Body)
	}
	if got := rec.Body.String(); strings.Contains(got, "image/svg+xml") {
		t.Errorf("returned BlobRef still carries the renderable type: %s", got)
	}
	if !strings.Contains(rec.Body.String(), "application/octet-stream") {
		t.Errorf("BlobRef content_type = %s, want application/octet-stream", rec.Body.String())
	}
}

// The baseline headers must be on EVERY hub response. handleMediaGet's own
// setMediaSecurityHeaders call happens after several early returns (400/401/
// 404/503), so without an outermost middleware those error paths ship bare.
func TestHubSecurityHeadersOnErrorPaths(t *testing.T) {
	store := newFakeMediaStore()
	api := mediaAPIRig(t, store)

	cases := map[string]*http.Request{
		"400 missing key":   httptest.NewRequest(http.MethodGet, "/internal/media", nil),
		"404 unknown route": httptest.NewRequest(http.MethodGet, "/no/such/route", nil),
	}
	for name, req := range cases {
		req.Header.Set(tenantHeader, tenantA)
		rec := doRequest(api, req)
		if rec.Code == http.StatusOK {
			t.Fatalf("%s: expected a non-200, got 200", name)
		}
		for header, want := range map[string]string{
			"X-Content-Type-Options":  "nosniff",
			"X-Frame-Options":         "DENY",
			"Referrer-Policy":         "no-referrer",
			"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",
		} {
			if got := rec.Header().Get(header); got != want {
				t.Errorf("%s: %s = %q, want %q", name, header, got, want)
			}
		}
	}
}

// A 401 has no tenant header at all — still must carry the baseline.
func TestHubSecurityHeadersOnUnauthenticated(t *testing.T) {
	api := mediaAPIRig(t, newFakeMediaStore())
	rec := doRequest(api, httptest.NewRequest(http.MethodGet, "/internal/media?key=x", nil))
	if rec.Code == http.StatusOK {
		t.Fatalf("expected a non-200 without a tenant header, got 200")
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}
