package main

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/asker/asker/platform/tenancy"
)

// capturedUpload is what the fake hub recorded from one proxied upload.
type capturedUpload struct {
	mu       sync.Mutex
	tenant   string
	filename string
	file     []byte
	title    string
}

// newFakeHub returns an httptest hub that parses the multipart body like the
// real /upload endpoint would and answers 202 {"doc_id":"d-123"}.
func newFakeHub(t *testing.T) (*httptest.Server, *capturedUpload) {
	t.Helper()
	got := &capturedUpload{}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.mu.Lock()
		defer got.mu.Unlock()
		got.tenant = r.Header.Get("x-asker-tenant")
		if r.URL.Path != "/upload" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer func() { _ = f.Close() }()
		got.filename = hdr.Filename
		got.file, _ = io.ReadAll(f)
		got.title = r.FormValue("title")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"doc_id":"d-123"}`))
	}))
	t.Cleanup(hub.Close)
	return hub, got
}

func multipartBody(t *testing.T, fileContent, title string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "notes.txt")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write([]byte(fileContent)); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if title != "" {
		if err := mw.WriteField("title", title); err != nil {
			t.Fatalf("write title: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

func TestUploadProxyHappyPath(t *testing.T) {
	hub, got := newFakeHub(t)
	env := newTestEnv(t, func(_ *gatewayConfig, d *deps) {
		d.hubURL = hub.URL
	})

	body, contentType := multipartBody(t, "hello upload", "Notes")
	rec := env.do(http.MethodPost, "/v1/upload", bytes.NewReader(body.Bytes()),
		http.Header{"Content-Type": []string{contentType}})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if resp := decodeObject(t, rec); resp["doc_id"] != "d-123" {
		t.Errorf("body = %v, want doc_id d-123 passed through", resp)
	}

	got.mu.Lock()
	defer got.mu.Unlock()
	// The hub must see the verified JWT tenant on the internal hop, and it
	// must re-validate cleanly (the trust contract for internal HTTP hops).
	if got.tenant != testSubject {
		t.Errorf("x-asker-tenant at hub = %q, want %q", got.tenant, testSubject)
	}
	if _, err := tenancy.FromHeaderValue(got.tenant); err != nil {
		t.Errorf("hub tenant header fails FromHeaderValue: %v", err)
	}
	// Multipart fidelity: same file bytes, filename and title.
	if string(got.file) != "hello upload" {
		t.Errorf("file at hub = %q, want %q", got.file, "hello upload")
	}
	if got.filename != "notes.txt" {
		t.Errorf("filename at hub = %q, want notes.txt", got.filename)
	}
	if got.title != "Notes" {
		t.Errorf("title at hub = %q, want Notes", got.title)
	}
}

func TestUploadRejectsNonMultipart(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(http.MethodPost, "/v1/upload", strings.NewReader(`{"file":"x"}`),
		http.Header{"Content-Type": []string{"application/json"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestUploadOversizeContentLength(t *testing.T) {
	hub, got := newFakeHub(t)
	env := newTestEnv(t, func(_ *gatewayConfig, d *deps) {
		d.hubURL = hub.URL
		d.maxUploadBytes = 64
	})
	body, contentType := multipartBody(t, strings.Repeat("x", 4096), "")
	rec := env.do(http.MethodPost, "/v1/upload", bytes.NewReader(body.Bytes()),
		http.Header{"Content-Type": []string{contentType}})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body: %s)", rec.Code, rec.Body.String())
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	if got.tenant != "" {
		t.Error("oversize upload reached the hub")
	}
}

// TestUploadOversizeChunked covers the streaming cap: no Content-Length, so
// http.MaxBytesReader must trip mid-proxy.
func TestUploadOversizeChunked(t *testing.T) {
	hub, _ := newFakeHub(t)
	env := newTestEnv(t, func(_ *gatewayConfig, d *deps) {
		d.hubURL = hub.URL
		d.maxUploadBytes = 64
	})
	body, contentType := multipartBody(t, strings.Repeat("x", 4096), "")
	// Hide the concrete reader type so httptest.NewRequest sets ContentLength=-1.
	rec := env.do(http.MethodPost, "/v1/upload", struct{ io.Reader }{body},
		http.Header{"Content-Type": []string{contentType}})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestUploadHubDown(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // port now refused
	env := newTestEnv(t, func(_ *gatewayConfig, d *deps) {
		d.hubURL = deadURL
	})
	body, contentType := multipartBody(t, "x", "")
	rec := env.do(http.MethodPost, "/v1/upload", bytes.NewReader(body.Bytes()),
		http.Header{"Content-Type": []string{contentType}})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestUploadPassesThroughHubErrors: the hub's status and JSON body are the
// caller's response, verbatim.
func TestUploadPassesThroughHubErrors(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"unsupported file type"}`))
	}))
	t.Cleanup(hub.Close)
	env := newTestEnv(t, func(_ *gatewayConfig, d *deps) {
		d.hubURL = hub.URL
	})
	body, contentType := multipartBody(t, "x", "")
	rec := env.do(http.MethodPost, "/v1/upload", bytes.NewReader(body.Bytes()),
		http.Header{"Content-Type": []string{contentType}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if decodeObject(t, rec)["error"] != "unsupported file type" {
		t.Errorf("body = %s, want hub error passed through", rec.Body.String())
	}
}
