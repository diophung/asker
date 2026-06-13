package main

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/asker/asker/platform/tenancy"
	"github.com/google/uuid"
)

// memStore is an in-memory Store for unit tests. It mirrors pgStore behavior
// exactly: tenant-scoped lookups, ErrNotFound for cross-tenant access,
// cascade delete of sync state and tokens, synthesized PENDING sync state.
type memStore struct {
	mu        sync.Mutex
	tenants   map[tenancy.TenantID]time.Time
	instances map[string]ConnectorInstance
	syncs     map[string]SyncState
	tokens    map[string]memToken
}

type memToken struct {
	tenantID   tenancy.TenantID
	ciphertext []byte
}

var _ Store = (*memStore)(nil)

func newMemStore() *memStore {
	return &memStore{
		tenants:   make(map[tenancy.TenantID]time.Time),
		instances: make(map[string]ConnectorInstance),
		syncs:     make(map[string]SyncState),
		tokens:    make(map[string]memToken),
	}
}

func (m *memStore) EnsureTenant(ctx context.Context, tenantID tenancy.TenantID) (Tenant, error) {
	if err := ctx.Err(); err != nil {
		return Tenant{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	created, ok := m.tenants[tenantID]
	if !ok {
		created = time.Now().UTC()
		m.tenants[tenantID] = created
	}
	return Tenant{ID: tenantID, CreatedAt: created}, nil
}

func (m *memStore) CreateConnectorInstance(ctx context.Context, inst ConnectorInstance) (ConnectorInstance, error) {
	if err := ctx.Err(); err != nil {
		return ConnectorInstance{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tenants[inst.TenantID]; !ok {
		m.tenants[inst.TenantID] = time.Now().UTC()
	}
	now := time.Now().UTC()
	inst.ID = uuid.NewString()
	inst.ConfigJSON = slices.Clone(inst.ConfigJSON)
	inst.CreatedAt = now
	inst.UpdatedAt = now
	m.instances[inst.ID] = inst
	return cloneInstance(inst), nil
}

func (m *memStore) ListConnectorInstances(ctx context.Context, tenantID tenancy.TenantID) ([]ConnectorInstance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ConnectorInstance
	for _, inst := range m.instances {
		if inst.TenantID == tenantID {
			out = append(out, cloneInstance(inst))
		}
	}
	slices.SortFunc(out, func(a, b ConnectorInstance) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

func (m *memStore) GetConnectorInstance(ctx context.Context, tenantID tenancy.TenantID, id string) (ConnectorInstance, error) {
	if err := ctx.Err(); err != nil {
		return ConnectorInstance{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, ok := m.ownedInstance(tenantID, id)
	if !ok {
		return ConnectorInstance{}, ErrNotFound
	}
	return cloneInstance(inst), nil
}

func (m *memStore) DeleteConnectorInstance(ctx context.Context, tenantID tenancy.TenantID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.ownedInstance(tenantID, id); !ok {
		return ErrNotFound
	}
	// Cascade, mirroring the ON DELETE CASCADE foreign keys in Postgres.
	delete(m.instances, id)
	delete(m.syncs, id)
	delete(m.tokens, id)
	return nil
}

func (m *memStore) GetSyncState(ctx context.Context, tenantID tenancy.TenantID, instanceID string) (SyncState, error) {
	if err := ctx.Err(); err != nil {
		return SyncState{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.ownedInstance(tenantID, instanceID); !ok {
		return SyncState{}, ErrNotFound
	}
	st, ok := m.syncs[instanceID]
	if !ok {
		return SyncState{
			ConnectorInstanceID: instanceID,
			TenantID:            tenantID,
			Phase:               "PENDING",
		}, nil
	}
	return cloneSyncState(st), nil
}

func (m *memStore) SetSyncState(ctx context.Context, tenantID tenancy.TenantID, st SyncState) (SyncState, error) {
	if err := ctx.Err(); err != nil {
		return SyncState{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.ownedInstance(tenantID, st.ConnectorInstanceID); !ok {
		return SyncState{}, ErrNotFound
	}
	st.TenantID = tenantID
	st = cloneSyncState(st)
	m.syncs[st.ConnectorInstanceID] = st
	return cloneSyncState(st), nil
}

func (m *memStore) PutToken(ctx context.Context, tenantID tenancy.TenantID, instanceID string, ciphertext []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.ownedInstance(tenantID, instanceID); !ok {
		return ErrNotFound
	}
	m.tokens[instanceID] = memToken{tenantID: tenantID, ciphertext: slices.Clone(ciphertext)}
	return nil
}

func (m *memStore) GetToken(ctx context.Context, tenantID tenancy.TenantID, instanceID string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	tok, ok := m.tokens[instanceID]
	if !ok || tok.tenantID != tenantID {
		return nil, ErrNotFound
	}
	return slices.Clone(tok.ciphertext), nil
}

func (m *memStore) DeleteToken(ctx context.Context, tenantID tenancy.TenantID, instanceID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	tok, ok := m.tokens[instanceID]
	if !ok || tok.tenantID != tenantID {
		return ErrNotFound
	}
	delete(m.tokens, instanceID)
	return nil
}

func (m *memStore) ListAll(ctx context.Context) ([]ConnectorInstance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ConnectorInstance, 0, len(m.instances))
	for _, inst := range m.instances {
		out = append(out, cloneInstance(inst))
	}
	slices.SortFunc(out, func(a, b ConnectorInstance) int {
		if c := strings.Compare(string(a.TenantID), string(b.TenantID)); c != 0 {
			return c
		}
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

func (m *memStore) CountConnectorInstances(ctx context.Context, tenantID tenancy.TenantID) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, inst := range m.instances {
		if inst.TenantID == tenantID {
			n++
		}
	}
	return n, nil
}

func (m *memStore) PurgeTenant(ctx context.Context, tenantID tenancy.TenantID) (PurgeCounts, error) {
	if err := ctx.Err(); err != nil {
		return PurgeCounts{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var counts PurgeCounts
	for id, inst := range m.instances {
		if inst.TenantID != tenantID {
			continue
		}
		counts.ConnectorInstances++
		if tok, ok := m.tokens[id]; ok && tok.tenantID == tenantID {
			counts.Tokens++
			delete(m.tokens, id)
		}
		delete(m.instances, id)
		delete(m.syncs, id)
	}
	delete(m.tenants, tenantID)
	return counts, nil
}

func (m *memStore) TenantResidue(ctx context.Context, tenantID tenancy.TenantID) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tenants[tenantID]; ok {
		return false, nil
	}
	for _, inst := range m.instances {
		if inst.TenantID == tenantID {
			return false, nil
		}
	}
	for _, tok := range m.tokens {
		if tok.tenantID == tenantID {
			return false, nil
		}
	}
	return true, nil
}

func (m *memStore) usageLocked(tenantID tenancy.TenantID, created time.Time) TenantUsage {
	var instances, paused, docs int64
	for id, inst := range m.instances {
		if inst.TenantID != tenantID {
			continue
		}
		instances++
		if inst.Status == "PAUSED" {
			paused++
		}
		if st, ok := m.syncs[id]; ok {
			docs += st.DocsEmitted
		}
	}
	return TenantUsage{
		TenantID:           tenantID,
		CreatedAt:          created,
		ConnectorInstances: instances,
		DocsEmitted:        docs,
		Suspended:          instances > 0 && paused == instances,
	}
}

func (m *memStore) ListTenants(ctx context.Context, afterTenantID string, limit int) ([]TenantUsage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]tenancy.TenantID, 0, len(m.tenants))
	for id := range m.tenants {
		if string(id) > afterTenantID {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, func(a, b tenancy.TenantID) int { return strings.Compare(string(a), string(b)) })
	out := make([]TenantUsage, 0, len(ids))
	for _, id := range ids {
		if limit > 0 && len(out) >= limit {
			break
		}
		out = append(out, m.usageLocked(id, m.tenants[id]))
	}
	return out, nil
}

func (m *memStore) GetTenantUsage(ctx context.Context, tenantID tenancy.TenantID) (TenantUsage, error) {
	if err := ctx.Err(); err != nil {
		return TenantUsage{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	created, ok := m.tenants[tenantID]
	if !ok {
		return TenantUsage{}, ErrNotFound
	}
	return m.usageLocked(tenantID, created), nil
}

func (m *memStore) SetTenantConnectorStatus(ctx context.Context, tenantID tenancy.TenantID, status string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var changed int64
	for id, inst := range m.instances {
		if inst.TenantID == tenantID && inst.Status != status {
			inst.Status = status
			inst.UpdatedAt = time.Now().UTC()
			m.instances[id] = inst
			changed++
		}
	}
	return changed, nil
}

// ownedInstance returns the instance only when it exists AND belongs to the
// tenant; both misses collapse into a single "not ok". Callers must hold m.mu.
func (m *memStore) ownedInstance(tenantID tenancy.TenantID, id string) (ConnectorInstance, bool) {
	inst, ok := m.instances[id]
	if !ok || inst.TenantID != tenantID {
		return ConnectorInstance{}, false
	}
	return inst, true
}

func cloneInstance(inst ConnectorInstance) ConnectorInstance {
	inst.ConfigJSON = slices.Clone(inst.ConfigJSON)
	return inst
}

func cloneSyncState(st SyncState) SyncState {
	if st.LastSyncStarted != nil {
		t := *st.LastSyncStarted
		st.LastSyncStarted = &t
	}
	if st.LastSyncCompleted != nil {
		t := *st.LastSyncCompleted
		st.LastSyncCompleted = &t
	}
	return st
}
