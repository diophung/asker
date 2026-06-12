# ADR-003: Dev/CI stack deviations from the target architecture

## Status

Accepted (M0).

## Context

The Docker Compose stack (`deploy/compose/`) exists so a developer or CI runner can bring up
the entire system on one machine with `make dev-up`. A handful of places diverge from the
production target architecture, either because the spec sanctions a dev substitute, because a
laptop/CI runner cannot reasonably run the production choice, or because compose networking
forces it. This ADR records those deviations so they are deliberate, visible, and bounded.

## Decision

The dev/CI stack deviates from the target architecture in exactly these ways:

1. **Redpanda instead of Apache Kafka (Strimzi).** The spec itself prescribes this split:
   Redpanda (single binary, no ZooKeeper/KRaft cluster) in compose; Apache Kafka via the
   Strimzi operator in Kubernetes (M4). Both speak the Kafka API; application code is
   identical against either.

2. **TEI embedding model overridable in CI.** The compose service uses
   `MODEL_ID=${TEI_MODEL_ID:-BAAI/bge-m3}`. Dev and prod default to `BAAI/bge-m3` (the
   decided embedding model, ~2.3GB download); CI sets `TEI_MODEL_ID` to a small model so the
   stack becomes healthy in seconds rather than tens of minutes. Embedding *quality* is never
   asserted in CI — only pipeline plumbing — so a small model changes nothing CI verifies.

3. **Explicit issuer + JWKS URL instead of OIDC discovery in the gateway.** In compose,
   Keycloak has a split identity: browsers and token clients reach it at
   `http://localhost:8081`, while the gateway reaches it at `http://keycloak:8080` on the
   compose network. `KC_HOSTNAME=http://localhost:8081` pins token `iss` to the host-visible
   URL, but OIDC discovery from inside the network would then yield endpoints the gateway
   resolves differently (or not at all). The gateway therefore takes `OIDC_ISSUER` (what to
   expect in `iss`) and `OIDC_JWKS_URL` (where to actually fetch keys) as separate settings.
   In Kubernetes with real DNS/ingress (M4), both can point at the same canonical URL and
   discovery becomes viable again.

4. **Non-standard host ports for Postgres (15432) and Redis (16379).** Ports 5432 and 6379
   are already taken on the primary dev machine. Container-internal ports are standard; only
   the host mapping differs, so anything running inside the compose network is unaffected.

5. **Memory caps on every large service (dev only).** A laptop Docker VM is small (8GB on
   the primary dev machine) and often shared with other compose stacks; uncapped, several
   images size themselves off total VM memory and the kernel OOM-kills whichever container
   spikes next (observed for TEI, then Redpanda, during M0 bring-up). The compose stack
   therefore pins: Vespa `VESPA_IGNORE_NOT_ENOUGH_MEMORY=true` (image otherwise refuses to
   start below 4GB available) plus a small config-server heap (`VESPA_CONFIGSERVER_JVMARGS`)
   and a capped query-container heap (`<jvm options>` in `vespa/app/services.xml`); Keycloak
   `JAVA_OPTS_KC_HEAP=-Xmx400m` (default heap is 70% of the VM when uncgrouped); Redpanda
   `--memory 512M --reserve-memory 0M` (Seastar otherwise pre-allocates from total VM
   memory); TEI `--tokenization-workers 2 --max-batch-tokens 1024` (defaults scale with
   nproc and OOM-killed the container during warmup). None of these caps apply to
   production sizing, which is an M4/M5 concern with its own capacity math.

6. **TEI runs under amd64 emulation on Apple Silicon, and constrained machines may
   override the model locally.** TEI publishes CPU images for linux/amd64 only, so Apple
   Silicon runs it under Rosetta (works; slower embeds). On machines whose Docker VM cannot
   fit bge-m3 (~2.3GB weights) alongside the rest of the stack, a gitignored
   `deploy/compose/.env` may set `TEI_MODEL_ID` to a small model — same mechanism CI uses.
   The committed default remains `BAAI/bge-m3`.

## Consequences

- `make dev-up && make e2e-smoke` runs the real architecture shape — same Kafka API, same TEI
  API, same OIDC validation path — with substitutions only where noted.
- CI must export `TEI_MODEL_ID` (small model) for fast runs; forgetting it means a long model
  download, not a failure.
- Anyone connecting host tools to Postgres/Redis must use `localhost:15432` /
  `localhost:16379` (documented in the README dev URLs table).
- The gateway's issuer/JWKS split is a config-shape commitment: it must keep working when
  both values converge in production, and discovery can be reconsidered in M4.
- Each deviation has a defined end state (M4 for 1 and 3; permanent-but-harmless for 2 and 4).
  New dev/prod divergences require updating this ADR or writing a new one.
