package crypto

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/asker/asker/platform/tenancy"
)

func testTenant(t *testing.T, id string) tenancy.Context {
	t.Helper()
	tc, err := tenancy.FromClaims(map[string]any{"tenant_id": id})
	if err != nil {
		t.Fatalf("build test tenant %q: %v", id, err)
	}
	return tc
}

func newTestKEK(t *testing.T) KEKProvider {
	t.Helper()
	kek, err := NewFileKEK(filepath.Join(t.TempDir(), "kek.bin"))
	if err != nil {
		t.Fatalf("NewFileKEK: %v", err)
	}
	return kek
}

func newTestCipher(t *testing.T) (*TenantCipher, KEKProvider, DEKStore) {
	t.Helper()
	kek := newTestKEK(t)
	store := NewMemDEKStore()
	return NewTenantCipher(kek, store), kek, store
}

func TestTenantCipherRoundTrip(t *testing.T) {
	c, _, _ := newTestCipher(t)
	ctx := context.Background()
	tc := testTenant(t, "tenant-a")

	for name, plaintext := range map[string][]byte{
		"empty": {},
		"short": []byte("hello, asker"),
		"large": bytes.Repeat([]byte("0123456789abcdef"), 64*1024), // 1 MiB
	} {
		ct, err := c.Encrypt(ctx, tc, plaintext)
		if err != nil {
			t.Fatalf("%s: Encrypt: %v", name, err)
		}
		if ct[0] != blobVersion1 {
			t.Fatalf("%s: version byte = 0x%02x, want 0x%02x", name, ct[0], blobVersion1)
		}
		if len(plaintext) > 0 && bytes.Contains(ct, plaintext) {
			t.Fatalf("%s: ciphertext contains the plaintext", name)
		}
		got, err := c.Decrypt(ctx, tc, ct)
		if err != nil {
			t.Fatalf("%s: Decrypt: %v", name, err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Fatalf("%s: round trip mismatch", name)
		}
	}
}

func TestTenantCipherNonceUniqueness(t *testing.T) {
	c, _, _ := newTestCipher(t)
	ctx := context.Background()
	tc := testTenant(t, "tenant-a")
	plaintext := []byte("same input")

	a, err := c.Encrypt(ctx, tc, plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	b, err := c.Encrypt(ctx, tc, plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Error("two encryptions of the same plaintext produced identical ciphertexts")
	}
}

func TestTenantCipherCrossTenantDecryptFails(t *testing.T) {
	c, _, _ := newTestCipher(t)
	ctx := context.Background()
	tenantA := testTenant(t, "tenant-a")
	tenantB := testTenant(t, "tenant-b")

	// Provision both tenants so the failure below is authentication, not a
	// missing DEK.
	ctA, err := c.Encrypt(ctx, tenantA, []byte("secret of A"))
	if err != nil {
		t.Fatalf("Encrypt A: %v", err)
	}
	if _, err := c.Encrypt(ctx, tenantB, []byte("secret of B")); err != nil {
		t.Fatalf("Encrypt B: %v", err)
	}

	if _, err := c.Decrypt(ctx, tenantB, ctA); err == nil {
		t.Error("tenant B decrypted tenant A's blob")
	}
}

func TestTenantCipherDecryptUnprovisionedTenant(t *testing.T) {
	c, _, _ := newTestCipher(t)
	ctx := context.Background()
	tenantA := testTenant(t, "tenant-a")
	tenantB := testTenant(t, "tenant-b")

	ct, err := c.Encrypt(ctx, tenantA, []byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := c.Decrypt(ctx, tenantB, ct); !errors.Is(err, ErrDEKNotFound) {
		t.Errorf("Decrypt for unprovisioned tenant: err = %v, want ErrDEKNotFound", err)
	}
}

func TestTenantCipherTamperedCiphertextFails(t *testing.T) {
	c, _, _ := newTestCipher(t)
	ctx := context.Background()
	tc := testTenant(t, "tenant-a")

	ct, err := c.Encrypt(ctx, tc, []byte("integrity matters"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	for _, pos := range []int{1, 1 + gcmNonceSize, len(ct) - 1} {
		tampered := bytes.Clone(ct)
		tampered[pos] ^= 0x01
		if _, err := c.Decrypt(ctx, tc, tampered); err == nil {
			t.Errorf("decrypt of blob tampered at byte %d succeeded", pos)
		}
	}
	if _, err := c.Decrypt(ctx, tc, ct[:1+gcmNonceSize]); err == nil {
		t.Error("decrypt of truncated blob succeeded")
	}
	if _, err := c.Decrypt(ctx, tc, nil); err == nil {
		t.Error("decrypt of empty blob succeeded")
	}
}

func TestTenantCipherRejectsUnknownVersion(t *testing.T) {
	c, _, _ := newTestCipher(t)
	ctx := context.Background()
	tc := testTenant(t, "tenant-a")

	ct, err := c.Encrypt(ctx, tc, []byte("versioned"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ct[0] = 0x02
	if _, err := c.Decrypt(ctx, tc, ct); !errors.Is(err, errUnsupportedVersion) {
		t.Errorf("Decrypt with version 0x02: err = %v, want errUnsupportedVersion", err)
	}
}

func TestTenantCipherWrongKEKFails(t *testing.T) {
	store := NewMemDEKStore()
	ctx := context.Background()
	tc := testTenant(t, "tenant-a")

	c1 := NewTenantCipher(newTestKEK(t), store)
	ct, err := c1.Encrypt(ctx, tc, []byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Same store, different KEK: the stored wrapped DEK fails to unwrap.
	c2 := NewTenantCipher(newTestKEK(t), store)
	if _, err := c2.Decrypt(ctx, tc, ct); err == nil {
		t.Error("decrypt with a different KEK succeeded")
	}
}

func TestTenantCipherDEKPersistsAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kek.bin")
	kek1, err := NewFileKEK(path)
	if err != nil {
		t.Fatalf("NewFileKEK: %v", err)
	}
	store := NewMemDEKStore()
	ctx := context.Background()
	tc := testTenant(t, "tenant-a")
	plaintext := []byte("survives restarts")

	ct, err := NewTenantCipher(kek1, store).Encrypt(ctx, tc, plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Fresh TenantCipher (empty DEK cache) and freshly loaded KEK, same
	// store: must decrypt without re-provisioning.
	kek2, err := NewFileKEK(path)
	if err != nil {
		t.Fatalf("NewFileKEK (reload): %v", err)
	}
	got, err := NewTenantCipher(kek2, store).Decrypt(ctx, tc, ct)
	if err != nil {
		t.Fatalf("Decrypt with new instance: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Error("round trip across instances mismatched")
	}
}

func TestTenantCipherRejectsZeroTenancyContext(t *testing.T) {
	c, _, _ := newTestCipher(t)
	ctx := context.Background()
	var zero tenancy.Context

	if _, err := c.Encrypt(ctx, zero, []byte("x")); !errors.Is(err, tenancy.ErrNoTenant) {
		t.Errorf("Encrypt with zero Context: err = %v, want ErrNoTenant", err)
	}
	if _, err := c.Decrypt(ctx, zero, []byte{blobVersion1}); !errors.Is(err, tenancy.ErrNoTenant) {
		t.Errorf("Decrypt with zero Context: err = %v, want ErrNoTenant", err)
	}
}

// countingStore wraps a DEKStore and counts calls, for asserting provisioning
// behavior under concurrency.
type countingStore struct {
	DEKStore
	mu   sync.Mutex
	puts int
}

func (s *countingStore) PutWrappedDEK(ctx context.Context, tenantID tenancy.TenantID, wrapped []byte) error {
	s.mu.Lock()
	s.puts++
	s.mu.Unlock()
	return s.DEKStore.PutWrappedDEK(ctx, tenantID, wrapped)
}

func TestTenantCipherConcurrentFirstEncrypt(t *testing.T) {
	kek := newTestKEK(t)
	store := &countingStore{DEKStore: NewMemDEKStore()}
	c := NewTenantCipher(kek, store)
	ctx := context.Background()
	tc := testTenant(t, "tenant-a")

	const n = 32
	ciphertexts := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ciphertexts[i], errs[i] = c.Encrypt(ctx, tc, fmt.Appendf(nil, "message %d", i))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Encrypt[%d]: %v", i, err)
		}
	}

	// All ciphertexts must decrypt under a FRESH cipher that can only see the
	// single stored wrapped DEK — proving every goroutine converged on it
	// rather than encrypting under a transient local key.
	fresh := NewTenantCipher(kek, store)
	for i, ct := range ciphertexts {
		got, err := fresh.Decrypt(ctx, tc, ct)
		if err != nil {
			t.Fatalf("Decrypt[%d] under stored DEK: %v", i, err)
		}
		if want := fmt.Sprintf("message %d", i); string(got) != want {
			t.Fatalf("Decrypt[%d] = %q, want %q", got, got, want)
		}
	}
	if store.puts == 0 {
		t.Error("no PutWrappedDEK call recorded")
	}
}

// raceStore simulates losing the provision race to another process: the first
// Get reports no DEK, and Put fails with a conflict because a competitor's
// wrapped DEK is already stored.
type raceStore struct {
	inner    DEKStore
	mu       sync.Mutex
	firstGet bool
}

func (s *raceStore) GetWrappedDEK(ctx context.Context, tenantID tenancy.TenantID) ([]byte, error) {
	s.mu.Lock()
	first := !s.firstGet
	s.firstGet = true
	s.mu.Unlock()
	if first {
		return nil, ErrDEKNotFound
	}
	return s.inner.GetWrappedDEK(ctx, tenantID)
}

func (s *raceStore) PutWrappedDEK(ctx context.Context, tenantID tenancy.TenantID, wrapped []byte) error {
	return errors.New("conflict: wrapped DEK already exists")
}

func (s *raceStore) Delete(ctx context.Context, tenantID tenancy.TenantID) error {
	return s.inner.Delete(ctx, tenantID)
}

func TestTenantCipherConvergesOnStoredDEKAfterPutConflict(t *testing.T) {
	kek := newTestKEK(t)
	inner := NewMemDEKStore()
	ctx := context.Background()
	tc := testTenant(t, "tenant-a")

	// A competitor provisioned the tenant first.
	competitor := NewTenantCipher(kek, inner)
	ct, err := competitor.Encrypt(ctx, tc, []byte("provisioned by competitor"))
	if err != nil {
		t.Fatalf("competitor Encrypt: %v", err)
	}

	// Our cipher sees not-found, generates its own DEK, gets a Put conflict,
	// and must converge on the competitor's stored DEK.
	c := NewTenantCipher(kek, &raceStore{inner: inner})
	ours, err := c.Encrypt(ctx, tc, []byte("ours"))
	if err != nil {
		t.Fatalf("Encrypt after Put conflict: %v", err)
	}
	if _, err := competitor.Decrypt(ctx, tc, ours); err != nil {
		t.Errorf("competitor cannot decrypt our blob — did not converge: %v", err)
	}
	if _, err := c.Decrypt(ctx, tc, ct); err != nil {
		t.Errorf("we cannot decrypt competitor's blob — did not converge: %v", err)
	}
}

// failStore fails configurable operations.
type failStore struct {
	getErr error
	putErr error
}

func (s *failStore) GetWrappedDEK(ctx context.Context, tenantID tenancy.TenantID) ([]byte, error) {
	return nil, s.getErr
}

func (s *failStore) PutWrappedDEK(ctx context.Context, tenantID tenancy.TenantID, wrapped []byte) error {
	if s.putErr != nil {
		return s.putErr
	}
	return nil
}

func (s *failStore) Delete(ctx context.Context, tenantID tenancy.TenantID) error { return nil }

func TestTenantCipherStoreFailures(t *testing.T) {
	kek := newTestKEK(t)
	ctx := context.Background()
	tc := testTenant(t, "tenant-a")
	storeDown := errors.New("store down")

	t.Run("get fails", func(t *testing.T) {
		c := NewTenantCipher(kek, &failStore{getErr: storeDown})
		if _, err := c.Encrypt(ctx, tc, []byte("x")); !errors.Is(err, storeDown) {
			t.Errorf("Encrypt: err = %v, want wrapped store error", err)
		}
		if _, err := c.Decrypt(ctx, tc, []byte{blobVersion1}); !errors.Is(err, storeDown) {
			t.Errorf("Decrypt: err = %v, want wrapped store error", err)
		}
	})

	t.Run("put and read-back fail", func(t *testing.T) {
		putErr := errors.New("put rejected")
		c := NewTenantCipher(kek, &failStore{getErr: ErrDEKNotFound, putErr: putErr})
		_, err := c.Encrypt(ctx, tc, []byte("x"))
		if !errors.Is(err, putErr) {
			t.Errorf("Encrypt: err = %v, want wrapped put error", err)
		}
	})

	t.Run("put ok but read-back fails", func(t *testing.T) {
		c := NewTenantCipher(kek, &failStore{getErr: ErrDEKNotFound})
		_, err := c.Encrypt(ctx, tc, []byte("x"))
		if err == nil || !strings.Contains(err.Error(), "read back") {
			t.Errorf("Encrypt: err = %v, want read-back failure", err)
		}
	})
}

// overwritableStore allows tests to corrupt a stored wrapped DEK (memDEKStore
// is first-writer-wins and cannot be overwritten through its public API).
type overwritableStore struct {
	mu   sync.Mutex
	deks map[tenancy.TenantID][]byte
}

func (s *overwritableStore) GetWrappedDEK(ctx context.Context, tenantID tenancy.TenantID) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.deks[tenantID]
	if !ok {
		return nil, ErrDEKNotFound
	}
	return bytes.Clone(w), nil
}

func (s *overwritableStore) PutWrappedDEK(ctx context.Context, tenantID tenancy.TenantID, wrapped []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.deks[tenantID]; !ok {
		s.deks[tenantID] = bytes.Clone(wrapped)
	}
	return nil
}

func (s *overwritableStore) Delete(ctx context.Context, tenantID tenancy.TenantID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.deks, tenantID)
	return nil
}

func TestTenantCipherCorruptedStoredDEKFails(t *testing.T) {
	kek := newTestKEK(t)
	store := &overwritableStore{deks: make(map[tenancy.TenantID][]byte)}
	ctx := context.Background()
	tc := testTenant(t, "tenant-a")

	ct, err := NewTenantCipher(kek, store).Encrypt(ctx, tc, []byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	store.mu.Lock()
	store.deks[tc.TenantID()][5] ^= 0x01
	store.mu.Unlock()

	// Fresh cipher must reload the (now corrupted) wrapped DEK and fail
	// authentication during unwrap.
	if _, err := NewTenantCipher(kek, store).Decrypt(ctx, tc, ct); err == nil {
		t.Error("decrypt with corrupted stored DEK succeeded")
	}
}

func TestTenantCipherWrappedDEKMovedAcrossTenantsFails(t *testing.T) {
	kek := newTestKEK(t)
	store := &overwritableStore{deks: make(map[tenancy.TenantID][]byte)}
	ctx := context.Background()
	tenantA := testTenant(t, "tenant-a")
	tenantB := testTenant(t, "tenant-b")

	if _, err := NewTenantCipher(kek, store).Encrypt(ctx, tenantA, []byte("secret")); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Copy tenant A's wrapped DEK into tenant B's slot: the tenant-ID AAD
	// binding in the KEK wrap must make B's unwrap fail.
	store.mu.Lock()
	store.deks[tenantB.TenantID()] = bytes.Clone(store.deks[tenantA.TenantID()])
	store.mu.Unlock()

	c := NewTenantCipher(kek, store)
	if _, err := c.Encrypt(ctx, tenantB, []byte("x")); err == nil {
		t.Error("tenant B used tenant A's wrapped DEK, want unwrap authentication failure")
	}
}
