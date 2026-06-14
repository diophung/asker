package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/asker/asker/platform/crypto"
	"github.com/asker/asker/platform/tenancy"
)

// fileDEKStore is a directory-backed crypto.DEKStore for the hub's blob
// encryption: one file of KEK-wrapped DEK bytes per tenant, kept on the same
// volume as the dev KEK so encrypted uploads stay decryptable across hub
// restarts. Tenant IDs are validated by tenancy ([A-Za-z0-9._-]{1,128},
// never "." or ".."), so they are safe as file names.
//
// This is the M1 dev-shim counterpart of the control plane's Postgres DEK
// store; M4's KMS/Vault-backed provider replaces both behind the same
// interface.
type fileDEKStore struct {
	dir string
}

var _ crypto.DEKStore = (*fileDEKStore)(nil)

func newFileDEKStore(dir string) *fileDEKStore { return &fileDEKStore{dir: dir} }

func (s *fileDEKStore) path(tenantID tenancy.TenantID) string {
	return filepath.Join(s.dir, string(tenantID)+".dek")
}

func (s *fileDEKStore) GetWrappedDEK(ctx context.Context, tenantID tenancy.TenantID) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tenantID == "" {
		return nil, tenancy.ErrNoTenant
	}
	b, err := os.ReadFile(s.path(tenantID))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, crypto.ErrDEKNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("connector-hub: read wrapped DEK: %w", err)
	}
	if len(b) == 0 {
		// An empty file is corruption, not absence: reporting "not found"
		// would mint a fresh DEK and silently orphan existing ciphertexts.
		return nil, fmt.Errorf("connector-hub: wrapped DEK file for tenant %s is empty", tenantID)
	}
	return b, nil
}

// PutWrappedDEK stores wrapped for the tenant, FIRST-WRITER-WINS (the
// crypto.DEKStore contract): an existing file is never replaced and the call
// becomes a nil no-op. The value is written to a temp file and hard-linked
// into place, so concurrent first-encrypts race on an atomic link and
// converge on a single stored DEK.
func (s *fileDEKStore) PutWrappedDEK(ctx context.Context, tenantID tenancy.TenantID, wrapped []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tenantID == "" {
		return tenancy.ErrNoTenant
	}
	if len(wrapped) == 0 {
		return errors.New("connector-hub: refusing to store empty wrapped DEK")
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("connector-hub: create DEK dir: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".dek-tmp-*")
	if err != nil {
		return fmt.Errorf("connector-hub: create temp DEK file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(wrapped); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("connector-hub: write wrapped DEK: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("connector-hub: close temp DEK file: %w", err)
	}
	if err := os.Link(tmpName, s.path(tenantID)); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil // first writer won; keep the stored value
		}
		return fmt.Errorf("connector-hub: store wrapped DEK: %w", err)
	}
	return nil
}

// Delete crypto-shreds the tenant's wrapped DEK file (the GDPR primitive).
// Idempotent: removing an absent file returns nil so a retried erasure
// converges. The control plane owns the durable Postgres DEK store; this hub
// shim mirrors the same Delete semantics for blob DEKs the hub wraps locally.
func (s *fileDEKStore) Delete(ctx context.Context, tenantID tenancy.TenantID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tenantID == "" {
		return tenancy.ErrNoTenant
	}
	if err := os.Remove(s.path(tenantID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("connector-hub: delete wrapped DEK: %w", err)
	}
	return nil
}
