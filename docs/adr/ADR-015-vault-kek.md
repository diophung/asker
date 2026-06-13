# ADR-015: HashiCorp Vault Transit as the production envelope-encryption KEK

## Status

Accepted (M4).

## Context

Asker's security model is per-tenant envelope encryption (M1, `platform/crypto`): every tenant
has one AES-256 Data Encryption Key (DEK); tenant data (OAuth tokens in Postgres, blobs in MinIO)
is encrypted under that DEK, and the DEK itself is stored only in *wrapped* form, encrypted by a
Key Encryption Key (KEK) via the `crypto.KEKProvider` interface
(`WrapDEK(ctx, tenantID, dek)` / `UnwrapDEK(ctx, tenantID, wrapped)`). The tenant ID is bound into
the wrapping as additional authenticated data (AAD), so a wrapped DEK moved across tenant rows
fails to unwrap — cross-tenant key movement is cryptographically detected, not merely
access-controlled (see `platform/crypto/doc.go`).

Through M1–M3 the only `KEKProvider` is `NewFileKEK`: a single 32-byte AES-256 key on a local file
(the shared `kek-keys` volume in `deploy/compose/docker-compose.yml`), wrapping DEKs with
AES-256-GCM and the tenant ID as GCM AAD. This is a deliberate **dev shim**: the KEK sits in plain
bytes in service memory and on a shared volume, with no rotation, no audit, and no central custody.
ADR-003 and ADR-013 explicitly deferred the file-KEK → HashiCorp Vault transition to M4, and
ADR-014 lists Vault integration as part of the M4 production-deployment surface. M4 also hardens
the internal trust boundary (ADR-009) with NetworkPolicy/mTLS; the KEK is the highest-value secret
that boundary protects, so it should leave service memory entirely in production.

The constraints are: keep the *exact* `KEKProvider` interface and the wrapped-DEK lifecycle
(`DEKStore`, `TenantCipher`) unchanged so the swap is a wiring change, not a rewrite; preserve the
per-tenant binding guarantee; and stay cloud-agnostic (no provider KMS) — Vault is itself
self-hostable and cloud-portable.

## Decision

