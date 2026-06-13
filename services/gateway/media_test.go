package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/asker/asker/platform/tenancy"
)

// capturedMedia records what the fake hub saw on its internal media endpoint.
type capturedMedia struct {
	mu     sync.Mutex
	hits   int
	tenant string
	key    string
	path   string
}

// fakeMediaHub returns an httptest hub that mimics the connector-hub's
// internal media endpoint: it echoes the requested key/tenant and serves a
// canned PNG body, or 404 for the magic "missing" key. The body and
// Content-Type are configurable so a test can assert byte/MIME fidelity.
func fakeMediaHub(t *testing.T, contentType string, body []byte) (*httptest.Server, *capturedMedia) {
	t.Helper()
	got := &capturedMedia{}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.mu.Lock()
		got.hits++
		got.tenant = r.Header.Get("x-asker-tenant")
		got.key = r.URL.Query().Get("key")
		got.path = r.URL.Path
		got.mu.Unlock()

		if r.URL.Path != "/internal/media" {
			http.NotFound(w, r)
			return
		}
		if got.key == "missing" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(hub.Close)
	return hub, got
}

func TestMediaRequiresAuth(t *testing.T) {
	hub, got := fakeMediaHub(t, "image/png", []byte("png-bytes"))
	env := newTestEnv(t, func(_ *gatewayConfig, d *deps) {
		d.hubURL = hub.URL
	})

	// No Authorization header: must be rejected before the hub is touched.
	req, rec := newRawRequest(http.MethodGet, "/v1/media?key=tenant/thumb.png")
	env.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body: %s)", rec.Code, rec.Body.String())
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	if got.hits != 0 {
		t.Errorf("hub was reached without auth (%d hits)", got.hits)
	}
}

// TestMediaProxyHappyPath is THE isolation property test: the hub must see the
// verified JWT tenant on x-asker-tenant (never a client-supplied value), the
// user's key forwarded as-is, and the decrypted bytes + Content-Type stream
// back verbatim.
func TestMediaProxyHappyPath(t *testing.T) {
	wantBody := []byte("\x89PNG\r\n\x1a\nthumbnail-bytes")
	hub, got := fakeMediaHub(t, "image/png", wantBody)
	env := newTestEnv(t, func(_ *gatewayConfig, d *deps) {
		d.hubURL = hub.URL
	})

	const key = "user-123/media/thumb-abc.png"
	rec := env.do(http.MethodGet, "/v1/media?key="+key, nil,
		http.Header{
			// Attacker-controlled tenant header must be ignored: the JWT wins.
			"X-Asker-Tenant": []string{"attacker-tenant"},
		})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png passed through", ct)
	}
	if !bytes.Equal(rec.Body.Bytes(), wantBody) {
		t.Errorf("body = %q, want decrypted bytes streamed verbatim", rec.Body.Bytes())
	}

	got.mu.Lock()
	defer got.mu.Unlock()
	if got.path != "/internal/media" {
		t.Errorf("hub path = %q, want /internal/media", got.path)
	}
	// The isolation property: tenant comes from the verified JWT, NOT the
	// X-Asker-Tenant header the caller tried to smuggle.
	if got.tenant != testSubject {
		t.Errorf("x-asker-tenant at hub = %q, want JWT tenant %q (not attacker-tenant)", got.tenant, testSubject)
	}
	if _, err := tenancy.FromHeaderValue(got.tenant); err != nil {
		t.Errorf("hub tenant header fails FromHeaderValue: %v", err)
	}
	// The hub enforces the tenant prefix; the gateway forwards the key as-is.
	if got.key != key {
		t.Errorf("key at hub = %q, want %q forwarded as-is", got.key, key)
	}
}

// TestMediaTenantIsolation proves a second tenant gets ITS OWN tenant header,
// never the first one's — the structural per-tenant guarantee.
func TestMediaTenantIsolation(t *testing.T) {
	hub, got := fakeMediaHub(t, "image/png", []byte("x"))
	env := newTestEnv(t, func(_ *gatewayConfig, d *deps) {
		d.hubURL = hub.URL
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/media?key=k", nil)
	req.Header.Set("Authorization", env.bearerFor("tenant-b"))
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	if got.tenant != "tenant-b" {
		t.Errorf("x-asker-tenant at hub = %q, want tenant-b (the caller's own JWT tenant)", got.tenant)
	}
}

func TestMediaNotFoundPassthrough(t *testing.T) {
	hub, _ := fakeMediaHub(t, "image/png", []byte("unused"))
	env := newTestEnv(t, func(_ *gatewayConfig, d *deps) {
		d.hubURL = hub.URL
	})

	rec := env.do(http.MethodGet, "/v1/media?key=missing", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 passthrough (body: %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want the hub's 404 content type", ct)
	}
}

func TestMediaEmptyKey(t *testing.T) {
	hub, got := fakeMediaHub(t, "image/png", []byte("x"))
	env := newTestEnv(t, func(_ *gatewayConfig, d *deps) {
		d.hubURL = hub.URL
	})

	rec := env.do(http.MethodGet, "/v1/media", nil, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for empty key (body: %s)", rec.Code, rec.Body.String())
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	if got.hits != 0 {
		t.Error("empty key reached the hub")
	}
}

func TestMediaBlankKey(t *testing.T) {
	hub, got := fakeMediaHub(t, "image/png", []byte("x"))
	env := newTestEnv(t, func(_ *gatewayConfig, d *deps) {
		d.hubURL = hub.URL
	})

	// key= present but empty is still a bad request.
	rec := env.do(http.MethodGet, "/v1/media?key=", nil, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for blank key", rec.Code)
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	if got.hits != 0 {
		t.Error("blank key reached the hub")
	}
}

// TestMediaOversizeCap proves the size cap truncates a hub body larger than
// MAX_MEDIA_MB rather than streaming it unbounded.
func TestMediaOversizeCap(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 4096)
	hub, _ := fakeMediaHub(t, "image/png", big)
	env := newTestEnv(t, func(_ *gatewayConfig, d *deps) {
		d.hubURL = hub.URL
		d.maxMediaBytes = 64
	})

	rec := env.do(http.MethodGet, "/v1/media?key=k", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Body.Len(); int64(got) != 64 {
		t.Errorf("body length = %d, want capped at 64 bytes", got)
	}
}

func TestMediaHubDown(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // port now refused
	env := newTestEnv(t, func(_ *gatewayConfig, d *deps) {
		d.hubURL = deadURL
	})

	rec := env.do(http.MethodGet, "/v1/media?key=k", nil, nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestMediaRejectsNonGET confirms the route is GET-only (the web client only
// ever GETs a thumbnail).
func TestMediaRejectsNonGET(t *testing.T) {
	hub, _ := fakeMediaHub(t, "image/png", []byte("x"))
	env := newTestEnv(t, func(_ *gatewayConfig, d *deps) {
		d.hubURL = hub.URL
	})
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := env.do(method, "/v1/media?key=k", strings.NewReader(""), nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", method, rec.Code)
		}
	}
}
