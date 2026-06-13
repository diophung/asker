package crypto

import (
	"context"
	"errors"
	"sync"

	"github.com/asker/asker/platform/tenancy"
)

// ErrDEKNotFound is returned by DEKStore.GetWrappedDEK when no wrapped DEK
// exists for the tenant.
var ErrDEKNotFound = errors.New("crypto: wrapped DEK not found")

// DEKStore persists wrapped (KEK-encrypted) tenant DEKs. Implementations
// never see key plaintext.
//
// PutWrappedDEK semantics are FIRST-WRITER-WINS and IDEMPOTENT: when a
// wrapped DEK already exists for the tenant, the stored value MUST be kept
// unchanged. Implementations may then return nil (treating the call as a
// no-op) or a conflict error — TenantCipher tolerates both by re-reading the
// store after every Put and using the stored value, so concurrent
// first-encrypts converge on a single DEK. A wrapped DEK is never silently
// replaced; key rotation (M4) will be an explicit, audited operation, not a
// Put.
//
// Delete is the GDPR crypto-shred primitive (M6): it permanently destroys the
// tenant's wrapped DEK. Because every blob/token ciphertext is sealed under
// that DEK and the KEK never persists the unwrapped key, destroying the
// wrapped DEK renders ALL of the tenant's ciphertext permanently unreadable —
// the fast, verifiable erasure primitive behind DeleteTenant. It is idempotent:
// deleting an absent DEK is a nil no-op (so a retried erasure converges), and a
// missing tenant ID fails closed with tenancy.ErrNoTenant. After Delete a fresh
// Encrypt for the same tenant provisions a NEW, unrelated DEK — old ciphertext
// stays unrecoverable. Callers MUST also drop any in-memory unwrapped-DEK cache
// (TenantCipher.Forget) so a cached AEAD cannot keep serving post-shred.
type DEKStore interface {
	GetWrappedDEK(ctx context.Context, tenantID tenancy.TenantID) ([]byte, error)
	PutWrappedDEK(ctx context.Context, tenantID tenancy.TenantID, wrapped []byte) error
	Delete(ctx context.Context, tenantID tenancy.TenantID) error
}

// memDEKStore is an in-memory DEKStore for tests and single-process dev use.
type memDEKStore struct {
	mu   sync.Mutex
	deks map[tenancy.TenantID][]byte
}

// NewMemDEKStore returns an empty in-memory DEKStore. It is safe for
// concurrent use and copies values on the way in and out.
func NewMemDEKStore() DEKStore {
	return &memDEKStore{deks: make(map[tenancy.TenantID][]byte)}
}

// GetWrappedDEK returns the stored wrapped DEK, or ErrDEKNotFound.
func (s *memDEKStore) GetWrappedDEK(ctx context.Context, tenantID tenancy.TenantID) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tenantID == "" {
		return nil, tenancy.ErrNoTenant
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wrapped, ok := s.deks[tenantID]
	if !ok {
		return nil, ErrDEKNotFound
	}
	out := make([]byte, len(wrapped))
	copy(out, wrapped)
	return out, nil
}

// PutWrappedDEK stores wrapped for the tenant. First writer wins: when a
// value already exists it is kept and the call returns nil.
func (s *memDEKStore) PutWrappedDEK(ctx context.Context, tenantID tenancy.TenantID, wrapped []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tenantID == "" {
		return tenancy.ErrNoTenant
	}
	if len(wrapped) == 0 {
		return errors.New("crypto: refusing to store empty wrapped DEK")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.deks[tenantID]; ok {
		return nil // first-writer-wins, idempotent
	}
	cp := make([]byte, len(wrapped))
	copy(cp, wrapped)
	s.deks[tenantID] = cp
	return nil
}

// Delete destroys the tenant's wrapped DEK (the GDPR crypto-shred). Idempotent:
// deleting an absent DEK returns nil so a retried erasure converges.
func (s *memDEKStore) Delete(ctx context.Context, tenantID tenancy.TenantID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tenantID == "" {
		return tenancy.ErrNoTenant
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.deks, tenantID)
	return nil
}
