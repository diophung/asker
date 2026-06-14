# Security

Hard requirement: **strict per-user data isolation — a user can NEVER see another user's
data.** This document is the M6 security sign-off: the threat model (assets, trust boundaries,
attacker classes), the authorization matrix (every external and internal surface mapped to
who-may-call-it, how authz is enforced, and the residual risk), the result of the M6
adversarial security audit, and the GDPR delete + crypto-shred guarantees. Tenancy enforcement
and the dev-vs-prod gap follow.

THE SACRED INVARIANT, restated so it cannot be forgotten: **no operation may ever cross
tenants; `tenant_id` comes ONLY from the verified OIDC token; `platform/tenancy` is the single
chokepoint; the cross-tenant leakage suite (`tools/e2e/leakage.sh`, `make e2e-leakage`) must
hold.** Every control below exists to keep that true under attack.

## 1. Threat model

### 1.1 Assets (what we protect, in priority order)

| # | Asset | Why it matters | Where it lives |
| :-- | :--- | :--- | :--- |
| A1 | **A tenant's indexed corpus** (documents, chunks, embeddings, snippets) | The product is the user's private data; a cross-tenant read is the worst outcome | Vespa streaming group keyed by `tenant_id` |
| A2 | **The tenant DEK** (per-tenant AES-256 data-encryption key) | Decrypts that tenant's blobs and OAuth tokens; destroying it crypto-shreds the tenant | Postgres `tenant_deks` (wrapped), unwrapped only in service memory / Vault |
| A3 | **The KEK** (key-encryption key that wraps every DEK) | Compromise unwraps every tenant's DEK at once | File shim (dev) / **HashiCorp Vault Transit** (prod, ADR-015) — never on disk in prod |
| A4 | **Connector OAuth tokens** (bearer credentials to Gmail/Drive/Slack/…) | Live access to the upstream source; a leaked token is an account takeover of the source | Postgres token vault, encrypted under the tenant DEK (A2) |
| A5 | **Tenant blobs** (originals, thumbnails, keyframes) | The raw user data behind the index | MinIO/S3, envelope-encrypted under the tenant DEK (A2) |
| A6 | **Control-plane metadata** (tenants, connector configs, sync cursors, quotas, delete jobs) | Reveals/controls a tenant's footprint; the admin surface acts on it | Postgres |
| A7 | **Cloud-instance credentials** (IMDS, `169.254.169.254`) | An SSRF that reaches IMDS mints IAM credentials → cluster/cloud takeover | Outside Asker, reachable from any pod with egress |

### 1.2 Trust boundaries

```
                    UNTRUSTED INTERNET
   ┌───────────────────────────────────────────────────────────────┐
   │  attacker-controlled clients, OIDC tokens, connector config,    │
   │  upstream-source webhook payloads, redirect targets             │
   └───────────────────────────────┬───────────────────────────────┘
                                    │  TLS (M4 edge) + OIDC JWT
   ════════════════════════════════╪═══════════════ BOUNDARY 1 (authn) ═══
                                    ▼
                         ┌──────────────────┐
                         │  GATEWAY          │  the ONLY place a JWT is
                         │  (OIDC authn,     │  verified and tenant_id is
                         │  pre-auth throttle,│ derived (ADR-002). All
                         │  per-tenant limit, │ external surface terminates
                         │  admin-role authz) │ here.
                         └────────┬──────────┘
                                  │ gRPC + x-asker-tenant metadata
   ═══════════════════════════════╪══════════════ BOUNDARY 2 (internal trust zone) ═══
                                  ▼   (ADR-009; M4 NetworkPolicy + scaffolded mTLS, ADR-016)
   ┌───────────────────────────────────────────────────────────────┐
   │  INTERNAL gRPC / Kafka TRUST ZONE                               │
   │  query · control-plane · connector-hub · ingest · enrich ·     │
   │  index-writer · clip — trust x-asker-tenant BECAUSE only        │
   │  gateway/hub (which derive it from a verified JWT) can reach    │
   │  them, enforced by default-deny NetworkPolicy.                  │
   └───────────────┬───────────────────────────────┬───────────────┘
                   │                                │
   ════════════════╪════ BOUNDARY 3 (crypto) ═══════╪═══ BOUNDARY 4 (SSRF egress) ═══
                   ▼                                ▼
            ┌────────────┐                 ┌────────────────────────┐
            │  VAULT KEK  │ wrap/unwrap     │  connector-hub egress   │
            │  (A3)       │ only; KEK never │  to ARBITRARY tenant-   │
            │             │ leaves Vault    │  configured URLs (A7)   │
            └────────────┘                 │  → platform/safehttp    │
                                           │    connect-time IP guard│
                                           └────────────────────────┘
```

