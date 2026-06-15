package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asker/asker/platform/crypto"
	"github.com/asker/asker/platform/tenancy"
)

// pgStore is the production Store backed by Postgres via pgx/v5. Every
// statement filters on tenant_id; a wrong-tenant row and an absent row are
// both "zero rows" and surface as the same ErrNotFound.
type pgStore struct {
	pool *pgxpool.Pool
}

// Compile-time conformance: the SQL paths are exercised end-to-end in the
// wave-3 compose e2e suite (and in the opt-in TestPGStore integration test).
var _ Store = (*pgStore)(nil)

func newPGStore(pool *pgxpool.Pool) *pgStore { return &pgStore{pool: pool} }

func (s *pgStore) EnsureTenant(ctx context.Context, tenantID tenancy.TenantID) (Tenant, error) {
	// The no-op DO UPDATE makes RETURNING yield a row on both insert and
	// conflict, so first call and repeats are a single round trip.
	const q = `
		INSERT INTO tenants (tenant_id) VALUES ($1)
		ON CONFLICT (tenant_id) DO UPDATE SET tenant_id = excluded.tenant_id
		RETURNING created_at`
	t := Tenant{ID: tenantID}
	if err := s.pool.QueryRow(ctx, q, string(tenantID)).Scan(&t.CreatedAt); err != nil {
		return Tenant{}, fmt.Errorf("ensure tenant: %w", err)
	}
	return t, nil
}

