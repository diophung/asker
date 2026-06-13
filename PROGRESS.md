# Progress log

This file is the project's memory between sessions. Read it (and
[MILESTONES.md](MILESTONES.md)) at the start of every session; append an entry at the end of
every session.

Entry format:

```
## YYYY-MM-DD — <milestone>
- Done: what was completed this session
- Next: the immediate next steps
- Known issues: open problems, workarounds, anything that will bite later
```

Newest entries go first.

---

## 2026-06-13 — M1 vertical slice (functionally complete; full-scale e2e gated on dev VM memory)

- Done: **The M1 vertical slice runs end to end on the live stack.** Gmail + upload connectors
  → connector-hub → Kafka (`docs.raw`) → ingest (normalize/dedupe/chunk) → enrich (Python, TEI
  embeddings) → index-writer → Vespa streaming groups; query service (hybrid + degradation +
  Redis cache) behind the gateway REST API; React search UI. 16-service compose stack, all
  healthy. Built in three parallel waves (platform libs + SDK + control-plane + Vespa-v1 +
  fake-gmail + web; then the 7 pipeline services; then the e2e/leakage suites), each integrated
  and committed.
  - **New platform libs:** `kafkautil` (tenant-enforcing producer/consumer, at-least-once +
    deadletter), `crypto` (per-tenant envelope encryption, file-KEK dev shim), `blob` (encrypted
    MinIO store), tenancy gRPC/Kafka propagation. `connectors/sdk` + `connectortest`.
  - **Services:** control-plane (encrypted token vault, SchedulerService), connector-hub
    (scheduler, webhooks, upload), ingest, enrich, index-writer, query, gateway REST buildout
    (search/connectors/upload + per-tenant rate limit + CORS).
  - **Verified directly against the live stack:** hybrid search precise (rare token → 1 hit) AND
    vector-ranked (closeness contributes — scores vary); keyword + hybrid + degradation paths;
    tenant isolation (per-group doc_id distinctness; streaming.groupname enforced).
  - **Adversarial review (21 agents):** 2 blockers + 1 major + minors found and fixed — ingest
    at-least-once (record-after-produce), hybrid vector signal (rank() operator), scheduler
    concurrent-sync drain, enrich retry/acks parity, before: date inclusivity. All with
    regression tests. 9 findings refuted as non-issues.
  - **Sacred cross-tenant leakage suite passes** (search/header-injection/param-injection/
    resource-id/token-abuse/internal-port-exposure isolation, 49 checks).
- Next: **M2 — connector framework + breadth.** Finalize the SDK (in-proc + gRPC plugin), then
  Outlook mail, Google Drive (ACLs), S3, calendars, iCal, Slack, Confluence, Jira, WhatsApp
  export, iMessage agent; connector management UI; "build a connector in <1 day" tutorial proven
  by implementing MS Teams. Before M2, run the full `tools/e2e/m1-e2e.sh` at 10K scale in CI
  (see known issues) to formally bank the M1 exit criterion.
- Known issues:
  - **The full 3-tenant / 10K-email e2e (`tools/e2e/m1-e2e.sh`) cannot run to completion on this
    8 GB Docker VM** — it is shared with two other always-on stacks (~3 GB), and the 16-service
    Asker stack under multi-tenant ingest load OOM-kills Vespa (exit 137), bringing the project
    down. The suite is wired into CI (`e2e-m1` job, ubuntu-latest 16 GB) where it has the
    headroom. Locally, every behavior the suite asserts was verified directly (search precision,
    vector ranking, isolation, the leakage suite). **Recommend bumping Docker Desktop memory to
    ≥14 GB before relying on the full local e2e.** The stack at rest fits comfortably (~3–4 GB);
    only concurrent ingest + builds + heavy host commands tip it over.
  - Pipeline freshness/delete/upload paths are exercised by the leakage suite (upload + isolation
    + post-attack re-sync) and by direct verification, but the m1-e2e suite's freshness/delete
    stopwatch assertions have only been validated for logic, not run green at scale locally — CI
    is the proving ground.
  - gmail `replayHistory` buffers a full `history.list` response in memory (fine at M1 scale;
    bound it for M5 large-corpus seeding — review finding, deferred with the other scale work).
  - SSRF: connector `base_url`/`webhook_url` are tenant-supplied and fetched server-side without
    host validation. Acceptable under the M1 closed-network trust model (ADR-009); harden in
    M4 (NetworkPolicy/egress) and the M6 security review.