- **Boundary 1 — authentication.** The internet/client boundary. Crossed only with a Keycloak
  JWT whose signature, issuer, audience, and expiry the gateway verifies. `tenant_id` is
  derived from the verified claims *after* this boundary and never before (ADR-002).
- **Boundary 2 — the internal trust zone.** Internal services trust the `x-asker-tenant` gRPC
  metadata (and the Kafka `tenant_id` header) *because only the gateway and connector-hub can
  reach them*. In M0–M3 this was **topological** (closed compose network); M4 makes it
  **enforced** with default-deny NetworkPolicies (ADR-016). `tenancygrpc` still re-validates
  the metadata against the tenant syntax allowlist and fails closed.
- **Boundary 3 — the crypto boundary.** The KEK (A3) is the highest-value secret. In prod it
  never leaves Vault: only wrap/unwrap round-trips cross the wire (ADR-015). The per-tenant
  binding (GCM AAD for the file KEK, Transit derived-key context for Vault) makes a DEK that is
  moved to another tenant's row cryptographically fail to unwrap.
- **Boundary 4 — the SSRF egress boundary.** connector-hub is the one service that fetches
  arbitrary, tenant-controlled URLs server-side. That outbound traffic is the SSRF surface
  (A7): it must never reach loopback, link-local/IMDS, RFC1918/ULA, or cluster-internal
  services. Defended in depth — `platform/safehttp` at the application layer plus the M4
  NetworkPolicy egress restriction (§4). The app-layer guard decides on the **resolved IP at
  connect time** (`net.Dialer.Control`, closing the DNS-rebinding TOCTOU), re-checks every redirect
  hop, disables proxy-env tunnelling, and blocks not just the obvious ranges but also **NAT64 /
  IPv4-embedded IPv6** (`64:ff9b::/96`, so an IMDS dial via `64:ff9b::a9fe:a9fe` is caught in
  DNS64/NAT64 clusters), **CGNAT `100.64.0.0/10`**, and IETF-reserved ranges — even when the dev
  `AllowPrivate` relaxation is set, the IMDS address and these ranges stay blocked.

### 1.3 Attacker classes

| Class | Capability | Primary goal | Primary defense |
| :--- | :--- | :--- | :--- |
| **AC1 — malicious tenant** | A valid Keycloak account; can call every `/v1/*` route, craft request bodies, configure connectors with attacker-chosen URLs, upload arbitrary blobs, and forge any value in a request *except* the verified token | Read or delete another tenant's data (A1/A5); reach IMDS via a connector (A7); exhaust shared resources | tenant_id only from the token (ADR-002), `platform/tenancy` chokepoint, Vespa group scoping, `platform/safehttp` SSRF guard, per-tenant quota + rate limit |
| **AC2 — compromised internal pod** | Code execution inside one pod in the internal trust zone; can dial whatever the NetworkPolicy allows and assert any `x-asker-tenant` value | Pivot to other tenants' data or to the KEK | default-deny NetworkPolicy (least-privilege call graph, ADR-016), KEK in Vault not memory (A3), per-tenant crypto binding, scaffolded mTLS for peer identity |
| **AC3 — network attacker** | On-path or able to reach a published port; unauthenticated | Flood the auth/crypto path (DoS), sniff internal traffic, hit IMDS through an SSRF amplifier | pre-auth throttle (§3.3), TLS at the edge (M4), NetworkPolicy, JWKS-fetch hardening |

Out of scope for V1 (accepted, see §6): a malicious Keycloak realm operator (Asker trusts the
IdP); a host-root compromise of a node; side-channels in Vespa streaming-mode scan timing.

