package crypto

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestSelectKEKDevUsesFileKEK(t *testing.T) {
	kekPath := filepath.Join(t.TempDir(), "kek.bin")
	kek, err := SelectKEK(KEKSelection{KEKFile: kekPath, IsProd: false})
	if err != nil {
		t.Fatalf("SelectKEK dev: %v", err)
	}
	// It must be the file KEK: a round-trip wrap/unwrap works with no network.
	ctx := context.Background()
	tid := testTenantID(t, "tenant-a")
	dek := make([]byte, dekSize)
	wrapped, err := kek.WrapDEK(ctx, tid, dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if _, err := kek.UnwrapDEK(ctx, tid, wrapped); err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
}

func TestSelectKEKProdWithoutVaultFailsClosed(t *testing.T) {
	_, err := SelectKEK(KEKSelection{KEKFile: filepath.Join(t.TempDir(), "kek.bin"), IsProd: true})
	if !errors.Is(err, ErrProdRequiresVault) {
		t.Fatalf("SelectKEK prod without vault: err = %v, want ErrProdRequiresVault", err)
	}
}

func TestSelectKEKVaultSelectedWhenAddrSet(t *testing.T) {
	ft := newFakeTransit(t)
	srv := httptest.NewServer(ft)
	t.Cleanup(srv.Close)

	// Even in prod, a set VaultAddr selects the Vault KEK (no fail-closed).
	kek, err := SelectKEK(KEKSelection{
		VaultAddr:    srv.URL,
		VaultToken:   ft.token,
		VaultKeyName: ft.keyName,
		IsProd:       true,
	})
	if err != nil {
		t.Fatalf("SelectKEK vault: %v", err)
	}
	ctx := context.Background()
	tid := testTenantID(t, "tenant-a")
	dek := make([]byte, dekSize)
	wrapped, err := kek.WrapDEK(ctx, tid, dek)
	if err != nil {
		t.Fatalf("WrapDEK via vault: %v", err)
	}
	if _, err := kek.UnwrapDEK(ctx, tid, wrapped); err != nil {
		t.Fatalf("UnwrapDEK via vault: %v", err)
	}
}

func TestSelectKEKVaultKeyNameDefaults(t *testing.T) {
	ft := newFakeTransit(t) // keyName "asker-kek"
	srv := httptest.NewServer(ft)
	t.Cleanup(srv.Close)
	// Empty VaultKeyName must default to "asker-kek".
	kek, err := SelectKEK(KEKSelection{VaultAddr: srv.URL, VaultToken: ft.token})
	if err != nil {
		t.Fatalf("SelectKEK vault default key: %v", err)
	}
	if _, err := kek.WrapDEK(context.Background(), testTenantID(t, "tenant-a"), make([]byte, dekSize)); err != nil {
		t.Fatalf("WrapDEK with defaulted key name: %v", err)
	}
}