## 2026-06-12 — M0 closed

- Done: **M0 complete — `make dev-up && make e2e-smoke` passes (all 19 checks).**
  - Monorepo scaffold: single Go module (ADR-001), Makefile, pinned tool versions.
  - Platform libraries: `platform/proto` (canonical Document protobuf + buf, generated code
    committed, CI drift check), `platform/tenancy` (tenant chokepoint, strict claim
    validation with syntax allowlist per ADR-002, 100% statement coverage), `platform/config`
    (env loader, 100%), `platform/telemetry` (OTel init + HTTP middleware, no-op safe, ~83%).
  - Dev stack (compose): Redpanda, Postgres, MinIO, Redis, Keycloak (realm auto-import:
    `asker` realm, `asker-web` client + audience mapper, dev users alice/bob), Vespa
    (streaming mode, app package deploys via `vespa/deploy.sh`), TEI. All healthchecked;
    all host ports bound to 127.0.0.1; memory-capped to fit small Docker VMs (ADR-003 §5).
  - Gateway: OIDC JWT validation (issuer+JWKS split per ADR-003 §3), tenant only from
    verified claims, `/healthz` `/readyz` `/v1/me`, distroless image with self-probe
    healthcheck. Tests cover forged-signature, alg=none, HS256, expired/iss/aud attacks.
  - CI: lint (golangci-lint v2 + buf lint + proto drift check), test + coverage gate
    (tenancy ==100%, platform libs >=75%), build + trivy scan, compose e2e smoke job.
  - Smoke test proves: stack health, OIDC password grant -> tenant == token `sub`,
    401 without token, and Vespa streaming-group tenant isolation (feed as tenant A,
    query as tenant B -> 0 hits; delete propagates).
  - Adversarial review (5 dimensions, every finding skeptic-verified): 14 confirmed
    findings all fixed (incl. 0.0.0.0 port bindings, nonexistent trivy-action ref,
    duplicate duration metric, tenant syntax validation); 8 false positives rejected.
- Next: **M1 — vertical slice.** Upload + Gmail connectors -> ingest -> chunk -> embed ->
  Vespa index -> query service (hybrid search) -> web UI. Tombstone/delete flow. The sacred
  cross-tenant leakage suite. Control-plane Postgres schema and `platform/kafkautil` /
  `platform/crypto` libs will be needed early.
- Known issues:
  - CI has never executed on GitHub (no remote configured yet) — workflows are authored and
    locally validated (lint/test/gate/smoke all green locally) but unproven on Actions
    runners; first push should watch the e2e job's disk/memory headroom.
  - The primary dev machine's Docker VM (8GB, shared with other stacks) cannot fit
    `BAAI/bge-m3`; a gitignored `deploy/compose/.env` pins `TEI_MODEL_ID=BAAI/bge-small-en-v1.5`
    locally (ADR-003 §6). Raise Docker Desktop memory to >=12GB to run the real default.
    Embedding dimension (384 vs 1024) starts to matter in M1 when vectors land in Vespa.
  - TEI runs under amd64 emulation on Apple Silicon (no arm64 CPU image) — fine for dev,
    slow for bulk embedding; M1 ingest tests should use small corpora locally.
  - Vespa M0 schema uses nativeRank only (streaming mode lacks bm25 corpus statistics);
    M1 hybrid ranking needs a deliberate ranking-profile design.