## 2. Authorization matrix

Every surface, who may call it, how authorization is enforced (not merely *authentication*),
and the residual risk. "Caller tenant" = the tenant derived from the verified token, the only
trusted source. Route paths are exactly as wired in `services/gateway/routes.go`; RPC names are
exactly as in `platform/proto/asker/controlplane/v1/controlplane.proto`.

### 2.1 Gateway HTTP routes (external; Boundary 1)

| Route | Who may call | Authz enforcement | Residual risk |
| :--- | :--- | :--- | :--- |
| `GET /healthz`, `GET /readyz` | anyone (operator/liveness) | none — no tenant context, no data | none (status only) |
| `GET /metrics` | operator / Prometheus | none by design; low-cardinality, **no tenant/doc/query labels** (ADR-017 §1) | metrics surface is intentionally unauthenticated (refuted finding, §5) — carries no tenant data |
| `GET /v1/me` | any authenticated user | auth middleware; returns the caller's own tenant/subject/email from the token | none |
| `DELETE /v1/me/data` | any authenticated user | auth middleware; tenant from token only; gateway fills the irreversible-action `confirm` from the verified context (`me.go`) — **no body field selects a tenant** | a user can erase only their OWN tenant; cross-tenant erasure is the admin path (§2.2) |
| `GET /v1/search` | any authenticated user | auth + per-tenant rate limit; query scoped to the caller's Vespa streaming group | `MAX_QUERY_CHARS` (default 1024) caps query size |
| `GET /v1/media` | any authenticated user | auth; thumbnail/keyframe proxy scoped to caller tenant; `MAX_MEDIA_MB` (default 25) caps bytes | none |
| `GET/POST /v1/connectors`, `DELETE /v1/connectors/{id}`, `PUT /v1/connectors/{id}/token` | any authenticated user | auth; all CRUD scoped to caller tenant; create is bounded by the per-tenant connector-instance **quota** (§3.1) | a tenant manages only its own connectors |
| `POST /v1/upload` | any authenticated user | auth; blob written under caller tenant prefix; `MAX_UPLOAD_MB` (default 32) caps size | none |
| `GET /v1/admin/tenants` | **admin only** | auth **+ `requireAdmin`**: a distinct `asker-admin` realm/client role or `asker:admin` scope claim (`admin.go`); a non-admin authenticated user gets **403**, not 404 | the admin surface existing is not a secret; only acting on a tenant must not leak |
| `GET/DELETE /v1/admin/tenants/{tenant}` | **admin only** | auth + `requireAdmin`; the `{tenant}` path value is validated against the tenant syntax allowlist before any store call | admin can read usage / erase any tenant **by design** (operator takedown / right-to-erasure on behalf of a user); audit-logged at the control plane |
| `POST /v1/admin/tenants/{tenant}/suspend` | **admin only** | auth + `requireAdmin` | pauses/resumes all of a target tenant's connectors; audit-logged |

The admin gate is **authorization, not authentication**: `isAdmin` (`services/gateway/admin.go`)
reads the `asker-admin` role from `realm_access.roles` or `resource_access.<aud>.roles`, or the
`asker:admin` scope from the space-separated `scope` claim. The claim is server-issued by
Keycloak (realm-role / scope mapper) — a user can never self-assert it, the same trust basis as
the tenant claim (ADR-002).

### 2.2 control-plane gRPC (internal; Boundary 2)

`ControlPlaneService` RPCs (`EnsureTenant`, `CreateConnectorInstance`, `ListConnectorInstances`,
`GetSyncState`, `SetSyncState`, `PutToken`, `GetToken`, `DeleteConnectorInstance`,
`DeleteTenant`, …) all derive the tenant from the verified `x-asker-tenant` metadata via the
**fail-closed** `tenancygrpc.UnaryServerInterceptor` and re-extract it with `callerTenant(ctx)`;
the tenant is **never** a request field. Every store call is scoped to it.

