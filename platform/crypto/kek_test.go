package crypto

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/asker/asker/platform/tenancy"
)

// testTenantID returns a validated tenant ID for direct KEKProvider calls.
func testTenantID(t *testing.T, id string) tenancy.TenantID {
	t.Helper()
	return testTenant(t, id).TenantID()
}

func newDEK(t *testing.T) []byte {
	t.Helper()
	dek := make([]byte, dekSize)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		t.Fatalf("generate test DEK: %v", err)
	}
	return dek
}

func TestNewFileKEKCreatesFileAndDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "kek.bin")
	if _, err := NewFileKEK(path); err != nil {
		t.Fatalf("NewFileKEK: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat created KEK: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("KEK file mode = %#o, want 0600", got)
	}
	if info.Size() != kekSize {
		t.Errorf("KEK file size = %d, want %d", info.Size(), kekSize)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat KEK dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got&0o077 != 0 {
		t.Errorf("KEK dir mode = %#o, want no group/world bits", got)
	}

	// No stray temp files left behind.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read KEK dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("KEK dir has %d entries, want 1 (temp file leaked?)", len(entries))
	}
}

func TestNewFileKEKReusesExistingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kek.bin")
	ctx := context.Background()
	tid := testTenantID(t, "tenant-a")
	dek := newDEK(t)

	first, err := NewFileKEK(path)
	if err != nil {
		t.Fatalf("NewFileKEK (create): %v", err)
	}
	wrapped, err := first.WrapDEK(ctx, tid, dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}

	second, err := NewFileKEK(path)
	if err != nil {
		t.Fatalf("NewFileKEK (reload): %v", err)
	}
	got, err := second.UnwrapDEK(ctx, tid, wrapped)
	if err != nil {
		t.Fatalf("UnwrapDEK with reloaded KEK: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Error("reloaded KEK unwrapped a different DEK")
	}
}

func TestNewFileKEKRefusesPermissiveModes(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660, 0o601} {
		path := filepath.Join(t.TempDir(), "kek.bin")
		if err := os.WriteFile(path, make([]byte, kekSize), mode); err != nil {
			t.Fatalf("seed KEK file: %v", err)
		}
		// WriteFile is subject to umask; force the exact mode under test.
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		if _, err := NewFileKEK(path); err == nil {
			t.Errorf("NewFileKEK accepted mode %#o, want refusal", mode)
		} else if !strings.Contains(err.Error(), "group/world") {
			t.Errorf("mode %#o: error %q does not mention group/world access", mode, err)
		}
	}
}

func TestNewFileKEKRefusesWrongSize(t *testing.T) {
	for _, size := range []int{0, 16, 31, 33, 64} {
		path := filepath.Join(t.TempDir(), "kek.bin")
		if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
			t.Fatalf("seed KEK file: %v", err)
		}
		if _, err := NewFileKEK(path); err == nil {
			t.Errorf("NewFileKEK accepted %d-byte key file, want refusal", size)
		}
	}
}

func TestNewFileKEKRefusesNonRegularFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.bin")
	if err := os.WriteFile(target, make([]byte, kekSize), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(dir, "kek.bin")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := NewFileKEK(link); err == nil {
		t.Error("NewFileKEK followed a symlink, want refusal")
	}
}

func TestNewFileKEKConcurrentCreateConverges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kek.bin")
	ctx := context.Background()
	tid := testTenantID(t, "tenant-a")
	dek := newDEK(t)

	const n = 16
	providers := make([]KEKProvider, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			providers[i], errs[i] = NewFileKEK(path)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("NewFileKEK[%d]: %v", i, err)
		}
	}

	// Every provider must hold the same KEK: a DEK wrapped by one unwraps
	// under all others.
	wrapped, err := providers[0].WrapDEK(ctx, tid, dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	for i, p := range providers {
		got, err := p.UnwrapDEK(ctx, tid, wrapped)
		if err != nil {
			t.Fatalf("provider %d holds a different KEK: %v", i, err)
		}
		if !bytes.Equal(got, dek) {
			t.Fatalf("provider %d unwrapped a different DEK", i)
		}
	}
}

