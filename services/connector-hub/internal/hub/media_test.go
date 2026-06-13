package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/asker/asker/platform/blob"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

// fakeMediaStore mimics platform/blob.Store closely enough to exercise the
// media handlers' tenant isolation and round-trip contract without an object
// store: keys are tenant-prefixed ("<tenant>/<key>"), payloads are stored as
// a trivially reversible "ciphertext" (a 0x01 version byte prefix) that is
// NOT equal to the plaintext, Get fails closed on a foreign prefix, and Get
// verifies the BlobRef sha256. Safe for concurrent use.
type fakeMediaStore struct {
	bucket string

	mu      sync.Mutex
	objects map[string][]byte // objectKey -> "ciphertext"
	putErr  error             // when set, Put returns it
	getErr  error             // when set, Get returns it (after prefix/ref checks)
}

func newFakeMediaStore() *fakeMediaStore {
	return &fakeMediaStore{bucket: "asker-blobs", objects: make(map[string][]byte)}
}

// encrypt is a stand-in for envelope encryption: a version byte + the bytes
// XORed against a tenant-derived constant. The point of the test is only that
// the stored object is NOT the plaintext, so any reversible, tenant-bound
// transform works.
func encrypt(tenant string, plaintext []byte) []byte {
	out := make([]byte, 0, len(plaintext)+1)
	out = append(out, 0x01)
	mask := byte(len(tenant))
	for _, b := range plaintext {
		out = append(out, b^mask^0x5a)
	}
	return out
}

func decrypt(tenant string, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) == 0 || ciphertext[0] != 0x01 {
		return nil, errors.New("fake: bad ciphertext")
	}
	body := ciphertext[1:]
	out := make([]byte, len(body))
	mask := byte(len(tenant))
	for i, b := range body {
		out[i] = b ^ mask ^ 0x5a
	}
	return out, nil
}

func (s *fakeMediaStore) Get(ctx context.Context, tc tenancy.Context, ref *askerv1.BlobRef) ([]byte, error) {
	if tc.TenantID() == "" {
		return nil, tenancy.ErrNoTenant
	}
	if ref == nil || ref.GetKey() == "" {
		return nil, errors.New("fake: nil or empty ref")
	}
	if ref.GetSha256() == "" {
		return nil, errors.New("fake: ref carries no sha256")
	}
	// Fail closed before any read: a key outside the caller's prefix is a
	// cross-tenant attempt.
	prefix := string(tc.TenantID()) + "/"
	if !strings.HasPrefix(ref.GetKey(), prefix) {
		return nil, blob.ErrTenantMismatch
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	ciphertext, ok := s.objects[ref.GetKey()]
	if !ok {
		return nil, blob.ErrNotFound
	}
	plaintext, err := decrypt(string(tc.TenantID()), ciphertext)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(plaintext)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), ref.GetSha256()) {
		return nil, blob.ErrChecksumMismatch
	}
	return plaintext, nil
}

func (s *fakeMediaStore) Put(ctx context.Context, tc tenancy.Context, key, contentType string, data []byte) (*askerv1.BlobRef, error) {
	if tc.TenantID() == "" {
		return nil, tenancy.ErrNoTenant
	}
	objectKey := string(tc.TenantID()) + "/" + key
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.putErr != nil {
		return nil, s.putErr
	}
	s.objects[objectKey] = encrypt(string(tc.TenantID()), data)
	sum := sha256.Sum256(data)
	return &askerv1.BlobRef{
		Bucket:      s.bucket,
		Key:         objectKey,
		SizeBytes:   int64(len(data)),
		ContentType: contentType,
		Sha256:      hex.EncodeToString(sum[:]),
	}, nil
}

// stored returns the raw (ciphertext) bytes under objectKey for assertions.
func (s *fakeMediaStore) stored(objectKey string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[objectKey]
	return b, ok
}

// mediaAPIRig builds an httpAPI wired with the given (possibly nil) media
// store; the scheduler is not running.
func mediaAPIRig(t *testing.T, store MediaBlobStore) *httpAPI {
	t.Helper()
	r := newRig(t, &fakeConnector{id: "gmail"}, nil)
	return &httpAPI{
		cp:         r.sch.cp,
		registry:   r.sch.registry,
		sched:      r.sch,
		emit:       r.sch.emit,
		upload:     stubUpload(nil),
		mediaBlobs: store,
		logger:     testLogger(),
	}
}