| RPC | Who may call | Authz enforcement | Residual risk |
| :--- | :--- | :--- | :--- |
| All `ControlPlaneService` RPCs | gateway, connector-hub (in trust zone) | per-tenant metadata interceptor (fail closed); store scoped to caller tenant | trust-zone reachability is the boundary (NetworkPolicy, ADR-016) |
| `ControlPlaneService.DeleteTenant` | gateway (on behalf of the user) | tenant from caller metadata; `req.confirm` must equal the caller's own tenant id — an intent check that **never selects** the tenant (`delete.go`) | self-erasure only |
| `SchedulerService.ListAllInstances` | connector-hub only | **deliberately cross-tenant** — the one ControlPlane exemption; exempted by **exact full-method name** in `main.go` (never by prefix); re-validates each row's tenant and fails closed per instance | the hub needs the fleet-wide instance list to schedule; it never returns document data, only instance metadata |
| `AdminService.{ListTenants,GetTenantUsage,SuspendTenant,AdminDeleteTenant}` | gateway, **after the admin-role gate** | **deliberately cross-tenant** — acts on the `tenant_id` in the request (the inverse of every ControlPlane RPC); exempted from the per-tenant metadata requirement by **exact full-method name** in `main.go`; the request tenant is validated against the syntax allowlist (`adminTenant`); every call audit-logged | safe ONLY because the gateway gates `/v1/admin/*` on the distinct admin claim AND the RPC is unreachable outside the trust zone; mirrors the `ListAllInstances` exemption exactly |

The cross-tenant **exemption** is the single highest-risk authz decision in M6 and was audited
specifically (§5). Two RPC families are exempt, both by exact method name and both safe for a
documented reason: `ListAllInstances` (scheduling metadata only, hub-only) and `AdminService`
(gated by the admin role at the edge, audit-logged). No other RPC is exempt; everything else
keeps the fail-closed tenancy interceptor.

### 2.3 Connector fetchers (Boundary 4 — server-side egress)

Connectors fetch tenant-configured URLs and source APIs server-side; the fetched body is
indexed and becomes searchable (an exfiltration oracle), and for OAuth connectors a decrypted
bearer token rides the request. Every tenant-controlled fetch is routed through
`platform/safehttp` (§4).

| Connector(s) | Fetch surface | Guard wiring | Residual risk |
| :--- | :--- | :--- | :--- |
| `ical`, `whatsapp-export`, `msteams` | tenant `feed_url` / `endpoint` / `export_url` | `safehttp.NewClientOrDefault` | a misconfigured deny-CIDR degrades to the secure default, never to an unguarded client |
| `s3` | tenant-configured S3 endpoint | `safehttp.NewTransport` as the minio `Options.Transport` | same |
| `gmail`, `gdrive`, `gcal`, `outlook-mail`, `outlook-cal`, `slack`, `confluence`, `jira` (8 OAuth connectors) | source API `base_url` (overridable) | `safehttp.GuardedBase()` as the base under the bearer transport — **so a `base_url` override can no longer ship the decrypted OAuth token to an internal/metadata host** | guard runs at connect time on the resolved IP; a public-then-internal DNS rebind is closed |
| `upload` | none (client streams bytes to the gateway) | n/a | not a server-side fetch |
| `imessage-agent` | none server-side (a local Go CLI reads `chat.db` and uploads via the API) | n/a | runs on the user's machine; not in the SSRF surface |

### 2.4 Pipeline & query internals (Boundary 2)

| Surface | Tenant source | Enforcement |
| :--- | :--- | :--- |
| Kafka `docs.raw` / `docs.chunked` / `docs.enriched` | message `tenant_id` header | `platform/kafkautil` tenant-checked produce/consume; poison records → `docs.deadletter` (ADR-004) |
| `query` → Vespa | `x-asker-tenant` metadata → `streaming.groupname=<tenant>` | every Vespa query carries the group selector (`services/query/vespa.go`); streaming mode physically scopes the scan to that group |
| `index-writer` → Vespa | `Document.tenant_id` → group `g=<tenant>` | upserts keyed into the tenant's group |

## 3. M6 abuse / DoS controls

