package crypto

import (
	"context"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/asker/asker/platform/tenancy"
)

// blobVersion1 is the version prefix for data ciphertexts produced by
// TenantCipher: 0x01 || nonce || GCM(DEK, plaintext, AAD=tenantID).
const blobVersion1 = 0x01

// TenantCipher encrypts and decrypts tenant data with per-tenant DEKs,
// provisioning a tenant's DEK lazily on first Encrypt (generate, wrap via the
// KEKProvider, persist via the DEKStore). Unwrapped DEKs are cached in memory
// after first use. Safe for concurrent use.
type TenantCipher struct {
	kek   KEKProvider
	store DEKStore

	mu    sync.RWMutex
	aeads map[tenancy.TenantID]cipher.AEAD
}

// NewTenantCipher returns a TenantCipher backed by kek and store.
func NewTenantCipher(kek KEKProvider, store DEKStore) *TenantCipher {
	return &TenantCipher{
		kek:   kek,
		store: store,
		aeads: make(map[tenancy.TenantID]cipher.AEAD),
	}
}

// Encrypt encrypts plaintext for the tenant in tc with AES-256-GCM under the
// tenant's DEK, binding the tenant ID as AAD. The first Encrypt for a tenant
// provisions its DEK. Output layout: 0x01 || 12-byte nonce || GCM ciphertext.
func (c *TenantCipher) Encrypt(ctx context.Context, tc tenancy.Context, plaintext []byte) ([]byte, error) {
	if tc.TenantID() == "" {
		return nil, fmt.Errorf("crypto: encrypt: %w", tenancy.ErrNoTenant)
	}
	aead, err := c.aeadFor(ctx, tc.TenantID(), true)
	if err != nil {
		return nil, fmt.Errorf("crypto: encrypt for tenant %q: %w", tc.TenantID(), err)
	}
	nonce := make([]byte, gcmNonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("crypto: encrypt: nonce: %w", err)
	}
	out := make([]byte, 0, 1+gcmNonceSize+len(plaintext)+aead.Overhead())
	out = append(out, blobVersion1)
	out = append(out, nonce...)
	out = aead.Seal(out, nonce, plaintext, []byte(tc.TenantID()))
	return out, nil
}

// Decrypt reverses Encrypt for the same tenant. It fails when the blob was
// encrypted for a different tenant, was tampered with, carries an unknown
// version prefix, or when the tenant has no DEK (ErrDEKNotFound) — Decrypt
// never provisions one.
func (c *TenantCipher) Decrypt(ctx context.Context, tc tenancy.Context, ciphertext []byte) ([]byte, error) {
	if tc.TenantID() == "" {
		return nil, fmt.Errorf("crypto: decrypt: %w", tenancy.ErrNoTenant)
	}
	if len(ciphertext) == 0 {
		return nil, errors.New("crypto: decrypt: empty ciphertext")
	}
	if ciphertext[0] != blobVersion1 {
		return nil, fmt.Errorf("%w: 0x%02x", errUnsupportedVersion, ciphertext[0])
	}
	body := ciphertext[1:]
	aead, err := c.aeadFor(ctx, tc.TenantID(), false)
	if err != nil {
		return nil, fmt.Errorf("crypto: decrypt for tenant %q: %w", tc.TenantID(), err)
	}
	if len(body) < gcmNonceSize+aead.Overhead() {
		return nil, errors.New("crypto: decrypt: ciphertext too short")
	}
	plaintext, err := aead.Open(nil, body[:gcmNonceSize], body[gcmNonceSize:], []byte(tc.TenantID()))
	if err != nil {
		// Deliberately uniform: do not reveal whether the key, AAD, or
		// ciphertext was at fault.
		return nil, fmt.Errorf("crypto: decrypt for tenant %q: authentication failed", tc.TenantID())
	}
	return plaintext, nil
}

// aeadFor returns the tenant's cached AEAD, loading (and, when create is
// true, provisioning) the DEK as needed.
func (c *TenantCipher) aeadFor(ctx context.Context, tenantID tenancy.TenantID, create bool) (cipher.AEAD, error) {
	c.mu.RLock()
	aead, ok := c.aeads[tenantID]
	c.mu.RUnlock()
	if ok {
		return aead, nil
	}

	dek, err := c.loadDEK(ctx, tenantID, create)
	if err != nil {
		return nil, err
	}
	aead, err = newAESGCM(dek)
	if err != nil {
		return nil, fmt.Errorf("init data cipher: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.aeads[tenantID]; ok {
		// A concurrent goroutine cached first. Both unwrapped the same stored
		// DEK (loadDEK converges on the store), so prefer the cached one.
		return existing, nil
	}
	c.aeads[tenantID] = aead
	return aead, nil
}

// loadDEK fetches and unwraps the tenant's DEK from the store, provisioning
// a new one when absent and create is true.
func (c *TenantCipher) loadDEK(ctx context.Context, tenantID tenancy.TenantID, create bool) ([]byte, error) {
	wrapped, err := c.store.GetWrappedDEK(ctx, tenantID)
	switch {
	case err == nil:
		return c.kek.UnwrapDEK(ctx, tenantID, wrapped)
	case errors.Is(err, ErrDEKNotFound):
		if !create {
			return nil, err
		}
		return c.provisionDEK(ctx, tenantID)
	default:
		return nil, fmt.Errorf("load wrapped DEK: %w", err)
	}
}

// provisionDEK generates, wraps, and persists a fresh DEK, then converges on
// whatever the store holds: PutWrappedDEK is first-writer-wins (see DEKStore),
// so after the Put — whether it stored our value, was an idempotent no-op, or
// returned a conflict error — the re-read returns the single authoritative
// wrapped DEK for the tenant.
func (c *TenantCipher) provisionDEK(ctx context.Context, tenantID tenancy.TenantID) ([]byte, error) {
	dek := make([]byte, dekSize)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, fmt.Errorf("generate DEK: %w", err)
	}
	wrapped, err := c.kek.WrapDEK(ctx, tenantID, dek)
	if err != nil {
		return nil, fmt.Errorf("wrap DEK: %w", err)
	}
	putErr := c.store.PutWrappedDEK(ctx, tenantID, wrapped)
	stored, getErr := c.store.GetWrappedDEK(ctx, tenantID)
	if getErr != nil {
		if putErr != nil {
			return nil, fmt.Errorf("persist wrapped DEK: %w (read-back also failed: %v)", putErr, getErr)
		}
		return nil, fmt.Errorf("read back wrapped DEK: %w", getErr)
	}
	return c.kek.UnwrapDEK(ctx, tenantID, stored)
}
