package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/asker/asker/platform/tenancy"
)

const (
	// dekSize is the size of a tenant Data Encryption Key (AES-256).
	dekSize = 32
	// kekSize is the size of the Key Encryption Key (AES-256).
	kekSize = 32
	// gcmNonceSize is the standard AES-GCM nonce size used everywhere here.
	gcmNonceSize = 12

	// wrapVersion1 is the version prefix for wrapped-DEK blobs produced by
	// FileKEK: 0x01 || nonce || GCM(KEK, DEK, AAD=tenantID).
	wrapVersion1 = 0x01
)

// errUnsupportedVersion is returned when a stored blob carries a version
// prefix this build does not understand.
var errUnsupportedVersion = errors.New("crypto: unsupported ciphertext version")

// KEKProvider wraps and unwraps tenant DEKs with a Key Encryption Key. The
// tenant ID is bound into the wrapping as authenticated data, so a wrapped
// DEK presented under a different tenant fails to unwrap.
//
// NewFileKEK is the dev implementation; a HashiCorp Vault transit-backed
// implementation with the same interface arrives in M4.
type KEKProvider interface {
	WrapDEK(ctx context.Context, tenantID tenancy.TenantID, dek []byte) ([]byte, error)
	UnwrapDEK(ctx context.Context, tenantID tenancy.TenantID, wrapped []byte) ([]byte, error)
}

// fileKEK is the development KEKProvider: a single AES-256 KEK loaded from a
// local file, wrapping DEKs with AES-256-GCM and the tenant ID as AAD.
type fileKEK struct {
	aead cipher.AEAD
}

// NewFileKEK loads the 32-byte KEK stored at path, creating it atomically
// (0600, parent directory 0700) from crypto/rand if it does not exist.
// Existing files that are group- or world-accessible, are not regular files,
// or are not exactly 32 bytes are refused.
func NewFileKEK(path string) (KEKProvider, error) {
	key, err := loadOrCreateKEKFile(path)
	if err != nil {
		return nil, err
	}
	aead, err := newAESGCM(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: init KEK cipher: %w", err)
	}
	return &fileKEK{aead: aead}, nil
}

// WrapDEK encrypts a 32-byte DEK under the file KEK with the tenant ID bound
// as AAD. Output layout: 0x01 || nonce || GCM ciphertext.
func (k *fileKEK) WrapDEK(ctx context.Context, tenantID tenancy.TenantID, dek []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tenantID == "" {
		return nil, fmt.Errorf("crypto: wrap DEK: %w", tenancy.ErrNoTenant)
	}
	if len(dek) != dekSize {
		return nil, fmt.Errorf("crypto: wrap DEK: key must be %d bytes, got %d", dekSize, len(dek))
	}
	nonce := make([]byte, gcmNonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("crypto: wrap DEK: nonce: %w", err)
	}
	out := make([]byte, 0, 1+gcmNonceSize+len(dek)+k.aead.Overhead())
	out = append(out, wrapVersion1)
	out = append(out, nonce...)
	out = k.aead.Seal(out, nonce, dek, []byte(tenantID))
	return out, nil
}

// UnwrapDEK decrypts a wrapped DEK produced by WrapDEK for the same tenant.
// Any mismatch — different tenant, tampered blob, different KEK — fails
// authentication.
func (k *fileKEK) UnwrapDEK(ctx context.Context, tenantID tenancy.TenantID, wrapped []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tenantID == "" {
		return nil, fmt.Errorf("crypto: unwrap DEK: %w", tenancy.ErrNoTenant)
	}
	if len(wrapped) == 0 {
		return nil, errors.New("crypto: unwrap DEK: empty blob")
	}
	if wrapped[0] != wrapVersion1 {
		return nil, fmt.Errorf("%w: 0x%02x", errUnsupportedVersion, wrapped[0])
	}
	body := wrapped[1:]
	if len(body) < gcmNonceSize+k.aead.Overhead() {
		return nil, errors.New("crypto: unwrap DEK: blob too short")
	}
	dek, err := k.aead.Open(nil, body[:gcmNonceSize], body[gcmNonceSize:], []byte(tenantID))
	if err != nil {
		return nil, fmt.Errorf("crypto: unwrap DEK for tenant %q: authentication failed", tenantID)
	}
	if len(dek) != dekSize {
		return nil, fmt.Errorf("crypto: unwrap DEK: unexpected key size %d", len(dek))
	}
	return dek, nil
}

// newAESGCM builds an AES-256-GCM AEAD from a 32-byte key.
func newAESGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// loadOrCreateKEKFile reads the KEK at path, generating and atomically
// installing a fresh one when the file does not exist. Creation is
// first-writer-wins: the key is written to a 0600 temp file in the target
// directory and hard-linked into place, so concurrent creators converge on a
// single key and readers never observe a partial file.
func loadOrCreateKEKFile(path string) ([]byte, error) {
	key, err := readKEKFile(path)
	switch {
	case err == nil:
		return key, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("crypto: create KEK directory: %w", err)
	}
	key = make([]byte, kekSize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("crypto: generate KEK: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".kek-*")
	if err != nil {
		return nil, fmt.Errorf("crypto: create KEK temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(key); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("crypto: write KEK: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("crypto: write KEK: %w", err)
	}
	// os.CreateTemp creates 0600 files, but be explicit: this is a key.
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return nil, fmt.Errorf("crypto: chmod KEK: %w", err)
	}
	if err := os.Link(tmpName, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			// Lost the creation race; use the winner's key.
			return readKEKFile(path)
		}
		return nil, fmt.Errorf("crypto: install KEK file: %w", err)
	}
	return key, nil
}

// readKEKFile loads and validates an existing KEK file.
func readKEKFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("crypto: stat KEK file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("crypto: KEK file %s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("crypto: refusing KEK file %s with group/world-accessible mode %#o (want 0600)", path, perm)
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("crypto: read KEK file: %w", err)
	}
	if len(key) != kekSize {
		return nil, fmt.Errorf("crypto: KEK file %s must be %d bytes, got %d", path, kekSize, len(key))
	}
	return key, nil
}