### 3.1 Per-tenant connector-instance quota
`control-plane` caps how many connector instances one tenant may create
(`MAX_CONNECTOR_INSTANCES_PER_TENANT`, default 50). `CreateConnectorInstance` counts existing
instances and returns gRPC `RESOURCE_EXHAUSTED` over the cap (`server.go`). Defense in depth: the
scheduler additionally bounds sync-worker goroutines per tenant (`maxInstancesPerTenant`,
`scheduler.go`) so even stale rows cannot let one tenant dominate the scheduler; selection is
deterministic (sorted by instance id) so the cap never flaps.

### 3.2 Hub-wide concurrency bound
The scheduler runs at most `maxConcurrentSyncs` (4) syncs hub-wide via a semaphore, and exactly
one worker per active instance (serialized by construction), with exponential backoff on
repeated failure.

### 3.3 Gateway pre-auth throttle (DoS)
A token-bucket throttle sits **in front of** JWT verification (`preauth.go`, wired outermost in
`routes.go`) because the post-auth per-tenant limiter cannot defend an *unauthenticated* flood:
without it, a flood of unverifiable tokens would hammer JWT verify + JWKS refetch. It limits per
source IP (`PREAUTH_PER_IP_PER_MINUTE`, default 120) with a global ceiling
(`PREAUTH_GLOBAL_PER_SEC`/`PREAUTH_GLOBAL_BURST`, default 500/1000), fails closed with `429`, and
sweeps a bounded per-IP table so source-IP churn cannot grow memory unbounded. `X-Forwarded-For`
is honored only when `TRUST_PROXY_HEADERS=true` (set only behind a trusted proxy). JWKS fetches
use a bounded client (5s timeout, capped idle pool, `auth.go`); go-oidc's `RemoteKeySet` already
rate-limits refetches to once/minute for an unknown `kid`.

## 4. SSRF posture (defense in depth)

The named M6 SSRF deliverable. Two independent layers, either of which alone would block the
attack:

1. **Application layer — `platform/safehttp`.** A reusable SSRF-safe HTTP client/transport whose
   `net.Dialer.Control` hook inspects the **resolved IP at connect time** (after DNS resolution,
   immediately before `connect(2)`). That closes the resolve-then-connect TOCTOU / DNS-rebinding
   window a parse-time URL check cannot: the IP the kernel is about to talk to is the IP we
   inspect, on the initial request **and every redirect hop** (`CheckRedirect` re-guards + caps
   hops at 5). It rejects loopback, link-local (incl. the `169.254.169.254` cloud-metadata IP,
   denied **unconditionally** — even the dev relaxation will not open it), link-local/interface
   multicast, RFC1918, IPv6 ULA (`fc00::/7`), the unspecified address, IPv4-mapped IPv6 forms
   (`::ffff:127.0.0.1` normalized to its v4 form), and configurable cluster CIDRs
   (`ASKER_SAFEHTTP_EXTRA_DENY_CIDRS`). http(s) only; body-size capped. **Secure by default** —
   the only relaxation (`ASKER_SAFEHTTP_ALLOW_PRIVATE`, for the dev/CI compose network) is
   explicit, loudly logged once, and *still* keeps the metadata-IP and unspecified-address
   denies. There is no env var that opens loopback in production without an operator
   deliberately setting that flag.

2. **Network layer — the M4 NetworkPolicy egress (ADR-016 §1).** connector-hub is the one
   service granted broad external egress; that egress is restricted at L3/L4 to non-cluster
   destinations via `ipBlock.except` (cluster/link-local/metadata ranges). This is part of the
   opt-in egress posture (`networkPolicy.restrictEgress`, **default `false`** — see §6).

So even if the app guard were bypassed, an enforced-egress cluster still blocks the dial; and
even on a cluster without egress restriction, the app guard blocks it. The two layers are why
the SSRF posture is "defense in depth" and not a single point of failure.

## 5. M6 security review result

The M6 pen-test-style adversarial audit was **executed**: a **28-agent** review across injection,
crypto-binding, the authz boundary, SSRF, DoS, and the GDPR cascade, skeptic-verified. **10
findings confirmed and fixed; 12 refuted.** Injection, the per-tenant crypto binding, and the
authz boundary were verified **sound** and left untouched.

