package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/asker/asker/platform/config"
	"github.com/asker/asker/platform/crypto"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

// testTenant builds a valid tenancy.Context for id.
func testTenant(t *testing.T, id string) tenancy.Context {
	t.Helper()
	tc, err := tenancy.FromClaims(map[string]any{"tenant_id": id})
	if err != nil {
		t.Fatalf("FromClaims(%q): %v", id, err)
	}
	return tc
}

// testCipher builds a real TenantCipher over a throwaway file KEK and an
// in-memory DEK store.
func testCipher(t *testing.T) *crypto.TenantCipher {
	t.Helper()
	kek, err := crypto.NewFileKEK(filepath.Join(t.TempDir(), "kek.bin"))
	if err != nil {
		t.Fatalf("NewFileKEK: %v", err)
	}
	return crypto.NewTenantCipher(kek, crypto.NewMemDEKStore())
}

// fakeAPI is an in-memory objectAPI standing in for MinIO.
type fakeAPI struct {
	mu           sync.Mutex
	objects      map[string][]byte // "<bucket>/<key>" -> stored bytes
	contentTypes map[string]string // "<bucket>/<key>" -> stored Content-Type
	ensureCalls  []string
	getCalls     int

	ensureErr error
	putErr    error
	getErr    error
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{objects: make(map[string][]byte), contentTypes: make(map[string]string)}
}

func (f *fakeAPI) ensureBucket(_ context.Context, bucket string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCalls = append(f.ensureCalls, bucket)
	return f.ensureErr
}

func (f *fakeAPI) putObject(_ context.Context, bucket, key, contentType string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	f.objects[bucket+"/"+key] = bytes.Clone(data)
	f.contentTypes[bucket+"/"+key] = contentType
	return nil
}

func (f *fakeAPI) getObject(_ context.Context, bucket, key string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.getErr != nil {
		return nil, "", f.getErr
	}
	data, ok := f.objects[bucket+"/"+key]
	if !ok {
		return nil, "", fmt.Errorf("get object %q: %w", key, ErrNotFound)
	}
	return bytes.Clone(data), f.contentTypes[bucket+"/"+key], nil
}

// stored returns the raw bytes the fake holds for bucket/key.
func (f *fakeAPI) stored(bucket, key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[bucket+"/"+key]
	return data, ok
}

func newTestStore(t *testing.T, api *fakeAPI) *Store {
	t.Helper()
	s, err := newStore(context.Background(), "test-bucket", testCipher(t), api)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	return s
}

func TestNewStoreEnsuresBucket(t *testing.T) {
	api := newFakeAPI()
	if _, err := newStore(context.Background(), "some-bucket", testCipher(t), api); err != nil {
		t.Fatalf("newStore: %v", err)
	}
	if got, want := api.ensureCalls, []string{"some-bucket"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("ensureBucket calls = %v, want %v", got, want)
	}
}

func TestNewStoreDefaultsBucket(t *testing.T) {
	api := newFakeAPI()
	s, err := newStore(context.Background(), "", testCipher(t), api)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	if s.bucket != defaultBucket {
		t.Errorf("bucket = %q, want %q", s.bucket, defaultBucket)
	}
	if len(api.ensureCalls) != 1 || api.ensureCalls[0] != defaultBucket {
		t.Errorf("ensureBucket calls = %v, want [%q]", api.ensureCalls, defaultBucket)
	}
}

func TestNewStoreEnsureBucketError(t *testing.T) {
	api := newFakeAPI()
	api.ensureErr = errors.New("minio down")
	if _, err := newStore(context.Background(), "b", testCipher(t), api); err == nil {
		t.Fatal("newStore succeeded despite ensureBucket failure")
	}
}

func TestNewStoreNilCipher(t *testing.T) {
	if _, err := newStore(context.Background(), "b", nil, newFakeAPI()); err == nil {
		t.Fatal("newStore succeeded with nil cipher")
	}
}

