# ADR-002: tenant_id derivation from the verified token

## Status

Accepted (M0).

## Context

Every data access in Asker must be scoped to exactly one tenant; the spec makes
cross-tenant leakage the cardinal failure. That means `tenant_id` must come from something the
server verifies, not anything the client asserts. The gateway validates OIDC access tokens
issued by Keycloak (signature via JWKS, issuer, audience, expiry), so verified JWT claims are
the only trustworthy per-request identity material.

Two candidate claims exist: `sub` (always present, unique per user in the realm) and a
custom `tenant_id` claim (not issued today, but the natural vehicle for organization-level
tenancy later, where many users map to one tenant).

## Decision

```
tenant_id := claims["tenant_id"]  if the claim is PRESENT (must then be valid — no fallback)
             claims["sub"]        only when "tenant_id" is entirely absent
```

derived **only from the verified token** — never from the request body, query string, or
headers. The derivation is implemented once, in `platform/tenancy` (`FromClaims`), which is
the only way to construct a tenant context; the gateway calls it after JWT verification and
attaches the result to the request context.

The chosen claim must be a string that, after whitespace trimming, matches the tenant
syntax rule: `^[A-Za-z0-9._-]{1,128}$`, excluding the path segments `.` and `..` (Keycloak
subject UUIDs always satisfy this). Tenant IDs end up embedded in Vespa group selectors,
object-store key prefixes, and Kafka message keys, so separators, control characters,
non-ASCII (including zero-width characters), and unbounded lengths are rejected at the
chokepoint. Anything invalid is an error: a **present-but-invalid `tenant_id` claim fails
closed (`ErrInvalidTenant`) and never falls back to `sub`** — falling back would let a
misconfigured IdP mapper silently re-route a user onto a different tenant. When neither
claim exists the result is `ErrNoTenant`; there is no anonymous or default tenant.

## Consequences

- **Per-user tenancy now, for free:** with no `tenant_id` claim issued, every user (`sub`) is
  their own tenant, which is exactly the V1 product model.
- **Org tenancy later, without code changes at the chokepoint:** mapping users to a shared
  tenant becomes an identity-provider configuration (issue a `tenant_id` claim via a Keycloak
  protocol mapper), not an application rewrite. The claim takes precedence by construction.
- **Keycloak is the tenant registry for now.** Tenant existence and user→tenant mapping live
  in the realm. When the control plane gains its own tenant table (M1+), it must treat the
  token-derived value as authoritative.
- Clients cannot influence tenant selection: a forged header or body field is simply ignored,
  and a forged token fails signature verification at the gateway.
- The fallback means `sub` values become long-lived tenant identifiers; migrating a user
  between Keycloak realms or IdPs would change their `sub` and therefore orphan their data —
  any such migration must include a tenant-rename procedure.