func (s *pgStore) CreateConnectorInstance(ctx context.Context, inst ConnectorInstance, maxInstances int) (ConnectorInstance, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ConnectorInstance{}, fmt.Errorf("create connector instance: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialize concurrent creates for THIS tenant on a per-tenant transaction
	// advisory lock so the count-gated insert below is race-free even under READ
	// COMMITTED (where a bare count subquery in two overlapping transactions
	// could each read "below cap" and both insert). The lock is keyed by a hash
	// of the tenant id and released automatically at commit/rollback; it
	// serializes only same-tenant creates, never cross-tenant ones.
	if maxInstances > 0 {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, string(inst.TenantID)); err != nil {
			return ConnectorInstance{}, fmt.Errorf("create connector instance: lock: %w", err)
		}
	}

	// Implicit EnsureTenant (see Store docs): satisfies the FK without a
	// failure mode for the legitimate first-instance-before-EnsureTenant case.
	if _, err := tx.Exec(ctx,
		`INSERT INTO tenants (tenant_id) VALUES ($1) ON CONFLICT (tenant_id) DO NOTHING`,
		string(inst.TenantID)); err != nil {
		return ConnectorInstance{}, fmt.Errorf("create connector instance: ensure tenant: %w", err)
	}

	// Atomic per-tenant cap (finding M6-#8): the cap is evaluated INSIDE the
	// insert via INSERT .. SELECT gated on the live count, in the same
	// transaction (and behind the per-tenant advisory lock above), so two
	// concurrent creates cannot both pass a check-then-insert and exceed the cap.
	// When the tenant is at/over the cap the SELECT yields no row, the insert
	// writes nothing, and RETURNING comes back empty -> we map that to
	// ErrQuotaExceeded. maxInstances <= 0 disables the cap (the WHERE $6 <= 0
	// short-circuits true).
	const q = `
		INSERT INTO connector_instances (tenant_id, connector_id, display_name, config_json, status)
		SELECT $1, $2, $3, $4::jsonb, $5
		WHERE $6 <= 0
		   OR (SELECT count(*) FROM connector_instances WHERE tenant_id = $1) < $6
		RETURNING id, created_at, updated_at`
	err = tx.QueryRow(ctx, q,
		string(inst.TenantID), inst.ConnectorID, inst.DisplayName, string(inst.ConfigJSON), inst.Status, maxInstances,
	).Scan(&inst.ID, &inst.CreatedAt, &inst.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// The gated SELECT produced no row: the tenant is at or over the cap.
		return ConnectorInstance{}, ErrQuotaExceeded
	}
	if err != nil {
		return ConnectorInstance{}, fmt.Errorf("create connector instance: insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ConnectorInstance{}, fmt.Errorf("create connector instance: commit: %w", err)
	}
	return inst, nil
}

func (s *pgStore) ListConnectorInstances(ctx context.Context, tenantID tenancy.TenantID) ([]ConnectorInstance, error) {
	const q = `
		SELECT id, tenant_id, connector_id, display_name, config_json, status, created_at, updated_at
		FROM connector_instances
		WHERE tenant_id = $1
		ORDER BY created_at, id`
	rows, err := s.pool.Query(ctx, q, string(tenantID))
	if err != nil {
		return nil, fmt.Errorf("list connector instances: %w", err)
	}
	defer rows.Close()

	var out []ConnectorInstance
	for rows.Next() {
		inst, err := scanInstance(rows)
		if err != nil {
			return nil, fmt.Errorf("list connector instances: scan: %w", err)
		}
		out = append(out, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list connector instances: rows: %w", err)
	}
	return out, nil
}

func (s *pgStore) GetConnectorInstance(ctx context.Context, tenantID tenancy.TenantID, id string) (ConnectorInstance, error) {
	const q = `
		SELECT id, tenant_id, connector_id, display_name, config_json, status, created_at, updated_at
		FROM connector_instances
		WHERE id = $1 AND tenant_id = $2`
	inst, err := scanInstance(s.pool.QueryRow(ctx, q, id, string(tenantID)))
	if errors.Is(err, pgx.ErrNoRows) {
		return ConnectorInstance{}, ErrNotFound
	}
	if err != nil {
		return ConnectorInstance{}, fmt.Errorf("get connector instance: %w", err)
	}
	return inst, nil
}

func (s *pgStore) DeleteConnectorInstance(ctx context.Context, tenantID tenancy.TenantID, id string) error {
	// Sync state and token rows go with it via ON DELETE CASCADE.
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM connector_instances WHERE id = $1 AND tenant_id = $2`,
		id, string(tenantID))
	if err != nil {
		return fmt.Errorf("delete connector instance: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *pgStore) GetSyncState(ctx context.Context, tenantID tenancy.TenantID, instanceID string) (SyncState, error) {
	// LEFT JOIN: an instance that has never checkpointed reads as the
	// synthesized PENDING state (proto SyncPhase comment: "created, no sync
	// yet"); only a missing/foreign instance is ErrNotFound.
	const q = `
		SELECT ci.id, ci.tenant_id,
		       COALESCE(ss.cursor, ''), COALESCE(ss.phase, 'PENDING'),
		       ss.last_sync_started, ss.last_sync_completed,
		       COALESCE(ss.last_error, ''), COALESCE(ss.docs_emitted, 0)
		FROM connector_instances ci
		LEFT JOIN sync_states ss ON ss.connector_instance_id = ci.id
		WHERE ci.id = $1 AND ci.tenant_id = $2`
	var st SyncState
	err := s.pool.QueryRow(ctx, q, instanceID, string(tenantID)).Scan(
		&st.ConnectorInstanceID, &st.TenantID, &st.Cursor, &st.Phase,
		&st.LastSyncStarted, &st.LastSyncCompleted, &st.LastError, &st.DocsEmitted)
	if errors.Is(err, pgx.ErrNoRows) {
		return SyncState{}, ErrNotFound
	}
	if err != nil {
		return SyncState{}, fmt.Errorf("get sync state: %w", err)
	}
	return st, nil
}

func (s *pgStore) SetSyncState(ctx context.Context, tenantID tenancy.TenantID, st SyncState) (SyncState, error) {
	// INSERT .. SELECT keyed off the tenant-filtered instance row: when the
	// instance is absent (or owned by another tenant) the SELECT is empty,
	// nothing is written, and zero rows return — ErrNotFound. A conflict can
	// only be with a row that already passed the same tenant filter, so the
	// DO UPDATE arm cannot touch foreign rows.
	const q = `
		INSERT INTO sync_states (connector_instance_id, tenant_id, cursor, phase,
		                         last_sync_started, last_sync_completed, last_error, docs_emitted)
		SELECT ci.id, ci.tenant_id, $3, $4, $5, $6, $7, $8
		FROM connector_instances ci
		WHERE ci.id = $1 AND ci.tenant_id = $2
		ON CONFLICT (connector_instance_id) DO UPDATE SET
			cursor              = excluded.cursor,
			phase               = excluded.phase,
			last_sync_started   = excluded.last_sync_started,
			last_sync_completed = excluded.last_sync_completed,
			last_error          = excluded.last_error,
			docs_emitted        = excluded.docs_emitted
		RETURNING connector_instance_id, tenant_id, cursor, phase,
		          last_sync_started, last_sync_completed, last_error, docs_emitted`
	var out SyncState
	err := s.pool.QueryRow(ctx, q,
		st.ConnectorInstanceID, string(tenantID), st.Cursor, st.Phase,
		st.LastSyncStarted, st.LastSyncCompleted, st.LastError, st.DocsEmitted,
	).Scan(
		&out.ConnectorInstanceID, &out.TenantID, &out.Cursor, &out.Phase,
		&out.LastSyncStarted, &out.LastSyncCompleted, &out.LastError, &out.DocsEmitted)
	if errors.Is(err, pgx.ErrNoRows) {
		return SyncState{}, ErrNotFound
	}
	if err != nil {
		return SyncState{}, fmt.Errorf("set sync state: %w", err)
	}
	return out, nil
}

func (s *pgStore) PutToken(ctx context.Context, tenantID tenancy.TenantID, instanceID string, ciphertext []byte) error {
	// Same tenant-filtered INSERT .. SELECT shape as SetSyncState.
	const q = `
		INSERT INTO tokens (connector_instance_id, tenant_id, ciphertext, updated_at)
		SELECT ci.id, ci.tenant_id, $3, now()
		FROM connector_instances ci
		WHERE ci.id = $1 AND ci.tenant_id = $2
		ON CONFLICT (connector_instance_id) DO UPDATE SET
			ciphertext = excluded.ciphertext,
			updated_at = now()`
	tag, err := s.pool.Exec(ctx, q, instanceID, string(tenantID), ciphertext)
	if err != nil {
		return fmt.Errorf("put token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *pgStore) GetToken(ctx context.Context, tenantID tenancy.TenantID, instanceID string) ([]byte, error) {
	var ciphertext []byte
	err := s.pool.QueryRow(ctx,
		`SELECT ciphertext FROM tokens WHERE connector_instance_id = $1 AND tenant_id = $2`,
		instanceID, string(tenantID)).Scan(&ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}
	return ciphertext, nil
}

func (s *pgStore) DeleteToken(ctx context.Context, tenantID tenancy.TenantID, instanceID string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM tokens WHERE connector_instance_id = $1 AND tenant_id = $2`,
		instanceID, string(tenantID))
	if err != nil {
		return fmt.Errorf("delete token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *pgStore) ListAll(ctx context.Context) ([]ConnectorInstance, error) {
	// Deliberately tenant-UNSCOPED: this backs the scheduler's cross-tenant
	// enumeration only (Store.ListAll contract).
	const q = `
		SELECT id, tenant_id, connector_id, display_name, config_json, status, created_at, updated_at
		FROM connector_instances
		ORDER BY tenant_id, created_at, id`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list all instances: %w", err)
	}
	defer rows.Close()

	var out []ConnectorInstance
	for rows.Next() {
		inst, err := scanInstance(rows)
		if err != nil {
			return nil, fmt.Errorf("list all instances: scan: %w", err)
		}
		out = append(out, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list all instances: rows: %w", err)
	}
	return out, nil
}

func (s *pgStore) CountConnectorInstances(ctx context.Context, tenantID tenancy.TenantID) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM connector_instances WHERE tenant_id = $1`,
		string(tenantID)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count connector instances: %w", err)
	}
	return n, nil
}

// PurgeTenant erases all of the tenant's Postgres rows in one transaction. The
// tokens/sync_states/connector_instances cascade off the tenants row, but we
// DELETE the leaf tables explicitly so the per-table counts are exact for the
// audit log; the final tenants delete then removes the root. tenant_deks is
// intentionally NOT touched here (the caller crypto-shreds it).
func (s *pgStore) PurgeTenant(ctx context.Context, tenantID tenancy.TenantID) (PurgeCounts, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PurgeCounts{}, fmt.Errorf("purge tenant: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var counts PurgeCounts
	tokTag, err := tx.Exec(ctx, `DELETE FROM tokens WHERE tenant_id = $1`, string(tenantID))
	if err != nil {
		return PurgeCounts{}, fmt.Errorf("purge tenant: delete tokens: %w", err)
	}
	counts.Tokens = tokTag.RowsAffected()

	if _, err := tx.Exec(ctx, `DELETE FROM sync_states WHERE tenant_id = $1`, string(tenantID)); err != nil {
		return PurgeCounts{}, fmt.Errorf("purge tenant: delete sync states: %w", err)
	}
	// Personalization (v3.2): deleted explicitly so erasure is exact even though
	// these also cascade off the tenants row below. Not counted in PurgeCounts
	// (same treatment as sync_states); the residue check verifies emptiness.
	for table, label := range map[string]string{
		"feedback_events":  "feedback events",
		"learned_weights":  "learned weights",
		"user_preferences": "preferences",
	} {
		if _, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE tenant_id = $1`, string(tenantID)); err != nil {
			return PurgeCounts{}, fmt.Errorf("purge tenant: delete %s: %w", label, err)
		}
	}
	ciTag, err := tx.Exec(ctx, `DELETE FROM connector_instances WHERE tenant_id = $1`, string(tenantID))
	if err != nil {
		return PurgeCounts{}, fmt.Errorf("purge tenant: delete connector instances: %w", err)
	}
	counts.ConnectorInstances = ciTag.RowsAffected()

	if _, err := tx.Exec(ctx, `DELETE FROM tenants WHERE tenant_id = $1`, string(tenantID)); err != nil {
		return PurgeCounts{}, fmt.Errorf("purge tenant: delete tenant row: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return PurgeCounts{}, fmt.Errorf("purge tenant: commit: %w", err)
	}
	return counts, nil
}

// TenantResidue returns empty=true when no tenant-owned row survives across
// tenants, connector_instances, sync_states and tokens.
func (s *pgStore) TenantResidue(ctx context.Context, tenantID tenancy.TenantID) (bool, error) {
	const q = `
		SELECT
			EXISTS (SELECT 1 FROM tenants WHERE tenant_id = $1)
			OR EXISTS (SELECT 1 FROM connector_instances WHERE tenant_id = $1)
			OR EXISTS (SELECT 1 FROM sync_states WHERE tenant_id = $1)
			OR EXISTS (SELECT 1 FROM tokens WHERE tenant_id = $1)
			OR EXISTS (SELECT 1 FROM user_preferences WHERE tenant_id = $1)
			OR EXISTS (SELECT 1 FROM learned_weights WHERE tenant_id = $1)
			OR EXISTS (SELECT 1 FROM feedback_events WHERE tenant_id = $1)`
	var anyResidue bool
	if err := s.pool.QueryRow(ctx, q, string(tenantID)).Scan(&anyResidue); err != nil {
		return false, fmt.Errorf("tenant residue: %w", err)
	}
	return !anyResidue, nil
}

func (s *pgStore) ListTenants(ctx context.Context, afterTenantID string, limit int) ([]TenantUsage, error) {
	// Keyset pagination on tenant_id (the PK), with a LEFT JOIN aggregate over
	// the tenant's connector instances for the summary usage.
	const q = `
		SELECT t.tenant_id, t.created_at,
		       count(ci.id) AS instances,
		       COALESCE(sum(ss.docs_emitted), 0) AS docs,
		       count(ci.id) > 0 AND count(ci.id) FILTER (WHERE ci.status = 'PAUSED') = count(ci.id) AS suspended
		FROM tenants t
		LEFT JOIN connector_instances ci ON ci.tenant_id = t.tenant_id
		LEFT JOIN sync_states ss ON ss.connector_instance_id = ci.id
		WHERE t.tenant_id > $1
		GROUP BY t.tenant_id, t.created_at
		ORDER BY t.tenant_id
		LIMIT $2`
	rows, err := s.pool.Query(ctx, q, afterTenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	defer rows.Close()
	var out []TenantUsage
	for rows.Next() {
		u, err := scanUsage(rows)
		if err != nil {
			return nil, fmt.Errorf("list tenants: scan: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tenants: rows: %w", err)
	}
	return out, nil
}

func (s *pgStore) GetTenantUsage(ctx context.Context, tenantID tenancy.TenantID) (TenantUsage, error) {
	const q = `
		SELECT t.tenant_id, t.created_at,
		       count(ci.id) AS instances,
		       COALESCE(sum(ss.docs_emitted), 0) AS docs,
		       count(ci.id) > 0 AND count(ci.id) FILTER (WHERE ci.status = 'PAUSED') = count(ci.id) AS suspended
		FROM tenants t
		LEFT JOIN connector_instances ci ON ci.tenant_id = t.tenant_id
		LEFT JOIN sync_states ss ON ss.connector_instance_id = ci.id
		WHERE t.tenant_id = $1
		GROUP BY t.tenant_id, t.created_at`
	u, err := scanUsage(s.pool.QueryRow(ctx, q, string(tenantID)))
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantUsage{}, ErrNotFound
	}
	if err != nil {
		return TenantUsage{}, fmt.Errorf("get tenant usage: %w", err)
	}
	return u, nil
}

func (s *pgStore) SetTenantConnectorStatus(ctx context.Context, tenantID tenancy.TenantID, status string) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE connector_instances SET status = $2, updated_at = now() WHERE tenant_id = $1 AND status <> $2`,
		string(tenantID), status)
	if err != nil {
		return 0, fmt.Errorf("set tenant connector status: %w", err)
	}
	return tag.RowsAffected(), nil
}

// --- Personalization (v3.2) -------------------------------------------------

func (s *pgStore) GetPersonalization(ctx context.Context, tenantID tenancy.TenantID) (UserPreferences, LearnedWeights, error) {
	var prefs UserPreferences
	err := s.pool.QueryRow(ctx,
		`SELECT profile_json::text, version FROM user_preferences WHERE tenant_id = $1`,
		string(tenantID)).Scan(&prefs.ProfileJSON, &prefs.Version)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No saved profile is normal (cold start), not an error.
	case err != nil:
		return UserPreferences{}, LearnedWeights{}, fmt.Errorf("get preferences: %w", err)
	default:
		prefs.Exists = true
	}

	var weights LearnedWeights
	err = s.pool.QueryRow(ctx,
		`SELECT weights_json::text, sample_count FROM learned_weights WHERE tenant_id = $1`,
		string(tenantID)).Scan(&weights.WeightsJSON, &weights.SampleCount)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return UserPreferences{}, LearnedWeights{}, fmt.Errorf("get learned weights: %w", err)
	default:
		weights.Exists = true
	}
	return prefs, weights, nil
}

func (s *pgStore) PutPreferences(ctx context.Context, tenantID tenancy.TenantID, profileJSON string) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("put preferences: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Implicit EnsureTenant: the FK requires the tenant row, and the identity is
	// always a verified JWT (same rationale as CreateConnectorInstance).
	if _, err := tx.Exec(ctx,
		`INSERT INTO tenants (tenant_id) VALUES ($1) ON CONFLICT (tenant_id) DO NOTHING`,
		string(tenantID)); err != nil {
		return 0, fmt.Errorf("put preferences: ensure tenant: %w", err)
	}

	// Upsert, bumping version monotonically: a new row starts at 1, an existing
	// one increments. The bumped version is what the query result-cache key
	// folds in, so a preference change invalidates cached orders.
	const q = `
		INSERT INTO user_preferences (tenant_id, profile_json, version, updated_at)
		VALUES ($1, $2::jsonb, 1, now())
		ON CONFLICT (tenant_id) DO UPDATE SET
			profile_json = excluded.profile_json,
			version      = user_preferences.version + 1,
			updated_at   = now()
		RETURNING version`
	var version int64
	if err := tx.QueryRow(ctx, q, string(tenantID), profileJSON).Scan(&version); err != nil {
		return 0, fmt.Errorf("put preferences: upsert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("put preferences: commit: %w", err)
	}
	return version, nil
}

func (s *pgStore) AppendFeedback(ctx context.Context, tenantID tenancy.TenantID, ev FeedbackEvent) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO tenants (tenant_id) VALUES ($1) ON CONFLICT (tenant_id) DO NOTHING`,
		string(tenantID)); err != nil {
		return fmt.Errorf("append feedback: ensure tenant: %w", err)
	}
	const q = `
		INSERT INTO feedback_events (tenant_id, doc_id, doc_type, connector_id, action, dwell_ms, query)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`
	if _, err := s.pool.Exec(ctx, q,
		string(tenantID), ev.DocID, ev.DocType, ev.ConnectorID, ev.Action, ev.DwellMs, ev.Query); err != nil {
		return fmt.Errorf("append feedback: %w", err)
	}
	return nil
}