**1. A Vault Transit-engine `KEKProvider` (`platform/crypto/vault.go`, `NewVaultKEK`).**
`vaultKEK` implements the same `crypto.KEKProvider` interface as `fileKEK`. `WrapDEK`/`UnwrapDEK`
call the Vault HTTP API directly — `POST {VAULT_ADDR}/v1/transit/encrypt/<keyName>` and
`/v1/transit/decrypt/<keyName>` with the `X-Vault-Token` header — using only `net/http` and the
stdlib (Go deps are frozen; no Vault SDK, mirroring the M2 connectors' REST approach). The KEK
never leaves Vault: only wrap/unwrap round-trips cross the wire, so unlike `fileKEK` the key bytes
never enter service memory. `NewVaultKEK(VaultConfig{Addr, Token, KeyName, HTTPClient})` validates
config and uses a 10s-timeout client when none is supplied; the wrapped DEK is the opaque Vault
ciphertext token (`vault:v1:…`).

**2. Per-tenant Transit "context" = the M1 AAD binding.** Every encrypt/decrypt sends the tenant ID
as the Transit `context` (base64). The `asker-kek` Transit key is created **derived (convergent)**
(`derived=true`), so Vault derives a distinct per-tenant key from the context. A DEK wrapped for
tenant A therefore cannot be unwrapped under tenant B (Vault returns a derived-key context
mismatch) — the same cross-tenant guarantee `fileKEK` gets from binding the tenant ID as GCM AAD.
This keeps the security property identical across the two providers.

**3. Selection by environment.** Control-plane and connector-hub (the two services that own
envelope crypto) select the provider by env: `VAULT_ADDR` set → `NewVaultKEK`; otherwise fall back
to `KEK_FILE` → `NewFileKEK`. **Integrator note (out of scope here — `services/**` is frozen):** add
the small `main.go` wiring in control-plane and connector-hub, e.g.

```go
var kek crypto.KEKProvider
if addr := os.Getenv("VAULT_ADDR"); addr != "" {
    kek, err = crypto.NewVaultKEK(crypto.VaultConfig{
        Addr:    addr,
        Token:   os.Getenv("VAULT_TOKEN"),
        KeyName: cmp.Or(os.Getenv("VAULT_KEK_KEY_NAME"), "asker-kek"),
    })
} else {
    kek, err = crypto.NewFileKEK(os.Getenv("KEK_FILE"))
}
```

Both services must agree on the same `KeyName` (as today they share the same KEK file bytes), so a
DEK wrapped by one unwraps in the other.

**4. A dev/CI Vault in the Helm chart (`deploy/helm/asker/templates/vault/**`), gated by
`vault.deploy`.** When `vault.deploy=true` the chart renders: a single-replica **dev-mode** Vault
`Deployment` (`vault server -dev`: in-memory, auto-unsealed, fixed root token), a ClusterIP
`Service` named `vault` (port 8200, so services dial `http://vault:8200` by DNS — same bare-name
convention as the app Services), a dev token `Secret`, a `ServiceAccount`, and a post-install Helm
hook `Job` that enables the `transit` engine and creates `asker-kek` as a derived key. This lets
the M4 kind/k3d CI stack exercise the full Vault KEK path self-contained. The dev Vault is loudly
marked **NON-PRODUCTION** (annotations + template banners): no persistence, no unseal/auto-unseal,
no HA, static root token. `vault.deploy=false` (the default) renders nothing — point `vault.addr`
at a managed/external Vault instead.

## Consequences

- **Vault availability is now on the decrypt path.** With `VAULT_ADDR` set, every cold tenant
  token/blob decrypt (first use, before the unwrapped DEK is cached by `TenantCipher`) requires a
  reachable Vault. Vault becoming unavailable degrades exactly those cold paths; warm tenants
  (cached DEKs) keep serving. Production must run Vault HA (Raft, multiple nodes, auto-unseal) so
  this dependency is as available as Postgres. This is the deliberate trade for removing the KEK
  from service memory.
- **Dev keeps `FileKEK`.** The compose inner loop and any deployment without `VAULT_ADDR` keep the
  zero-dependency file shim, so local development is unchanged. Only prod (and the
  `vault.deploy=true` CI stack) takes the Vault path.
- **Interface and stored format unchanged.** `KEKProvider`, `DEKStore`, `TenantCipher`, and the
  data-ciphertext format are untouched; only the *wrapped-DEK* bytes differ (a `vault:v1:` token vs.
  the `0x01||nonce||GCM` file-KEK blob). Existing tenants' wrapped DEKs are tied to whichever
  provider wrapped them — migrating an existing FileKEK deployment to Vault means re-wrapping each
  tenant's DEK (unwrap under FileKEK, wrap under Vault); this is a one-time, audited operation, like
  the planned key rotation, and is out of scope for this ADR (greenfield prod starts on Vault).
- **Prod hardening required for the chart's dev Vault.** The shipped dev-mode Vault must NOT reach
  production. Production runs Vault out-of-band (HashiCorp Vault Helm chart / operator) with
  Integrated Storage (Raft), HA, auto-unseal (cloud KMS or a Transit auto-unseal Vault), audit
  logging, and a **least-privilege token** — a policy granting only `transit/encrypt/asker-kek` and
  `transit/decrypt/asker-kek`, delivered via the Kubernetes auth method / Vault Agent injection /
  External Secrets, not a static root token. The transit engine + derived key are provisioned by
  Vault bootstrap (Terraform / config-as-code), not the init Job. This aligns with ADR-014's
  `secrets.strategy: vault` recommendation.
- **No new Go dependency, no provider lock-in.** The HTTP-only client keeps `go.mod` frozen and
  keeps the design cloud-agnostic (Vault runs anywhere); the cost is that we track the small surface
  of the Transit encrypt/decrypt API ourselves rather than via the Vault SDK.
- **New values keys.** `vault.{deploy,addr,keyName,image,port,devRootToken,tokenSecretName,
  serviceAccountName}` are consumed via `default()` in the vault templates so the chart renders
  before they are merged into `values.yaml`; the integrator should add the documented `vault:` block
  to the authoritative `values.yaml`.
