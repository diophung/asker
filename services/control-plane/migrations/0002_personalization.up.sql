-- Personalization schema (v3.2): per-tenant preference profile, the learned
-- ranking model, and the behavioral feedback log.
--
-- Every table is keyed/scoped by tenant_id and REFERENCES tenants (tenant_id)
-- ON DELETE CASCADE, so the existing GDPR delete cascade (the final
-- `DELETE FROM tenants`) erases personalization data automatically; the
-- delete-verification residue check (store_pg.go TenantResidue) is extended to
-- cover these tables. profile_json / weights_json are opaque blobs owned by the
-- platform/personalization library; Postgres only stores and versions them.

-- One resolved UserPreferenceProfile per tenant, monotonically versioned so a
-- preference change invalidates the per-tenant query result cache.
CREATE TABLE user_preferences (
    tenant_id    text PRIMARY KEY REFERENCES tenants (tenant_id) ON DELETE CASCADE,
    profile_json jsonb NOT NULL,
    version      bigint NOT NULL DEFAULT 1,
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- The online learning-to-rank model (logistic coefficients), one row per tenant.
CREATE TABLE learned_weights (
    tenant_id    text PRIMARY KEY REFERENCES tenants (tenant_id) ON DELETE CASCADE,
    weights_json jsonb NOT NULL DEFAULT '{}',
    sample_count bigint NOT NULL DEFAULT 0,
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- Append-only behavioral feedback log (implicit signals). Retained for analytics
-- and "reset what you've learned about me"; the model is updated online so a
-- read of this table is not on the ranking hot path.
CREATE TABLE feedback_events (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    text NOT NULL REFERENCES tenants (tenant_id) ON DELETE CASCADE,
    doc_id       text NOT NULL DEFAULT '',
    doc_type     text NOT NULL DEFAULT '',
    connector_id text NOT NULL DEFAULT '',
    action       text NOT NULL DEFAULT '',
    dwell_ms     bigint NOT NULL DEFAULT 0,
    query        text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX feedback_events_tenant_id_idx ON feedback_events (tenant_id);
