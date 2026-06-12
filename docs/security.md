# Security

Hard requirement: **strict per-user data isolation — a user can NEVER see another user's
data.** This document covers how tenancy is enforced, where the M0 dev stack deliberately
falls short of the production posture, and the planned GDPR delete flow.

## Tenancy model

- **`tenant_id` derives only from the verified OIDC token.** The gateway validates the JWT
  (signature against Keycloak's JWKS, issuer, audience, expiry) and then derives the tenant:
  the `tenant_id` claim when that claim is present (a present-but-invalid value fails closed
  and never falls back), else `sub`. The chosen value must match a strict syntax allowlist
  (`[A-Za-z0-9._-]`, max 128 chars, no `.`/`..`) because tenant IDs are embedded in storage
  keys and query selectors downstream. Nothing from the request body, query string, or
  headers ever influences tenant selection. Rationale and consequences in
  [ADR-002](adr/ADR-002-tenant-id-derivation.md).
- **`platform/tenancy` is the single chokepoint.** It is the only way to construct a tenant
  context (`tenancy.FromClaims` → `tenancy.WithContext`), and downstream code can only read
  it back via `tenancy.FromContext` / `MustFromContext`. The `Context` type has unexported
  fields, so a tenant context cannot be forged outside the package. Data-access code takes a
  tenant context, not a raw string — querying without a tenant is a compile-time or
  fail-closed error, not a code-review catch. The spec mandates 100% test coverage on this
  package.
- **Tenant scoping propagates through every layer** (from M1): Kafka messages are keyed by
  `tenant_id`, every internal RPC carries it, and every Vespa query must include the tenant
  group selector (`streaming.groupname=<tenant>`). Vespa streaming mode then physically scopes
  the scan to that tenant's document group.
- **The cross-tenant leakage test suite (M1) is sacred:** it attempts every API with
  mismatched tenant tokens and must prove isolation before M1 closes.

## Dev vs prod gap

The M0 compose stack is intentionally insecure in ways that are acceptable only on a developer
machine or CI runner. Every gap below has a planned closure.

| Area | Dev (M0 compose) | Production target | Closes in |
| :--- | :--- | :--- | :--- |
| Transport | HTTP only, no TLS anywhere | TLS for external and internal traffic | M4 |
| Admin/user credentials | Hardcoded dev-only passwords (Keycloak `admin/admin`, users `password123`, MinIO/Postgres static creds) | Provisioned secrets, no defaults | M4 |
| Secrets management | Plaintext env vars in compose files | HashiCorp Vault | M4 |
| OAuth token storage (connector tokens) | n/a — no connectors yet | Encrypted token vault in Postgres | M1 (upload/Gmail) – M2 (full hub) |
| Blob encryption | n/a — nothing stored yet | Per-tenant envelope encryption: per-tenant DEK wrapped by a KEK; KEK in Vault (prod) / file-based dev shim with the same interface | M1 |
| Network posture | Flat compose network, all ports published to localhost | Default-deny network policies in K8s | M4 |
| Keycloak | `start-dev` mode, imported dev realm | Production mode, managed realm config | M4 |
| Rate limiting / quotas | None | Per-tenant + global token buckets in Redis; quota/abuse controls | M1 (gateway) / M6 (abuse) |
| Security review | None | Pen-test-style checklist: authz matrix, SSRF in connector fetchers, token vault, injection | M6 |

No secrets in code, ever — the dev credentials above live only in compose/realm files and are
clearly labeled dev-only.

## GDPR delete flow (planned)

Full tenant erasure is a first-class, verifiable operation (drilled in M6):

1. A delete job is recorded in Postgres (control plane) — durable, resumable, auditable.
2. Connectors/control plane emit **tombstones** for the tenant's documents; index writers and
   blob GC honor tombstones within the 30-minute SLA (mechanism exists from M1).
3. One command wipes the tenant from every store:
   - **Postgres** — tenant row, connector configs, sync cursors, encrypted OAuth tokens;
   - **Vespa** — delete the tenant's document group;
   - **MinIO** — delete the tenant's blobs (and destroy the tenant DEK, rendering any stragglers
     unreadable);
   - **Kafka** — tombstones replayed so downstream consumers converge.
4. A **verification job** re-checks every store and the search path to confirm zero residual
   data before the job is marked complete.
