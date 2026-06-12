-- Control-plane schema (M1): tenant-scoped metadata.
--
-- Every row carries tenant_id and every query in store_pg.go filters on it;
-- a row reached with the wrong tenant is indistinguishable from an absent row
-- (no existence oracle). Token ciphertext is envelope-encrypted by
-- platform/crypto BEFORE it reaches the tokens table; tenant_deks holds only
-- KEK-wrapped DEKs, never key plaintext.

CREATE TABLE tenants (
    tenant_id  text PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE connector_instances (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    text NOT NULL REFERENCES tenants (tenant_id) ON DELETE CASCADE,
    connector_id text NOT NULL,
    display_name text NOT NULL DEFAULT '',
    config_json  jsonb NOT NULL DEFAULT '{}',
    status       text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX connector_instances_tenant_id_idx ON connector_instances (tenant_id);

CREATE TABLE sync_states (
    connector_instance_id uuid PRIMARY KEY REFERENCES connector_instances (id) ON DELETE CASCADE,
    tenant_id             text NOT NULL,
    cursor                text NOT NULL DEFAULT '',
    phase                 text NOT NULL,
    last_sync_started     timestamptz,
    last_sync_completed   timestamptz,
    last_error            text NOT NULL DEFAULT '',
    docs_emitted          bigint NOT NULL DEFAULT 0
);

CREATE INDEX sync_states_tenant_id_idx ON sync_states (tenant_id);

CREATE TABLE tokens (
    connector_instance_id uuid PRIMARY KEY REFERENCES connector_instances (id) ON DELETE CASCADE,
    tenant_id  text NOT NULL,
    ciphertext bytea NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX tokens_tenant_id_idx ON tokens (tenant_id);

CREATE TABLE tenant_deks (
    tenant_id   text PRIMARY KEY,
    wrapped_dek bytea NOT NULL
);