func (s *pgStore) UpdateLearnedWeights(ctx context.Context, tenantID tenancy.TenantID, update func(curJSON string, curSamples int64) (string, int64, error)) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("update learned weights: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`INSERT INTO tenants (tenant_id) VALUES ($1) ON CONFLICT (tenant_id) DO NOTHING`,
		string(tenantID)); err != nil {
		return fmt.Errorf("update learned weights: ensure tenant: %w", err)
	}
	// Materialize the row (default when absent) so the FOR UPDATE lock has
	// something to hold; the lock serializes concurrent feedback for THIS tenant
	// so the read-modify-write below cannot lose an update (TOCTOU).
	if _, err := tx.Exec(ctx,
		`INSERT INTO learned_weights (tenant_id) VALUES ($1) ON CONFLICT (tenant_id) DO NOTHING`,
		string(tenantID)); err != nil {
		return fmt.Errorf("update learned weights: ensure row: %w", err)
	}

	var curJSON string
	var curSamples int64
	if err := tx.QueryRow(ctx,
		`SELECT weights_json::text, sample_count FROM learned_weights WHERE tenant_id = $1 FOR UPDATE`,
		string(tenantID)).Scan(&curJSON, &curSamples); err != nil {
		return fmt.Errorf("update learned weights: lock: %w", err)
	}

	newJSON, newSamples, err := update(curJSON, curSamples)
	if err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		`UPDATE learned_weights SET weights_json = $2::jsonb, sample_count = $3, updated_at = now() WHERE tenant_id = $1`,
		string(tenantID), newJSON, newSamples); err != nil {
		return fmt.Errorf("update learned weights: write: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("update learned weights: commit: %w", err)
	}
	return nil
}

