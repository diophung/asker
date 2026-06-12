package main

import (
	"context"
	"errors"
	"time"

	"github.com/asker/asker/platform/tenancy"
)

// ErrNotFound is returned by Store implementations when a row does not exist
// FOR THE GIVEN TENANT. A row owned by another tenant and a row that has never
// existed produce the exact same error, so callers (and clients) cannot use
// the control plane as a cross-tenant existence oracle.
var ErrNotFound = errors.New("control-plane: not found")

// Tenant is a registered tenant.
type Tenant struct {
	ID        tenancy.TenantID
	CreatedAt time.Time
}

// ConnectorInstance is one tenant's configured connection to one source.
// Status holds the ConnectorStatus enum name ("ACTIVE", "PAUSED", "ERROR").
type ConnectorInstance struct {
	ID          string // UUID, assigned by the store on create
	TenantID    tenancy.TenantID
	ConnectorID string
	DisplayName string
	ConfigJSON  []byte // valid JSON, normalized to "{}" when empty
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// SyncState is the resumable cursor + health row for one connector instance.
// Phase holds the SyncPhase enum name ("PENDING", "FULL_SYNC", "INCREMENTAL",
// "FAILED"). Nil time pointers map to SQL NULL (sync never started/completed).
type SyncState struct {
	ConnectorInstanceID string // UUID
	TenantID            tenancy.TenantID
	Cursor              string
	Phase               string
	LastSyncStarted     *time.Time
	LastSyncCompleted   *time.Time
	LastError           string
	DocsEmitted         int64
}

// Store is the control-plane persistence boundary. Two implementations exist:
// memStore (unit tests) and pgStore (production, Postgres via pgx).
//
// Tenancy contract: every method takes the tenant explicitly and MUST scope
// every read, write, and delete to it. Lookups that miss — including lookups
// for rows owned by a different tenant — return ErrNotFound.
type Store interface {
	// EnsureTenant idempotently registers the tenant, returning the stored
	// row (the original creation time survives repeated calls).
	EnsureTenant(ctx context.Context, tenantID tenancy.TenantID) (Tenant, error)

	// CreateConnectorInstance persists a new instance for inst.TenantID and
	// returns it with ID and timestamps assigned. It implicitly ensures the
	// tenant row exists (the tenant identity always originates from a
	// verified JWT, so registering it here is exactly EnsureTenant).
	CreateConnectorInstance(ctx context.Context, inst ConnectorInstance) (ConnectorInstance, error)

	// ListConnectorInstances returns the tenant's instances ordered by
	// creation time (then ID, for a stable order).
	ListConnectorInstances(ctx context.Context, tenantID tenancy.TenantID) ([]ConnectorInstance, error)

	GetConnectorInstance(ctx context.Context, tenantID tenancy.TenantID, id string) (ConnectorInstance, error)

	// DeleteConnectorInstance removes the instance and, by cascade, its sync
	// state and token.
	DeleteConnectorInstance(ctx context.Context, tenantID tenancy.TenantID, id string) error

	// GetSyncState returns the instance's sync state. When the instance
	// exists but has never had SetSyncState called, it returns a synthesized
	// PENDING state (proto comment: "created, no sync yet") rather than an
	// error; ErrNotFound means the instance itself is absent for this tenant.
	GetSyncState(ctx context.Context, tenantID tenancy.TenantID, instanceID string) (SyncState, error)

	// SetSyncState upserts the instance's sync state; this is the cursor
	// checkpoint path. ErrNotFound when the instance is absent for this
	// tenant.
	SetSyncState(ctx context.Context, tenantID tenancy.TenantID, st SyncState) (SyncState, error)

	// PutToken upserts the instance's credential ciphertext. The value MUST
	// already be encrypted (the gRPC layer encrypts via platform/crypto
	// before calling this). ErrNotFound when the instance is absent for this
	// tenant.
	PutToken(ctx context.Context, tenantID tenancy.TenantID, instanceID string, ciphertext []byte) error

	GetToken(ctx context.Context, tenantID tenancy.TenantID, instanceID string) ([]byte, error)

	DeleteToken(ctx context.Context, tenantID tenancy.TenantID, instanceID string) error
}