| # | Finding (confirmed) | Severity | Disposition |
| :-- | :--- | :--- | :--- |
| F1 | App-layer SSRF in connector fetchers — a tenant URL could reach IMDS/internal hosts; a parse-time check is defeated by DNS rebinding | **HIGH** | Closed via `platform/safehttp` connect-time IP guard, routed through every tenant-controlled fetch (§2.3, §4) |
| F2 | Prod could silently run on an ephemeral dev file KEK if `VAULT_ADDR` was unset | MEDIUM | Fail-closed `crypto.SelectKEK`: `ASKER_ENV=production` + empty `VAULT_ADDR` ⇒ startup error (`ErrProdRequiresVault`), never a silent dev KEK (ADR-015 §3) |
| F3 | An unauthenticated flood could hammer JWT verify + JWKS refetch (post-auth limiter cannot defend it) | MEDIUM | Pre-auth throttle in front of auth + JWKS-fetch hardening (§3.3) |
| F4 | One tenant could create unbounded connector instances → unbounded scheduler goroutines | MEDIUM | Per-tenant connector-instance quota (`RESOURCE_EXHAUSTED`) + scheduler per-tenant worker bound (§3.1) |
| F5 | No verifiable per-tenant erasure path (GDPR right to erasure) | MEDIUM | GDPR delete cascade + crypto-shred + verification (§7); `tools/e2e/gdpr-delete.sh` |
| F6 | Query / upload / media had no explicit size caps | LOW | `MAX_QUERY_CHARS`, `MAX_UPLOAD_MB`, `MAX_MEDIA_MB` (§2.1) |
| F7–F10 | Hardening refinements: redirect hop cap + per-hop re-guard, body-size cap, bounded JWKS client, audit logging on every admin/erasure action | LOW | All landed in the M6 commit |

**Refuted (12, examples):** the unauthenticated `/metrics` endpoint (intentional, carries no
tenant data, ADR-017 §1); a topic-label cardinality nit; an fp32/fp16 wording quibble; and
several "the tenant could be confused" reports that the `platform/tenancy` chokepoint already
makes impossible. The injection surface (Vespa YQL, Redis key globs, SQL) was probed and found
sound — the tenant alphabet `[A-Za-z0-9._-]` excludes every metacharacter that could widen a
selector or glob, and queries are parameterized.

## 6. Dev vs prod gap and accepted risks

The compose stack is intentionally insecure in ways acceptable only on a dev machine or CI
runner. Each gap has a planned/landed closure.

| Area | Dev (compose) | Production target | Status |
| :--- | :--- | :--- | :--- |
| Transport | HTTP only, no TLS | TLS at the edge; internal mTLS via mesh/cert-manager | Edge TLS + cert-manager `Certificate` CRs land in M4; internal mTLS handshakes are **scaffolded, not performed by the chart** (ADR-016 §2) |
| Credentials | Hardcoded dev passwords (Keycloak `admin/admin`, users `password123`, static MinIO/Postgres creds) | Provisioned secrets, no defaults | M4 (External Secrets / managed); dev creds live only in compose/realm files, loudly marked |
| Secrets / KEK | File-based dev KEK shim on a shared volume | HashiCorp Vault Transit; KEK never leaves Vault | **M6 wired** the provider selection with the fail-closed prod guard (F2); the chart's dev Vault is `vault server -dev` and **NON-PRODUCTION** (ADR-015) |
| SSRF egress | App guard relaxed via `ASKER_SAFEHTTP_ALLOW_PRIVATE` (flat compose net) | App guard strict + NetworkPolicy egress restriction | App guard **default-secure**; `networkPolicy.restrictEgress` **defaults `false`** — a zero-trust / multi-tenant-cluster deployment **must enable it** before go-live (ADR-016 §1, accepted risk) |
| Network posture | Flat compose network, ports on 127.0.0.1 | default-deny NetworkPolicy (ingress on by default; egress opt-in) | M4 (ADR-016); enforcement needs a policy-enforcing CNI (Calico/Cilium), not kind's default kindnet |
| Internal peer identity | Topological trust | mTLS (cryptographic peer auth) | Scaffolded; until a mesh is deployed, internal traffic is **authorized-but-plaintext** (ADR-016 §2, accepted for single-tenant-per-cluster) |
| Admin console | API only (`/v1/admin/*`) | Web admin UI | **Deferred follow-up** (M6 shipped the API; the console is post-M6) |

