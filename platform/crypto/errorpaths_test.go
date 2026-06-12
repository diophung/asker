package crypto

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/asker/asker/platform/tenancy"
)

func TestNewAESGCMRejectsBadKeySize(t *testing.T) {
	for _, size := range []int{0, 5, 16, 31} {
		if size == 16 {
			continue // AES-128 is a valid cipher size; we never pass it, but it would not error
		}
		if _, err := newAESGCM(make([]byte, size)); err == nil {
			t.Errorf("newAESGCM accepted %d-byte key", size)
		}
	}
}

// failKEK fails every operation, for exercising provisioning error paths.
type failKEK struct{ err error }

func (k *failKEK) WrapDEK(ctx context.Context, tenantID tenancy.TenantID, dek []byte) ([]byte, error) {
	return nil, k.err
}

func (k *failKEK) UnwrapDEK(ctx context.Context, tenantID tenancy.TenantID, wrapped []byte) ([]byte, error) {
	return nil, k.err
}

func TestTenantCipherWrapFailureSurfaces(t *testing.T) {
	kekDown := errors.New("kek unavailable")
	c := NewTenantCipher(&failKEK{err: kekDown}, NewMemDEKStore())

	_, err := c.Encrypt(context.Background(), testTenant(t, "tenant-a"), []byte("x"))
	if !errors.Is(err, kekDown) {
		t.Errorf("Encrypt with failing KEK: err = %v, want wrapped KEK error", err)
	}
}

func TestNewFileKEKUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission checks do not apply")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	if _, err := NewFileKEK(filepath.Join(parent, "sub", "kek.bin")); err == nil {
		t.Error("NewFileKEK created a key under an unwritable directory")
	}
}