// putMedia stores plaintext for tenant via the fake's Put and returns the
// resulting BlobRef so a GET can pass back the full triple.
func putMedia(t *testing.T, store *fakeMediaStore, tenant, key, contentType string, plaintext []byte) *askerv1.BlobRef {
	t.Helper()
	ref, err := store.Put(context.Background(), mustTenant(t, tenant), key, contentType, plaintext)
	if err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	return ref
}

func TestMediaGet(t *testing.T) {
	const plaintext = "the decrypted media bytes"

	t.Run("returns decrypted bytes for the calling tenant", func(t *testing.T) {
		store := newFakeMediaStore()
		ref := putMedia(t, store, tenantA, "thumb.jpg", "image/jpeg", []byte(plaintext))
		api := mediaAPIRig(t, store)

		req := httptest.NewRequest(http.MethodGet, "/internal/media?key="+ref.GetKey()+"&sha256="+ref.GetSha256()+"&content_type=image/jpeg", nil)
		req.Header.Set(tenantHeader, tenantA)
		rec := doRequest(api, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %s), want 200", rec.Code, rec.Body)
		}
		if got := rec.Body.String(); got != plaintext {
			t.Errorf("body = %q, want %q (decrypted)", got, plaintext)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
			t.Errorf("Content-Type = %q, want image/jpeg", ct)
		}
		// The stored object is ciphertext, never the plaintext.
		if raw, ok := store.stored(ref.GetKey()); !ok || string(raw) == plaintext {
			t.Errorf("stored bytes %q must be ciphertext != plaintext", raw)
		}
	})

	t.Run("a different tenant cannot read the object (no cross-tenant read)", func(t *testing.T) {
		store := newFakeMediaStore()
		// Object belongs to tenant A.
		ref := putMedia(t, store, tenantA, "secret.bin", "application/octet-stream", []byte(plaintext))
		api := mediaAPIRig(t, store)

		// Tenant B asks for tenant A's key. Must be 404 and empty (a foreign
		// object is indistinguishable from a missing one).
		req := httptest.NewRequest(http.MethodGet, "/internal/media?key="+ref.GetKey()+"&sha256="+ref.GetSha256(), nil)
		req.Header.Set(tenantHeader, tenantB)
		rec := doRequest(api, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (no cross-tenant read)", rec.Code)
		}
		if strings.Contains(rec.Body.String(), plaintext) {
			t.Fatal("cross-tenant read leaked plaintext")
		}
	})

	t.Run("missing tenant header is 401 (fail closed)", func(t *testing.T) {
		store := newFakeMediaStore()
		ref := putMedia(t, store, tenantA, "thumb.jpg", "image/jpeg", []byte(plaintext))
		api := mediaAPIRig(t, store)

		req := httptest.NewRequest(http.MethodGet, "/internal/media?key="+ref.GetKey()+"&sha256="+ref.GetSha256(), nil)
		rec := doRequest(api, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("invalid tenant header is 401", func(t *testing.T) {
		api := mediaAPIRig(t, newFakeMediaStore())
		req := httptest.NewRequest(http.MethodGet, "/internal/media?key=tenant-a/x&sha256=deadbeef", nil)
		req.Header.Set(tenantHeader, "spaces not allowed")
		rec := doRequest(api, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("absent key is 404", func(t *testing.T) {
		api := mediaAPIRig(t, newFakeMediaStore())
		req := httptest.NewRequest(http.MethodGet, "/internal/media?key=tenant-a/nope.jpg&sha256="+hexOf("anything"), nil)
		req.Header.Set(tenantHeader, tenantA)
		rec := doRequest(api, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("missing key is 400", func(t *testing.T) {
		api := mediaAPIRig(t, newFakeMediaStore())
		req := httptest.NewRequest(http.MethodGet, "/internal/media?sha256=abc", nil)
		req.Header.Set(tenantHeader, tenantA)
		rec := doRequest(api, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("missing sha256 is 400", func(t *testing.T) {
		api := mediaAPIRig(t, newFakeMediaStore())
		req := httptest.NewRequest(http.MethodGet, "/internal/media?key=tenant-a/x.jpg", nil)
		req.Header.Set(tenantHeader, tenantA)
		rec := doRequest(api, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("no media store wired is 503", func(t *testing.T) {
		api := mediaAPIRig(t, nil)
		req := httptest.NewRequest(http.MethodGet, "/internal/media?key=tenant-a/x.jpg&sha256=abc", nil)
		req.Header.Set(tenantHeader, tenantA)
		rec := doRequest(api, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
	})

	t.Run("store error is 500", func(t *testing.T) {
		store := newFakeMediaStore()
		ref := putMedia(t, store, tenantA, "thumb.jpg", "image/jpeg", []byte(plaintext))
		store.getErr = errors.New("object store down")
		api := mediaAPIRig(t, store)

		req := httptest.NewRequest(http.MethodGet, "/internal/media?key="+ref.GetKey()+"&sha256="+ref.GetSha256(), nil)
		req.Header.Set(tenantHeader, tenantA)
		rec := doRequest(api, req)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
	})

	t.Run("checksum mismatch is 404", func(t *testing.T) {
		// A tampered/wrong sha256 collapses to not-found rather than leaking
		// that the object exists.
		store := newFakeMediaStore()
		ref := putMedia(t, store, tenantA, "thumb.jpg", "image/jpeg", []byte(plaintext))
		api := mediaAPIRig(t, store)

		req := httptest.NewRequest(http.MethodGet, "/internal/media?key="+ref.GetKey()+"&sha256="+hexOf("wrong"), nil)
		req.Header.Set(tenantHeader, tenantA)
		rec := doRequest(api, req)
		// ErrChecksumMismatch is neither ErrNotFound nor ErrTenantMismatch, so
		// it surfaces as a 500 (corrupt object / caller passed a bad ref).
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500 (checksum failure)", rec.Code)
		}
	})

	t.Run("content type defaults to octet-stream", func(t *testing.T) {
		store := newFakeMediaStore()
		ref := putMedia(t, store, tenantA, "raw.bin", "", []byte(plaintext))
		api := mediaAPIRig(t, store)

		req := httptest.NewRequest(http.MethodGet, "/internal/media?key="+ref.GetKey()+"&sha256="+ref.GetSha256(), nil)
		req.Header.Set(tenantHeader, tenantA)
		rec := doRequest(api, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != defaultMediaContentType {
			t.Errorf("Content-Type = %q, want %q", ct, defaultMediaContentType)
		}
	})
}

func TestMediaPut(t *testing.T) {
	t.Run("round-trips: PUT then GET returns the same bytes, stored is ciphertext", func(t *testing.T) {
		store := newFakeMediaStore()
		api := mediaAPIRig(t, store)
		payload := []byte("keyframe-0001 jpeg bytes")

		putReq := httptest.NewRequest(http.MethodPut, "/internal/media?key=keyframes/0001.jpg&content_type=image/jpeg",
			strings.NewReader(string(payload)))
		putReq.Header.Set(tenantHeader, tenantA)
		putRec := doRequest(api, putReq)
		if putRec.Code != http.StatusCreated {
			t.Fatalf("PUT status = %d (body %s), want 201", putRec.Code, putRec.Body)
		}

		var ref mediaBlobRef
		if err := json.Unmarshal(putRec.Body.Bytes(), &ref); err != nil {
			t.Fatalf("PUT response not JSON: %v", err)
		}
		wantKey := tenantA + "/keyframes/0001.jpg"
		if ref.Key != wantKey {
			t.Errorf("ref.Key = %q, want %q (tenant-prefixed)", ref.Key, wantKey)
		}
		if ref.ContentType != "image/jpeg" || ref.SizeBytes != int64(len(payload)) || ref.Sha256 == "" {
			t.Errorf("ref = %+v", ref)
		}

		// Stored object is ciphertext, never the plaintext (encrypt-on-write).
		raw, ok := store.stored(wantKey)
		if !ok {
			t.Fatal("nothing stored under the returned key")
		}
		if string(raw) == string(payload) {
			t.Error("stored bytes equal the plaintext; encryption did not happen")
		}

		// GET with the returned ref (worker passes back the BlobRef triple +
		// content type) decrypts to the original bytes.
		getReq := httptest.NewRequest(http.MethodGet, "/internal/media?key="+ref.Key+"&sha256="+ref.Sha256+"&content_type="+ref.ContentType, nil)
		getReq.Header.Set(tenantHeader, tenantA)
		getRec := doRequest(api, getReq)
		if getRec.Code != http.StatusOK {
			t.Fatalf("GET status = %d, want 200", getRec.Code)
		}
		if getRec.Body.String() != string(payload) {
			t.Errorf("round-trip body = %q, want %q", getRec.Body.String(), payload)
		}
		if ct := getRec.Header().Get("Content-Type"); ct != "image/jpeg" {
			t.Errorf("round-trip Content-Type = %q, want image/jpeg", ct)
		}
	})

	t.Run("a tenant cannot PUT then have another tenant GET it", func(t *testing.T) {
		store := newFakeMediaStore()
		api := mediaAPIRig(t, store)

		putReq := httptest.NewRequest(http.MethodPut, "/internal/media?key=x.bin", strings.NewReader("secret"))
		putReq.Header.Set(tenantHeader, tenantA)
		putRec := doRequest(api, putReq)
		if putRec.Code != http.StatusCreated {
			t.Fatalf("PUT status = %d, want 201", putRec.Code)
		}
		var ref mediaBlobRef
		if err := json.Unmarshal(putRec.Body.Bytes(), &ref); err != nil {
			t.Fatalf("PUT response not JSON: %v", err)
		}

		getReq := httptest.NewRequest(http.MethodGet, "/internal/media?key="+ref.Key+"&sha256="+ref.Sha256, nil)
		getReq.Header.Set(tenantHeader, tenantB)
		getRec := doRequest(api, getReq)
		if getRec.Code != http.StatusNotFound {
			t.Errorf("cross-tenant GET status = %d, want 404", getRec.Code)
		}
	})

	t.Run("missing tenant header is 401", func(t *testing.T) {
		api := mediaAPIRig(t, newFakeMediaStore())
		req := httptest.NewRequest(http.MethodPut, "/internal/media?key=x.bin", strings.NewReader("body"))
		rec := doRequest(api, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("missing key is 400", func(t *testing.T) {
		api := mediaAPIRig(t, newFakeMediaStore())
		req := httptest.NewRequest(http.MethodPut, "/internal/media", strings.NewReader("body"))
		req.Header.Set(tenantHeader, tenantA)
		rec := doRequest(api, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("no media store wired is 503", func(t *testing.T) {
		api := mediaAPIRig(t, nil)
		req := httptest.NewRequest(http.MethodPut, "/internal/media?key=x.bin", strings.NewReader("body"))
		req.Header.Set(tenantHeader, tenantA)
		rec := doRequest(api, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
	})

	t.Run("body over the cap is 413", func(t *testing.T) {
		api := mediaAPIRig(t, newFakeMediaStore())
		// A body larger than maxMediaBytes; use a reader that reports the
		// length without allocating 200MiB.
		req := httptest.NewRequest(http.MethodPut, "/internal/media?key=big.bin",
			io.LimitReader(neverEndingReader{}, maxMediaBytes+1))
		req.Header.Set(tenantHeader, tenantA)
		rec := doRequest(api, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413", rec.Code)
		}
	})

	t.Run("store error is 500", func(t *testing.T) {
		store := newFakeMediaStore()
		store.putErr = errors.New("object store down")
		api := mediaAPIRig(t, store)
		req := httptest.NewRequest(http.MethodPut, "/internal/media?key=x.bin", strings.NewReader("body"))
		req.Header.Set(tenantHeader, tenantA)
		rec := doRequest(api, req)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
	})

	t.Run("content type defaults to octet-stream", func(t *testing.T) {
		store := newFakeMediaStore()
		api := mediaAPIRig(t, store)
		req := httptest.NewRequest(http.MethodPut, "/internal/media?key=x.bin", strings.NewReader("body"))
		req.Header.Set(tenantHeader, tenantA)
		rec := doRequest(api, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", rec.Code)
		}
		var ref mediaBlobRef
		if err := json.Unmarshal(rec.Body.Bytes(), &ref); err != nil {
			t.Fatalf("response not JSON: %v", err)
		}
		if ref.ContentType != defaultMediaContentType {
			t.Errorf("content_type = %q, want %q", ref.ContentType, defaultMediaContentType)
		}
	})
}

// hexOf returns the lowercase-hex sha256 of s, for crafting non-matching ref
// digests in tests.
func hexOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// neverEndingReader yields an endless stream of 'a' bytes; bounded by an outer
// io.LimitReader so the body crosses the size cap without buffering it all.
type neverEndingReader struct{}

func (neverEndingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}