func (s *pgStore) ResetLearning(ctx context.Context, tenantID tenancy.TenantID) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("reset learning: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM learned_weights WHERE tenant_id = $1`, string(tenantID)); err != nil {
		return 0, fmt.Errorf("reset learning: delete weights: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM feedback_events WHERE tenant_id = $1`, string(tenantID))
	if err != nil {
		return 0, fmt.Errorf("reset learning: delete feedback: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("reset learning: commit: %w", err)
	}
	return tag.RowsAffected(), nil
}

// scanUsage scans one tenant-usage aggregate row.
func scanUsage(row pgx.Row) (TenantUsage, error) {
	var u TenantUsage
	err := row.Scan(&u.TenantID, &u.CreatedAt, &u.ConnectorInstances, &u.DocsEmitted, &u.Suspended)
	return u, err
}

// scanInstance scans one connector_instances row in the canonical column
// order used by Get and List.
func scanInstance(row pgx.Row) (ConnectorInstance, error) {
	var inst ConnectorInstance
	err := row.Scan(&inst.ID, &inst.TenantID, &inst.ConnectorID, &inst.DisplayName,
		&inst.ConfigJSON, &inst.Status, &inst.CreatedAt, &inst.UpdatedAt)
	return inst, err
}

// pgDEKStore stores KEK-wrapped tenant DEKs in the tenant_deks table. It is
// the durable crypto.DEKStore behind the token vault's TenantCipher.
type pgDEKStore struct {
	pool *pgxpool.Pool
}

var _ crypto.DEKStore = (*pgDEKStore)(nil)

func newPGDEKStore(pool *pgxpool.Pool) *pgDEKStore { return &pgDEKStore{pool: pool} }

func (s *pgDEKStore) GetWrappedDEK(ctx context.Context, tenantID tenancy.TenantID) ([]byte, error) {
	if tenantID == "" {
		return nil, tenancy.ErrNoTenant
	}
	var wrapped []byte
	err := s.pool.QueryRow(ctx,
		`SELECT wrapped_dek FROM tenant_deks WHERE tenant_id = $1`,
		string(tenantID)).Scan(&wrapped)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, crypto.ErrDEKNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get wrapped DEK: %w", err)
	}
	return wrapped, nil
}

// PutWrappedDEK is first-writer-wins per the crypto.DEKStore contract: an
// existing wrapped DEK is NEVER replaced (overwriting would orphan every
// ciphertext encrypted under it). ON CONFLICT DO NOTHING + nil return is one
// of the two behaviors TenantCipher tolerates (it re-reads after every Put).
func (s *pgDEKStore) PutWrappedDEK(ctx context.Context, tenantID tenancy.TenantID, wrapped []byte) error {
	if tenantID == "" {
		return tenancy.ErrNoTenant
	}
	if len(wrapped) == 0 {
		return errors.New("control-plane: refusing to store empty wrapped DEK")
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO tenant_deks (tenant_id, wrapped_dek) VALUES ($1, $2)
		 ON CONFLICT (tenant_id) DO NOTHING`,
		string(tenantID), wrapped)
	if err != nil {
		return fmt.Errorf("put wrapped DEK: %w", err)
	}
	return nil
}

// Delete crypto-shreds the tenant's wrapped DEK (the GDPR primitive): with the
// wrapped DEK gone and the KEK never persisting the unwrapped key, every blob/
// token ciphertext sealed under it becomes permanently unreadable. Idempotent:
// deleting an absent row returns nil so a retried erasure converges.
func (s *pgDEKStore) Delete(ctx context.Context, tenantID tenancy.TenantID) error {
	if tenantID == "" {
		return tenancy.ErrNoTenant
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM tenant_deks WHERE tenant_id = $1`, string(tenantID)); err != nil {
		return fmt.Errorf("delete wrapped DEK: %w", err)
	}
	return nil
}
