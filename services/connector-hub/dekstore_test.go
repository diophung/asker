package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/asker/asker/platform/crypto"
	"github.com/asker/asker/platform/tenancy"
)

func TestFileDEKStoreRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "deks")
	s := newFileDEKStore(dir)
	ctx := context.Background()

	if _, err := s.GetWrappedDEK(ctx, "tenant-a"); !errors.Is(err, crypto.ErrDEKNotFound) {
		t.Fatalf("Get before Put: err = %v, want ErrDEKNotFound", err)
	}

	if err := s.PutWrappedDEK(ctx, "tenant-a", []byte("wrapped-1")); err != nil {
		t.Fatalf("PutWrappedDEK: %v", err)
	}
	got, err := s.GetWrappedDEK(ctx, "tenant-a")
	if err != nil || !bytes.Equal(got, []byte("wrapped-1")) {
		t.Fatalf("GetWrappedDEK = %q, %v", got, err)
	}

	// First-writer-wins: a second Put is a nil no-op that keeps the original.
	if err := s.PutWrappedDEK(ctx, "tenant-a", []byte("wrapped-2")); err != nil {
		t.Fatalf("second PutWrappedDEK: %v", err)
	}
	got, err = s.GetWrappedDEK(ctx, "tenant-a")
	if err != nil || !bytes.Equal(got, []byte("wrapped-1")) {
		t.Fatalf("after second put: GetWrappedDEK = %q, %v (first writer must win)", got, err)
	}

	// A fresh store over the same directory sees the persisted DEK.
	if got, err := newFileDEKStore(dir).GetWrappedDEK(ctx, "tenant-a"); err != nil || !bytes.Equal(got, []byte("wrapped-1")) {
		t.Fatalf("restart read = %q, %v", got, err)
	}
}

func TestFileDEKStoreValidation(t *testing.T) {
	s := newFileDEKStore(t.TempDir())
	ctx := context.Background()

	if _, err := s.GetWrappedDEK(ctx, ""); !errors.Is(err, tenancy.ErrNoTenant) {
		t.Errorf("Get with empty tenant: %v, want ErrNoTenant", err)
	}
	if err := s.PutWrappedDEK(ctx, "", []byte("x")); !errors.Is(err, tenancy.ErrNoTenant) {
		t.Errorf("Put with empty tenant: %v, want ErrNoTenant", err)
	}
	if err := s.PutWrappedDEK(ctx, "tenant-a", nil); err == nil {
		t.Error("Put with empty DEK accepted")
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.GetWrappedDEK(canceled, "tenant-a"); !errors.Is(err, context.Canceled) {
		t.Errorf("Get with canceled ctx: %v", err)
	}
	if err := s.PutWrappedDEK(canceled, "tenant-a", []byte("x")); !errors.Is(err, context.Canceled) {
		t.Errorf("Put with canceled ctx: %v", err)
	}
}

func TestFileDEKStoreEmptyFileIsCorruptionNotAbsence(t *testing.T) {
	dir := t.TempDir()
	s := newFileDEKStore(dir)
	if err := os.WriteFile(filepath.Join(dir, "tenant-a.dek"), nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := s.GetWrappedDEK(context.Background(), "tenant-a")
	if err == nil || errors.Is(err, crypto.ErrDEKNotFound) {
		t.Errorf("empty DEK file: err = %v, want a non-NotFound error", err)
	}
}

// TestFileDEKStoreBacksTenantCipher proves the store works under the real
// TenantCipher: encrypt, then decrypt with a SECOND cipher over the same
// KEK + directory (a hub restart), and cross-tenant decryption still fails.
func TestFileDEKStoreBacksTenantCipher(t *testing.T) {
	dir := t.TempDir()
	kek, err := crypto.NewFileKEK(filepath.Join(dir, "kek.bin"))
	if err != nil {
		t.Fatalf("NewFileKEK: %v", err)
	}
	dekDir := filepath.Join(dir, "deks")
	ctx := context.Background()
	tcA, err := tenancy.FromHeaderValue("tenant-a")
	if err != nil {
		t.Fatalf("FromHeaderValue: %v", err)
	}
	tcB, err := tenancy.FromHeaderValue("tenant-b")
	if err != nil {
		t.Fatalf("FromHeaderValue: %v", err)
	}

	c1 := crypto.NewTenantCipher(kek, newFileDEKStore(dekDir))
	ciphertext, err := c1.Encrypt(ctx, tcA, []byte("blob bytes"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	c2 := crypto.NewTenantCipher(kek, newFileDEKStore(dekDir)) // "restart"
	plain, err := c2.Decrypt(ctx, tcA, ciphertext)
	if err != nil || string(plain) != "blob bytes" {
		t.Fatalf("Decrypt after restart = %q, %v", plain, err)
	}

	if _, err := c2.Decrypt(ctx, tcB, ciphertext); err == nil {
		t.Error("cross-tenant decrypt succeeded")
	}
}