No secrets in code, ever. See [`docs/ship-review.md`](ship-review.md) §4 for the full
accepted-risk register and the CI-vs-local matrix.

## 7. GDPR delete + crypto-shred guarantees

Full per-tenant erasure (right to erasure) is a first-class, verifiable operation, drilled by
`tools/e2e/gdpr-delete.sh` (`make e2e-gdpr`). Two entry points, one shared cascade
(`cascadeDelete` in `services/control-plane/delete.go`):

- **Self-serve:** `DELETE /v1/me/data` → `ControlPlaneService.DeleteTenant`. The tenant comes
  ONLY from the verified token; the gateway fills the `confirm` field from the verified context;
  **a user can only ever erase their own data.**
- **Operator:** `DELETE /v1/admin/tenants/{tenant}` → `AdminService.AdminDeleteTenant`, behind
  the admin-role gate (§2.1), for abuse takedown / erasure on behalf of a user. Audit-logged.

The cascade, in this order (order matters):

1. **Crypto-shred the DEK first** (`DEKStore.Delete` + `cipher.Forget`). With the wrapped DEK
   destroyed, every blob/token ciphertext is **already unrecoverable** even if a later store
   deletion partially fails — the fast, fail-safe GDPR primitive.
2. **Postgres** — the tenant row, `connector_instances`, `sync_states`, and the encrypted
   `tokens` (`Store.PurgeTenant`).
3. **Vespa** — delete the tenant's entire streaming group
   (`DELETE /document/v1/asker/doc/group/<tenant>`, following continuations to completion).
4. **MinIO** — delete the tenant's blob prefix (`blob.DeletePrefix`).
5. **Redis** — best-effort purge of the `q:<tenant>:*` result-cache and `rl:<tenant>:*`
   rate-limit keys (derived, TTL'd data; a failure is logged, not fatal). The tenant alphabet
   excludes Redis glob metacharacters, so the match pattern cannot widen to another tenant.
6. **Verify** — re-read Postgres + the DEK store; any residue **fails the RPC closed**
   (`VerifiedEmpty`).

Every step is tenant-scoped from the verified identity, idempotent (a retried erasure
converges), and emits an audit line. `gdpr-delete.sh` proves a tenant's data is gone across all
stores **and** that a second tenant's data is completely untouched — the sacred isolation
invariant holds for delete too, not just read.

## 8. Tenancy model (the foundation)

- **`tenant_id` derives only from the verified OIDC token.** The gateway validates the JWT
  (signature against Keycloak's JWKS, issuer, audience, expiry) then derives the tenant: the
  `tenant_id` claim when present (a present-but-invalid value fails closed — never falls back),
  else `sub`. The chosen value must match a strict syntax allowlist (`[A-Za-z0-9._-]`, max 128
  chars, no `.`/`..`) because tenant IDs are embedded in storage keys and query selectors.
  Nothing from the request body, query string, or headers ever influences tenant selection
  (ADR-002).
- **`platform/tenancy` is the single chokepoint.** It is the only way to construct a tenant
  context (`tenancy.FromClaims` → `tenancy.WithContext`); downstream code reads it back via
  `tenancy.FromContext` / `MustFromContext`. The `Context` type has unexported fields, so a
  tenant context cannot be forged outside the package. Data-access code takes a tenant context,
  not a raw string — querying without a tenant is a compile-time or fail-closed error. The spec
  mandates 100% test coverage on this package (enforced by `make coverage-gate`).
- **Tenant scoping propagates through every layer:** Kafka messages keyed by `tenant_id`, every
  internal RPC carries it (ADR-009), every Vespa query carries `streaming.groupname=<tenant>`,
  blobs and tokens are encrypted under the per-tenant DEK.
- **The cross-tenant leakage suite (`tools/e2e/leakage.sh`) is sacred:** it attempts every API
  with mismatched tenant tokens and must prove isolation. It must pass before any ship.