func TestFileKEKWrapUnwrapRoundTrip(t *testing.T) {
	kek := newTestKEK(t)
	ctx := context.Background()
	tid := testTenantID(t, "tenant-a")
	dek := newDEK(t)

	wrapped, err := kek.WrapDEK(ctx, tid, dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if bytes.Contains(wrapped, dek) {
		t.Fatal("wrapped blob contains the plaintext DEK")
	}
	if wrapped[0] != wrapVersion1 {
		t.Fatalf("wrapped version byte = 0x%02x, want 0x%02x", wrapped[0], wrapVersion1)
	}
	got, err := kek.UnwrapDEK(ctx, tid, wrapped)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Error("round-tripped DEK differs")
	}
}

func TestFileKEKUnwrapWrongTenantFails(t *testing.T) {
	kek := newTestKEK(t)
	ctx := context.Background()

	wrapped, err := kek.WrapDEK(ctx, testTenantID(t, "tenant-a"), newDEK(t))
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if _, err := kek.UnwrapDEK(ctx, testTenantID(t, "tenant-b"), wrapped); err == nil {
		t.Error("unwrap under a different tenant succeeded, want authentication failure")
	}
}

func TestFileKEKUnwrapTamperedFails(t *testing.T) {
	kek := newTestKEK(t)
	ctx := context.Background()
	tid := testTenantID(t, "tenant-a")

	wrapped, err := kek.WrapDEK(ctx, tid, newDEK(t))
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	for _, pos := range []int{1, 1 + gcmNonceSize, len(wrapped) - 1} {
		tampered := bytes.Clone(wrapped)
		tampered[pos] ^= 0x01
		if _, err := kek.UnwrapDEK(ctx, tid, tampered); err == nil {
			t.Errorf("unwrap of blob tampered at byte %d succeeded", pos)
		}
	}
}

func TestFileKEKUnwrapRejectsUnknownVersion(t *testing.T) {
	kek := newTestKEK(t)
	ctx := context.Background()
	tid := testTenantID(t, "tenant-a")

	wrapped, err := kek.WrapDEK(ctx, tid, newDEK(t))
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	wrapped[0] = 0x7f
	if _, err := kek.UnwrapDEK(ctx, tid, wrapped); !errors.Is(err, errUnsupportedVersion) {
		t.Errorf("unwrap with version 0x7f: err = %v, want errUnsupportedVersion", err)
	}
}

func TestFileKEKUnwrapRejectsShortBlobs(t *testing.T) {
	kek := newTestKEK(t)
	ctx := context.Background()
	tid := testTenantID(t, "tenant-a")

	for _, blob := range [][]byte{nil, {}, {wrapVersion1}, append([]byte{wrapVersion1}, make([]byte, gcmNonceSize)...)} {
		if _, err := kek.UnwrapDEK(ctx, tid, blob); err == nil {
			t.Errorf("unwrap of %d-byte blob succeeded", len(blob))
		}
	}
}

func TestFileKEKInputValidation(t *testing.T) {
	kek := newTestKEK(t)
	ctx := context.Background()
	tid := testTenantID(t, "tenant-a")

	if _, err := kek.WrapDEK(ctx, "", newDEK(t)); !errors.Is(err, tenancy.ErrNoTenant) {
		t.Errorf("WrapDEK with empty tenant: err = %v, want ErrNoTenant", err)
	}
	if _, err := kek.WrapDEK(ctx, tid, make([]byte, 16)); err == nil {
		t.Error("WrapDEK accepted a 16-byte DEK")
	}
	if _, err := kek.UnwrapDEK(ctx, "", []byte{wrapVersion1}); !errors.Is(err, tenancy.ErrNoTenant) {
		t.Errorf("UnwrapDEK with empty tenant: err = %v, want ErrNoTenant", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := kek.WrapDEK(canceled, tid, newDEK(t)); !errors.Is(err, context.Canceled) {
		t.Errorf("WrapDEK with canceled ctx: err = %v, want context.Canceled", err)
	}
	if _, err := kek.UnwrapDEK(canceled, tid, []byte{wrapVersion1}); !errors.Is(err, context.Canceled) {
		t.Errorf("UnwrapDEK with canceled ctx: err = %v, want context.Canceled", err)
	}
}

func TestFileKEKDifferentKEKFails(t *testing.T) {
	kekA := newTestKEK(t)
	kekB := newTestKEK(t)
	ctx := context.Background()
	tid := testTenantID(t, "tenant-a")

	wrapped, err := kekA.WrapDEK(ctx, tid, newDEK(t))
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if _, err := kekB.UnwrapDEK(ctx, tid, wrapped); err == nil {
		t.Error("unwrap under a different KEK succeeded")
	}
}