func TestNewConfigValidation(t *testing.T) {
	ctx := context.Background()
	cipher := testCipher(t)
	for name, cfg := range map[string]Config{
		"missing endpoint":   {AccessKey: "ak", SecretKey: "sk"},
		"missing access key": {Endpoint: "minio:9000", SecretKey: "sk"},
		"missing secret key": {Endpoint: "minio:9000", AccessKey: "ak"},
	} {
		if _, err := New(ctx, cfg, cipher); err == nil {
			t.Errorf("New(%s) succeeded, want error", name)
		}
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	api := newFakeAPI()
	s := newTestStore(t, api)
	tc := testTenant(t, "tenant-a")
	plaintext := []byte("hello, encrypted world")

	ref, err := s.Put(ctx, tc, "upload/abc123", "text/plain", plaintext)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	wantSum := sha256.Sum256(plaintext)
	if ref.GetBucket() != "test-bucket" {
		t.Errorf("ref.bucket = %q, want %q", ref.GetBucket(), "test-bucket")
	}
	if want := "tenant-a/upload/abc123"; ref.GetKey() != want {
		t.Errorf("ref.key = %q, want %q", ref.GetKey(), want)
	}
	if ref.GetSizeBytes() != int64(len(plaintext)) {
		t.Errorf("ref.size_bytes = %d, want plaintext size %d", ref.GetSizeBytes(), len(plaintext))
	}
	if ref.GetContentType() != "text/plain" {
		t.Errorf("ref.content_type = %q, want %q", ref.GetContentType(), "text/plain")
	}
	if want := hex.EncodeToString(wantSum[:]); ref.GetSha256() != want {
		t.Errorf("ref.sha256 = %q, want plaintext hash %q", ref.GetSha256(), want)
	}

	got, err := s.Get(ctx, tc, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("Get = %q, want %q", got, plaintext)
	}
}

func TestPutEncryptsAtRest(t *testing.T) {
	ctx := context.Background()
	api := newFakeAPI()
	s := newTestStore(t, api)
	tc := testTenant(t, "tenant-a")
	plaintext := []byte("super secret original document body")

	ref, err := s.Put(ctx, tc, "k", "application/octet-stream", plaintext)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	stored, ok := api.stored("test-bucket", ref.GetKey())
	if !ok {
		t.Fatal("object not stored")
	}
	if bytes.Equal(stored, plaintext) {
		t.Error("stored bytes equal plaintext: not encrypted at rest")
	}
	if bytes.Contains(stored, plaintext) {
		t.Error("stored bytes contain plaintext")
	}
	if ref.GetSizeBytes() != int64(len(plaintext)) {
		t.Errorf("ref.size_bytes = %d, want PLAINTEXT size %d (not ciphertext %d)",
			ref.GetSizeBytes(), len(plaintext), len(stored))
	}
}

func TestPutFailsClosedWithoutTenant(t *testing.T) {
	s := newTestStore(t, newFakeAPI())
	_, err := s.Put(context.Background(), tenancy.Context{}, "k", "text/plain", []byte("x"))
	if !errors.Is(err, tenancy.ErrNoTenant) {
		t.Errorf("Put with zero tenant: err = %v, want ErrNoTenant", err)
	}
}

func TestPutRejectsBadKeys(t *testing.T) {
	s := newTestStore(t, newFakeAPI())
	tc := testTenant(t, "tenant-a")
	for _, key := range []string{"", "/abs", "a//b", "..", "../x", "a/../b", "trailing/"} {
		if _, err := s.Put(context.Background(), tc, key, "text/plain", []byte("x")); err == nil {
			t.Errorf("Put(%q) succeeded, want invalid-key error", key)
		}
	}
}

func TestPutPropagatesStoreError(t *testing.T) {
	api := newFakeAPI()
	s := newTestStore(t, api)
	api.putErr = errors.New("disk full")
	if _, err := s.Put(context.Background(), testTenant(t, "tenant-a"), "k", "", []byte("x")); err == nil {
		t.Fatal("Put succeeded despite putObject failure")
	}
}

func TestGetEnforcesTenantPrefix(t *testing.T) {
	ctx := context.Background()
	api := newFakeAPI()
	s := newTestStore(t, api)
	tenantA := testTenant(t, "tenant-a")
	tenantB := testTenant(t, "tenant-b")

	ref, err := s.Put(ctx, tenantA, "secret", "text/plain", []byte("tenant A data"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	_, err = s.Get(ctx, tenantB, ref)
	if !errors.Is(err, ErrTenantMismatch) {
		t.Errorf("cross-tenant Get: err = %v, want ErrTenantMismatch", err)
	}
	if api.getCalls != 0 {
		t.Errorf("cross-tenant Get hit the object store %d times; must fail closed before I/O", api.getCalls)
	}

	// A prefix that merely *starts with* the tenant ID must not pass: tenant
	// "tenant-a" must not read "tenant-ab/...".
	refAB := &askerv1.BlobRef{Bucket: "test-bucket", Key: "tenant-ab/secret", Sha256: ref.GetSha256()}
	if _, err := s.Get(ctx, tenantA, refAB); !errors.Is(err, ErrTenantMismatch) {
		t.Errorf("prefix-collision Get: err = %v, want ErrTenantMismatch", err)
	}
}

func TestGetCrossTenantCiphertextUndecryptable(t *testing.T) {
	// Even if an attacker rebinds tenant A's object under tenant B's prefix
	// (store-level tampering the prefix check cannot see), the tenant-bound
	// AEAD must refuse to decrypt it for B.
	ctx := context.Background()
	api := newFakeAPI()
	s := newTestStore(t, api)
	tenantA := testTenant(t, "tenant-a")
	tenantB := testTenant(t, "tenant-b")

	ref, err := s.Put(ctx, tenantA, "doc", "text/plain", []byte("tenant A data"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Provision tenant B's DEK so decryption fails on AAD, not a missing key.
	if _, err := s.Put(ctx, tenantB, "own", "text/plain", []byte("tenant B data")); err != nil {
		t.Fatalf("Put for tenant B: %v", err)
	}

	stolen, _ := api.stored("test-bucket", ref.GetKey())
	api.objects["test-bucket/tenant-b/doc"] = stolen
	forged := &askerv1.BlobRef{Bucket: "test-bucket", Key: "tenant-b/doc", Sha256: ref.GetSha256()}

	if _, err := s.Get(ctx, tenantB, forged); err == nil {
		t.Fatal("Get decrypted another tenant's ciphertext")
	}
}

func TestGetVerifiesSha256(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, newFakeAPI())
	tc := testTenant(t, "tenant-a")

	ref, err := s.Put(ctx, tc, "doc", "text/plain", []byte("content"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	bad := &askerv1.BlobRef{Bucket: ref.GetBucket(), Key: ref.GetKey(), Sha256: strings.Repeat("ab", 32)}
	if _, err := s.Get(ctx, tc, bad); !errors.Is(err, ErrChecksumMismatch) {
		t.Errorf("Get with wrong sha256: err = %v, want ErrChecksumMismatch", err)
	}

	noSum := &askerv1.BlobRef{Bucket: ref.GetBucket(), Key: ref.GetKey()}
	if _, err := s.Get(ctx, tc, noSum); err == nil {
		t.Error("Get with empty sha256 succeeded, want fail-closed error")
	}

	// Uppercase hex of the correct digest is accepted.
	upper := &askerv1.BlobRef{Bucket: ref.GetBucket(), Key: ref.GetKey(), Sha256: strings.ToUpper(ref.GetSha256())}
	if _, err := s.Get(ctx, tc, upper); err != nil {
		t.Errorf("Get with uppercase sha256: %v", err)
	}
}

func TestGetTamperedCiphertextFails(t *testing.T) {
	ctx := context.Background()
	api := newFakeAPI()
	s := newTestStore(t, api)
	tc := testTenant(t, "tenant-a")

	ref, err := s.Put(ctx, tc, "doc", "text/plain", []byte("content"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	stored, _ := api.stored("test-bucket", ref.GetKey())
	stored[len(stored)-1] ^= 0xff
	api.objects["test-bucket/"+ref.GetKey()] = stored

	if _, err := s.Get(ctx, tc, ref); err == nil {
		t.Fatal("Get succeeded on tampered ciphertext")
	}
}

func TestGetMissingObject(t *testing.T) {
	s := newTestStore(t, newFakeAPI())
	tc := testTenant(t, "tenant-a")
	ref := &askerv1.BlobRef{Bucket: "test-bucket", Key: "tenant-a/nope", Sha256: strings.Repeat("00", 32)}
	if _, err := s.Get(context.Background(), tc, ref); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing object: err = %v, want ErrNotFound", err)
	}
}

func TestGetInvalidInputs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, newFakeAPI())
	tc := testTenant(t, "tenant-a")
	okRef := &askerv1.BlobRef{Bucket: "test-bucket", Key: "tenant-a/k", Sha256: strings.Repeat("00", 32)}

	if _, err := s.Get(ctx, tenancy.Context{}, okRef); !errors.Is(err, tenancy.ErrNoTenant) {
		t.Errorf("Get with zero tenant: err = %v, want ErrNoTenant", err)
	}
	if _, err := s.Get(ctx, tc, nil); err == nil {
		t.Error("Get(nil ref) succeeded")
	}
	if _, err := s.Get(ctx, tc, &askerv1.BlobRef{Bucket: "test-bucket", Sha256: "00"}); err == nil {
		t.Error("Get with empty key succeeded")
	}
	foreign := &askerv1.BlobRef{Bucket: "other-bucket", Key: "tenant-a/k", Sha256: "00"}
	if _, err := s.Get(ctx, tc, foreign); err == nil {
		t.Error("Get with foreign bucket succeeded, want fail-closed error")
	}
}

func TestGetByKeyRoundTrip(t *testing.T) {
	ctx := context.Background()
	api := newFakeAPI()
	s := newTestStore(t, api)
	tc := testTenant(t, "tenant-a")
	plaintext := []byte("the decrypted thumbnail bytes")

	// Put then GetByKey returns the bytes WITHOUT any sha256 — the gateway/UI
	// path holds only the thumbnail key.
	ref, err := s.Put(ctx, tc, "thumb/abc.jpg", "image/jpeg", plaintext)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, contentType, err := s.GetByKey(ctx, tc, ref.GetKey())
	if err != nil {
		t.Fatalf("GetByKey: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("GetByKey = %q, want %q", got, plaintext)
	}
	if contentType != "image/jpeg" {
		t.Errorf("GetByKey content-type = %q, want image/jpeg", contentType)
	}
}

func TestGetByKeyDefaultsContentType(t *testing.T) {
	ctx := context.Background()
	api := newFakeAPI()
	s := newTestStore(t, api)
	tc := testTenant(t, "tenant-a")

	// Object stored with no Content-Type: GetByKey defaults to octet-stream.
	ref, err := s.Put(ctx, tc, "raw.bin", "", []byte("payload"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	_, contentType, err := s.GetByKey(ctx, tc, ref.GetKey())
	if err != nil {
		t.Fatalf("GetByKey: %v", err)
	}
	if contentType != defaultContentType {
		t.Errorf("GetByKey content-type = %q, want %q", contentType, defaultContentType)
	}
}

func TestGetByKeyFailsClosedWithoutTenant(t *testing.T) {
	s := newTestStore(t, newFakeAPI())
	if _, _, err := s.GetByKey(context.Background(), tenancy.Context{}, "tenant-a/k"); !errors.Is(err, tenancy.ErrNoTenant) {
		t.Errorf("GetByKey with zero tenant: err = %v, want ErrNoTenant", err)
	}
}

func TestGetByKeyRejectsEmptyKey(t *testing.T) {
	s := newTestStore(t, newFakeAPI())
	tc := testTenant(t, "tenant-a")
	if _, _, err := s.GetByKey(context.Background(), tc, ""); err == nil {
		t.Error("GetByKey with empty key succeeded, want fail-closed error")
	}
}

func TestGetByKeyCrossTenantDenied(t *testing.T) {
	ctx := context.Background()
	api := newFakeAPI()
	s := newTestStore(t, api)
	tenantA := testTenant(t, "tenant-a")
	tenantB := testTenant(t, "tenant-b")

	ref, err := s.Put(ctx, tenantA, "thumb/x.jpg", "image/jpeg", []byte("tenant A thumbnail"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Tenant B asks for tenant A's key by key alone: must be denied before I/O.
	if _, _, err := s.GetByKey(ctx, tenantB, ref.GetKey()); !errors.Is(err, ErrTenantMismatch) {
		t.Errorf("cross-tenant GetByKey: err = %v, want ErrTenantMismatch", err)
	}
	if api.getCalls != 0 {
		t.Errorf("cross-tenant GetByKey hit the object store %d times; must fail closed before I/O", api.getCalls)
	}
}

// TestGetByKeyRejectsTraversal proves a crafted key cannot escape the tenant
// prefix via "../": "<tenant>/../<other>/x" passes a naive HasPrefix yet
// resolves outside the prefix, so it must be rejected (before any I/O) rather
// than reaching the object store.
func TestGetByKeyRejectsTraversal(t *testing.T) {
	ctx := context.Background()
	api := newFakeAPI()
	s := newTestStore(t, api)
	tc := testTenant(t, "tenant-a")

	for _, key := range []string{
		"tenant-a/../tenant-b/secret",
		"tenant-a/..",
		"tenant-a/./x",
		"tenant-a//x",
		"tenant-a/sub/../../tenant-b/x",
	} {
		if _, _, err := s.GetByKey(ctx, tc, key); !errors.Is(err, ErrTenantMismatch) {
			t.Errorf("GetByKey(%q): err = %v, want ErrTenantMismatch", key, err)
		}
	}
	if api.getCalls != 0 {
		t.Errorf("traversal GetByKey hit the object store %d times; must fail closed before I/O", api.getCalls)
	}

	// A legitimate nested key still passes the gate (reaches I/O, then 404s).
	if _, _, err := s.GetByKey(ctx, tc, "tenant-a/thumb/id.jpg"); !errors.Is(err, ErrNotFound) {
		t.Errorf("legitimate nested GetByKey: err = %v, want ErrNotFound (passed the gate)", err)
	}
}

// TestGetRejectsTraversal proves Store.Get (the sha256 path) also rejects a
// "../" traversal key before any I/O — defense in depth, not just the AAD at
// decrypt time.
func TestGetRejectsTraversal(t *testing.T) {
	ctx := context.Background()
	api := newFakeAPI()
	s := newTestStore(t, api)
	tc := testTenant(t, "tenant-a")

	// Craft a ref whose key passes HasPrefix("tenant-a/") but traverses out.
	forged := &askerv1.BlobRef{
		Bucket: "test-bucket",
		Key:    "tenant-a/../tenant-b/secret",
		Sha256: strings.Repeat("00", 32),
	}
	if _, err := s.Get(ctx, tc, forged); !errors.Is(err, ErrTenantMismatch) {
		t.Errorf("Get with traversal key: err = %v, want ErrTenantMismatch", err)
	}
	if api.getCalls != 0 {
		t.Errorf("traversal Get hit the object store %d times; must fail closed before I/O", api.getCalls)
	}
}

// unsetenv removes name from the environment for the test's duration.
// t.Setenv registers restoration of the original value; the subsequent
// os.Unsetenv leaves the variable truly absent (not set-but-empty) so
// envDefault tags apply.
func unsetenv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("Unsetenv(%q): %v", name, err)
	}
}

func TestConfigEnvDefaults(t *testing.T) {
	for _, name := range []string{"MINIO_ENDPOINT", "MINIO_ACCESS_KEY", "MINIO_SECRET_KEY", "BLOB_BUCKET", "MINIO_USE_SSL"} {
		unsetenv(t, name)
	}

	var cfg Config
	if err := config.Load("", &cfg); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Endpoint != "minio:9000" {
		t.Errorf("Endpoint default = %q, want %q", cfg.Endpoint, "minio:9000")
	}
	if cfg.Bucket != "asker-blobs" {
		t.Errorf("Bucket default = %q, want %q", cfg.Bucket, "asker-blobs")
	}
	if cfg.UseSSL {
		t.Error("UseSSL default = true, want false")
	}
}

func TestConfigEnvValues(t *testing.T) {
	t.Setenv("MINIO_ENDPOINT", "minio.internal:9123")
	t.Setenv("MINIO_ACCESS_KEY", "ak")
	t.Setenv("MINIO_SECRET_KEY", "sk")
	t.Setenv("BLOB_BUCKET", "custom-bucket")
	t.Setenv("MINIO_USE_SSL", "true")

	var cfg Config
	if err := config.Load("", &cfg); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	want := Config{Endpoint: "minio.internal:9123", AccessKey: "ak", SecretKey: "sk", Bucket: "custom-bucket", UseSSL: true}
	if cfg != want {
		t.Errorf("cfg = %+v, want %+v", cfg, want)
	}
}
