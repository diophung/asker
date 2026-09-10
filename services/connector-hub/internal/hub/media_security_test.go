package hub

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The stored Content-Type originates from an upload's multipart part header
// (attacker controlled) and becomes this response's Content-Type. These
// headers are what stop those bytes from being rendered as an active document
// in the serving origin. The gateway proxy sets them too; both layers are
// asserted so neither can silently regress.
func TestMediaGetSetsNonRenderableHeaders(t *testing.T) {
	store := newFakeMediaStore()
	payload := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	ref := putMedia(t, store, tenantA, "payload.svg", "image/svg+xml", payload)
	api := mediaAPIRig(t, store)

	req := httptest.NewRequest(http.MethodGet, "/internal/media?key="+ref.GetKey()+"&sha256="+ref.GetSha256(), nil)
	req.Header.Set(tenantHeader, tenantA)
	rec := doRequest(api, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s), want 200", rec.Code, rec.Body)
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
