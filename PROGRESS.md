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

## 2026-06-13 — Search results link to their original source (post-V1)

- Done: **Every search result now carries a `source_url` the web renders as a "View original ↗"
  link** — jump from a hit straight to the item at its origin (the Gmail message in Gmail, the
  Drive file, the Slack permalink, the Outlook/Jira/Confluence/Teams/calendar item, …).
  - **No proto/Vespa/index-writer change.** `Document.metadata` already flows end to end
    (connector → `metadata_json` summary → `Hit.metadata` → gateway JSON → web). Most connectors
    already recorded a link under inconsistent keys (Outlook/Outlook-cal `web_link`, Drive
    `web_view_link`, Slack `permalink`, GCal `html_link`, Teams/Confluence/Jira `web_url`, iCal
    `url`); the **gateway** now derives ONE canonical `source_url` from them in priority order
    (`services/gateway/search.go` `sourceURLFromMetadata`). Safety gate: only an absolute http(s)
    URL **with a host** becomes a link — never `javascript:`/`data:`/scheme-relative/opaque — since
    the web renders it as an href.
  - **Gmail** was the one connector with no link in its metadata; added
    `web_link = https://mail.google.com/mail/u/0/#all/<id>`.
  - **Web:** `Hit.source_url`; `ResultCard` renders the link `target=_blank rel="noopener
    noreferrer"`; `.result-meta` is now a flex row. Degrades to no link when absent.
  - **Tested + reviewed:** gateway `TestSourceURLFromMetadata` (15 cases incl. the blocked dangerous
    schemes), pinned-shape test updated, gmail golden metadata, web 91 tests (3 new). Adversarial
    review (link-safety + contract, 17 agents skeptic-verified): **no defects**; the one hostless-URL
    nit was fixed. Committed `982aa76`. Live in the dev stack (gateway+web rebuilt).
- Next: optional — an Asker-served link for sources with NO web origin (uploaded files, WhatsApp
  export, iMessage): a gateway `/v1/download` (or extend `/v1/media`) route serving the decrypted
  original blob, then surface it as the `source_url` fallback for FILE hits. Currently those hits
  correctly show no link.
- Known issues: the Gmail deep link uses `/u/0/` (the browser's first signed-in Google account); a
  user signed into multiple Google accounts in the same browser may land in the wrong account's
  Gmail. Acceptable for the common single-account case.

## 2026-06-13 — Connector OAuth integrations + dev-stack fixes (post-V1)

- Done: **Real OAuth2 (auth-code + PKCE + refresh) for all four providers — Google, Microsoft,
  Slack, Atlassian — wired end-to-end against a dev fake provider, plus four reported dev-stack
  issues root-caused and fixed.** Shipped as two committed waves + a security-review fix pass.
  - **Dev-stack fixes (all four user reports resolved):**
    - Web sign-in "could not reach the identity provider" was **not an Asker bug** — host port 3000
      collided (IPv6 `::1` shadowing the IPv4 bind) with the user's other Next.js app. Remapped the
      web UI to **127.0.0.1:13001** (compose) and updated the `asker-web` realm `redirectUris` +
      `webOrigins`, the gateway `CORS_ALLOWED_ORIGINS`, and the README dev-URL table/quickstart.
    - "Keycloak admin/admin didn't work" / "alice/password123 didn't work" — both **verified working**
      (master-realm console = admin/admin; the app = the `asker` realm, alice/password123). The
      browser failure was same-origin autofill of the admin password; incognito resolves it. No code
      change — usage clarification proven by a simulated full auth-code flow (302 + code).
    - **Connectors tab blank screen** — the gateway's connector endpoints serialize protojson
      (lowerCamelCase, int64-as-string, base64 config) while the web expected snake_case;
      `sync.docs_emitted.toLocaleString()` threw on `undefined`. The gateway shape is e2e-locked, so
      the fix is a web anti-corruption layer in `web/src/api.ts` (normalize camelCase→snake_case,
      coerce numbers, null→""). 88 web tests pass.
  - **OAuth feature (the "implement all the actual OAuth integrations" ask):**
    - `platform/oauth` — provider registry (Google/Microsoft/Slack/Atlassian), the stored `Token`
      blob (JSON in the SAME envelope-encrypted vault — no proto change; legacy opaque tokens like
      `fake-gmail-token:…` still pass through unchanged), and the flow service: `AuthCodeURL`
      (per-provider quirks — Google `access_type=offline&prompt=consent`, Atlassian `audience`+
      `prompt=consent`, Slack `user_scope`), `Exchange`, `Refresh`. Slack's non-standard
      `authed_user` envelope (ok:false on HTTP 200) is parsed by hand for BOTH exchange and refresh.
    - Gateway: `GET /v1/connectors/{id}/oauth/start` (**authed**, instance-ownership checked) →
      Redis-stored single-use state (GETDEL) carrying tenant + connector + PKCE verifier →
      `GET /v1/oauth/callback` (**public**, outside the authed mux) that rebuilds the tenant **solely
      from the trusted server-side state**, never any request value, exchanges the code, stores the
      token under the correct tenant, and 302s to the FIXED `WEB_APP_URL`.
    - Hub: a single `fetchToken` refresh hook (refresh-if-`NeedsRefresh(now,60s)`, re-store, hand the
      connector the access-token string) — no per-connector changes; the `oauth.Service` is built
      once, only when a provider is configured, over an SSRF-safe client.
    - Dev fake provider `tools/fake-oauth` (127.0.0.1:9500, auto-consent, real PKCE-S256 +
      single-use-code + redirect_uri validation, issues the `fake-gmail-token:<subject>` shim for
      Google, the `authed_user` envelope for Slack) + compose wiring (browser `AUTH_URL`=localhost,
      in-network `TOKEN_URL`=fake-oauth, `ASKER_SAFEHTTP_ALLOW_PRIVATE=1` dev-only). `docs/oauth.md`
      is the operator guide for plugging in real apps (register app → redirect URI
      `<GATEWAY_PUBLIC_URL>/v1/oauth/callback` → `ASKER_OAUTH_*` env).
    - Web Connect UX: per-connector "Connect with <provider>" buttons + `?oauth=connected|error`
      banners; `ConnectorClient.startOAuth` hits the authed start endpoint.
  - **Adversarial security review** (4 dimensions, skeptic-verified): the security core is **sound** —
    tenant binding from trusted state only, single-use crypto-random state, server-side PKCE, no
    secret/token leakage, legacy pass-through intact. Two **provider-correctness** findings confirmed
    and fixed: (LOW) a refresh reply omitting `expires_in` left `Expiry` zero → `NeedsRefresh` read it
    as non-expiring and stopped self-healing — now carries `prev.Expiry` forward in `tokenFrom`;
    (MEDIUM) a rotation-enabled Slack token broke on refresh because `Refresh` routed Slack through
    x/oauth2's decoder, which can't parse the `authed_user` envelope — `Refresh` now has a hand-rolled
    Slack branch (`refreshSlack`) mirroring exchange, with carry-forward of refresh/expiry/scope.
  - **Validation:** `tools/e2e/oauth.sh` (12/12: start-without-bearer→401, authorize→callback→
    `?oauth=connected`, single-use replay→error, post-OAuth gmail sync emitted 26 docs, searchable) +
    88 web tests + the full Go sweep (build/vet/lint clean; coverage gate PASSED, `platform/oauth` 92.7%).
- Next: **none required.** Operators supply real `client_id`/`client_secret` per `docs/oauth.md` to go
  live; live webhook/push remains the M2 follow-up. Optional: a web admin console for connector OAuth status.
- Known issues / accepted (dev-only, 127.0.0.1-bound, loudly marked): the fake provider auto-consents
  and `ASKER_SAFEHTTP_ALLOW_PRIVATE=1` is set on gateway+hub in compose only (NOT the Helm values; the
  metadata IP stays blocked regardless). Real provider token endpoints are public, so prod keeps the
  full `safehttp` SSRF guard.

## 2026-06-13 — M6 hardening (complete) — V1 BUILD COMPLETE (M0–M6)

- Done: **M6 complete — the security review was executed, the hardening landed, and the ship-review
  is signed off (docs/ship-review.md). M0–M6 are all complete; Asker V1 is built.** Done as: a
  pre-build adversarial security audit (28 agents, skeptic-verified: 10 confirmed / 12 refuted) →
  a 2-builder hardening vertical → a focused review of the new security code (22 agents: 13
  confirmed / 4 refuted) → all 13 fixed → docs + ship-review.
  - **SSRF (the named deliverable; was HIGH):** new `platform/safehttp` — an SSRF-safe HTTP client
    whose `net.Dialer.Control` decides on the RESOLVED IP at connect time (closing DNS-rebinding /
    TOCTOU), re-checks every redirect hop, disables proxy-env tunnelling, and blocks loopback/
    link-local/IMDS/RFC1918/ULA/unspecified PLUS NAT64 (`64:ff9b::/96`) and CGNAT (`100.64/10`) +
    IETF-reserved (the close-out review caught the NAT64/CGNAT holes). Wired into all 12 connector
    fetchers (AuthNone via `NewClientOrDefault`/`NewTransport`; the 8 OAuth connectors via
    `GuardedBase` under the bearer transport — so a `base_url` override can't ship the decrypted
    token to an internal host). Defense-in-depth with the M4 NetworkPolicy egress.
  - **GDPR per-tenant delete cascade (greenfield):** control-plane `DeleteTenant` + gateway
    `DELETE /v1/me/data` (caller's own tenant) — Postgres rows + **crypto-shred** of `tenant_deks`
    (destroying the DEK shreds all the tenant's ciphertext), Vespa group delete, MinIO prefix purge,
    Redis purge, an audit line + verification. It **suspends the tenant's connectors first**
    (re-ingest fence) and is idempotent. `tools/e2e/gdpr-delete.sh` proves the tenant is gone and a
    second tenant is untouched. (Residual: a per-tenant Kafka tombstone for in-flight records is a
    documented follow-up.)
  - **Quotas/DoS:** atomic per-tenant connector-instance cap (no TOCTOU), a gateway **pre-auth
    throttle** in front of JWT verify (the post-auth per-tenant limiter can't stop an unauth flood),
    JWKS bounding, query/upload caps.
  - **Vault-KEK wiring (was MEDIUM):** control-plane + connector-hub select `NewVaultKEK` when
    `VAULT_ADDR` is set, else the file-KEK, with a **fail-closed prod guard** (`ASKER_ENV=production`
    + empty `VAULT_ADDR` → startup error; tolerant of capitalization).
  - **Admin API (greenfield):** `AdminService` (ListTenants/GetTenantUsage/SuspendTenant/
    AdminDeleteTenant) + gateway `/v1/admin/*` gated by the `asker-admin` role claim, audit-logged
    with the **verified operator identity** (`x-asker-admin-subject`). Web UI deferred.
  - **Docs:** `docs/security.md` (threat model + authz matrix + the audit dispositions),
    `docs/runbooks/top-10-failure-modes.md`, `docs/ship-review.md` (the exit-criterion sign-off).
  - **Hygiene:** the coverage gate now enforces the CLAUDE.md contract for EVERY `platform/*`
    package (was a hardcoded 3); `platform/safehttp` is at 87%.
- Next: **none — V1 (M0–M6) is complete.** Post-V1 follow-ups (tracked in docs/ship-review.md §5):
  the admin web console, the GDPR in-flight-Kafka tombstone, turning on `networkPolicy.restrictEgress`
  + a policy-enforcing CNI in prod, internal mTLS via a mesh, and live OAuth/webhook push (M2).
- Known issues / accepted risks (see docs/ship-review.md §5): the kind chaos (M4), full load + 2h
  soak (M5), NetworkPolicy enforcement, and the GDPR drill run in **CI / on a real cluster only**,
  not on the 8GB dev VM. Dev creds/Vault/Keycloak are loudly dev-only. `restrictEgress` is opt-in,
  so the app-layer `safehttp` guard is the active SSRF control by default.

## 2026-06-13 — M5 scale & SLO verification (complete; full-scale load + soak run in CI)

- Done: **M5 complete — the SLO instrumentation, the load/soak tooling, and the observability
  stack are built, and the adversarial review's findings are fixed so the SLO verification
  actually measures real work.** Built in 2 waves + an integration pass + a 6-dimension review
  (20 agents): **13 confirmed / 3 refuted**, all 13 fixed (the headline one was a vacuous load
  suite — see below).
  - **SLO metrics (wave 0, platform/telemetry + services):** a Prometheus exporter is always
    installed (so `/metrics` works with no collector; OTLP coexists when an endpoint is set),
    served on each service's health port. New low-cardinality OTel instruments (NO tenant/doc/
    query labels — that would explode Prometheus at ~10M tenants): query search-duration
    histogram {mode,degraded,cache,outcome} with buckets straddling the P90≤5s SLO, cache
    hit/miss, degradation-rung counters; pipeline records/stage-duration + the
    `asker_index_doc_age_seconds` freshness proxy (1800s SLA bucket); the authoritative
    `asker_pipeline_deadletter_total{origin_topic}` zero-data-loss signal (kafkautil quarantine
    site); otelgrpc on query.
  - **Synthetic data generator (wave 0, tools/synthgen):** deterministic, resume-safe, mixed-type
    per-tenant corpus, planned to 100K tenants / TB via `--dry-run` (defaults true so it can't
    feed by accident); vespa-direct + gateway-upload feeders; qzx rare tokens + per-tenant
    isolation markers. Added a `--tenant-id` override (the review fix below).
  - **Observability (wave 1, OPT-IN — not in `make dev-up`, gated in Helm, memory-capped):** a
    Prometheus+Grafana compose overlay + gated chart templates; 2 Grafana SLO dashboards + 12
    Prometheus alerts committed IN the chart (`files/observability/**`, embedded via .Files.Glob,
    same files the compose overlay mounts — single source of truth); a Prometheus→health-port
    scrape NetworkPolicy per service.
  - **Load suites + reporting (wave 1):** k6 stepped query suite (P90≤5s gate + max-sustained-RPS
    read-out for the 50K-TPS extrapolation) and ingest/freshness suite (edit→searchable P90<30min);
    run-load.sh orchestrator + soak; docs/capacity.md scaling model; docs/loadtest-report.md
    template; ADR-017; a nightly/manual load.yml.
  - **Review fixes (all 13):** the BLOCKER — the load suite seeded `synthgen-*` tenants but queried
    as `alice` (a different streaming group) so **every query returned 0 hits**, measuring
    empty-result no-ops, not search. Fixed end-to-end: synthgen `--tenant-id`, run-load.sh now
    resolves the querying tenant via `GET /v1/me` and seeds the corpus INTO it, and
    `CHECK_EXACT_HITS` (now wired to a hard `asker_exact_hit_mismatch==0` threshold) asserts
    exactly 1 hit so a regression to empty fails loudly. The soak now drives **concurrent ingest**
    (a read-only soak can't exercise the deadletter path it gates on). The freshness suite now
    requires `upload_ok_total>0` + `docs_searchable_total>0` (total upload failure was passing
    vacuously). The query latency histogram now records **failed** searches too (outcome=error),
    so the P90 SLO alert sees slow-error brownouts. index-writer's health mux is wrapped in the RED
    middleware (its 5xx alert was silently uncovered). A Grafana→Prometheus ingress NetworkPolicy
    was added (under default-deny the dashboards couldn't reach Prometheus). ADR-017 corrected
    (`doc_type` is index-writer-only). 3 findings refuted (a topic-label nit, unauth `/metrics` on
    the gateway, an fp32/fp16 wording quibble).
- Next: **M6 — Hardening.** Pen-test-style security review checklist (authz matrix, SSRF in
  connector fetchers, token vault, injection), GDPR delete drill, quota/abuse controls, admin
  console, runbooks for the top-10 failure modes. Exit: ship-review doc signed off; a new engineer
  can deploy + operate from docs alone.
- Known issues:
  - The full-scale load (k6 stepped to cluster max) + the 2-hour soak + a live Prometheus/Grafana
    + NetworkPolicy enforcement run in CI / on a real cluster, NOT on the 8GB dev VM (k6 isn't even
    installed locally). The k6 scripts are validated with `node --check`, run-load.sh with
    `bash -n`, the chart with helm-lint+kubeconform, dashboards with json.tool, and alert rules
    with promtool (in CI). `docs/loadtest-report.md` is a TEMPLATE the CI run fills; its numbers
    are clearly-marked samples until a real run lands.
  - The query SLO is measured on ONE representative tenant (each query scans exactly one streaming
    group, so per-tenant corpus size is the right unit); multi-tenant storage scale is the
    capacity-model's job (`docs/capacity.md`), and the soak's zero-data-loss deadletter scrape
    needs index-writer `:9701/metrics` reachable (compose doesn't publish health ports → it
    degrades to a warning unless run in-network).
  - Observability is opt-in (`make obs-up`; Grafana 127.0.0.1:13000, Prometheus :19090; dev-only
    creds). The OTLP push path is still available but no collector is deployed by default.

## 2026-06-13 — M4 production deployment (complete; kind chaos runs in CI)

- Done: **M4 complete — cloud-agnostic Helm umbrella chart `deploy/helm/asker` deploys the full
  stack and the kind chaos test (the exit criterion) is wired in CI.** Built in 3 waves
  (stateless workloads → stateful/Strimzi/Vespa/Vault + default-deny NetworkPolicies + TLS
  scaffolding → CI kind+chaos + runbooks), then an adversarial review (5 dimensions, 23 agents,
  every finding skeptic-verified): **13 confirmed / 5 refuted**, all 13 fixed.
  - **Chart:** 9 stateless services (Deployment/Service/HPA/PDB/Ingress from one `_workload.tpl`),
    self-hosted stateful deps (postgres/redis/minio/keycloak/tei, gated `<dep>.deploy`), Strimzi
    Kafka CRs (`kafka.strimzi.enabled`), multi-group streaming Vespa StatefulSet (`vespa.deploy`),
    dev Vault + `crypto.NewVaultKEK` (`vault.deploy`), default-deny + per-target allow
    NetworkPolicies, cert-manager TLS scaffolding. `values.yaml` is the authoritative interface;
    `values-dev.yaml` (single node) and `values-ci.yaml` (slim query-path-only kind profile).
    Validates: `helm lint` clean; kubeconform **default 69 / dev 52 / ci 24** + features-on 87
    valid (6 CRDs skipped), 0 errors.
  - **Exit criterion:** `.github/workflows/k8s.yml` stands up a kind cluster, builds+loads the app
    images, `helm install` (slim CI profile), and runs `tools/e2e/k8s-chaos.sh`, which kills one
    query pod and one gateway pod under a continuous retrying `/v1/search` loop and asserts no query
    fails beyond retry + pods reschedule Ready. Runs in CI only (the 8GB dev VM can't host kind).
  - **Review fixes (all 13):** (blockers) the CI job was structurally **unpassable** — query
    `/readyz` live-pings Vespa `:8080` which only serves after the app package activates, but the
    package was activated *after* `helm install --wait` and *after* the query-rollout wait →
    reordered the chaos script (Vespa StatefulSet Ready on `:19071` → activate package → *then*
    gate query/gateway) and dropped `--wait`; **Vespa NetworkPolicy** selected `component: vespa`
    but the pods are `component: search` (zero-pod match → query/index-writer→Vespa denied under
    enforced netpol) → centralized the per-dep selector in `asker.netpol.depSelectorBody` (search
    for Vespa, `strimzi.io/cluster` for Kafka, part-of+component for the rest) and gave Vespa pods
    the full chart labels; **missing `gateway→keycloak` allow** (JWKS fetch denied → token verify
    fails on a cache miss) → added the keycloak ingress allow + gateway egress + ADR-016 matrix
    row. (major) **KEK at `/keys`** was a read-only Secret mount on a read-only rootfs so
    `NewFileKEK` self-create and connector-hub's file DEK store both fail → KEK volume is now a
    writable `emptyDir` when no Secret is configured (self-create under RO rootfs; durable/shared
    KEK via secretName/PVC/Vault); **Kafka netpol** label mismatch (Strimzi owns broker labels);
    **k8s-docs** falsely claimed `vault.addr` flows to the workload env → corrected (integrator
    must inject `VAULT_ADDR`). (minor) gateway-kill could sever the test's own pinned
    `port-forward` → re-establish after the kill; clip cache mounted at `/root/.cache` but the image
    is nonroot `$HOME=/home/nonroot` (+ added fsGroup) → fixed in chart **and** compose; connector-
    hub `envFrom`'d the whole Secret → scoped to MINIO keys via `secretKeys`/secretKeyRef
    (control-plane → DATABASE_URL only); ADR-015 CI-coverage overstatement + backup-runbook CronJob
    netpol gap → documented.
- Next: **M5 — Scale & SLO verification.** Synthetic data generator (100K tenants, TB-scale), k6
  load suites (query stepped to cluster max + extrapolation math to 50K TPS; ingest 30-min
  freshness under load), query result cache, the degradation ladder tested, `docs/capacity.md`
  finalized, Grafana SLO dashboards + alerts, 2-hour soak with zero data loss.
- Known issues:
  - **NetworkPolicy enforcement and the kind chaos test are not exercised together locally.** The
    8GB dev VM cannot host a kind cluster + the stack, so `make e2e-k8s` runs in CI only; and kind's
    default kindnet CNI does not enforce NetworkPolicy, so the netpol correctness (Vespa/Kafka/
    keycloak selectors) is validated by `helm template` + kubeconform + the rendered-selector
    assertions, not by a live policy-enforcing CNI. Real netpol validation needs Calico/Cilium
    (ADR-016 records this); the selectors now match the actual pod labels (verified in the render).
  - **Vault KEK is provisioned but not wired into the app workloads.** Per ADR-015 §3 (`services/**`
    frozen), the `main.go` provider selection (VAULT_ADDR set → `NewVaultKEK`) is an integrator
    step; the chart configures the dev Vault + Transit derived key but does not inject `VAULT_ADDR`,
    so control-plane/connector-hub stay on the file-KEK until an operator adds it (now documented in
    k8s-docs §4.3 and ADR-015). `platform/crypto/vault.go` is unit-tested (fake Transit), not
    integration-tested in CI.
  - The dev Vault, the chart-rendered dev Secret, and Keycloak `start-dev` are all NON-PRODUCTION
    (loudly marked); prod uses Vault HA + External Secrets + Keycloak `start` + external DB.
  - Backup CronJobs in the runbooks need a netpol identity (label or a dedicated allow) to dial
    postgres/minio under default-deny — documented in the runbooks.

## 2026-06-13 — M3 media pipeline (complete)

- Done: **M3 exit criterion met, verified live: `tools/e2e/m3-media.sh` passes all 16 checks** —
  search a spoken phrase → the VIDEO at the right transcript timestamp (modality `asr`, segment
  `[start,end]`), plus CLIP text→image, OCR, thumbnail serving, and media tenant isolation. M0
  smoke still green (no regression).
  - **Two embedding spaces (ADR-013):** bge-m3 text/OCR/ASR (existing `embedding`) + a new CLIP
    space (`clip_embedding`, CLIP_DIM=512). New `services/clip` (open_clip ViT-B/32, one model →
    both encoders, L2-normalized) serves `/embed/text` (query text→image arm) and `/embed/image`
    (enrich). Vespa schema v2: clip_embedding field, parallel chunk_starts_ms/ends_ms/modalities,
    media summary fields, a `clip` rank profile with `closest()` for matched-chunk resolution.
  - **Media enrich (Python):** IMAGE → Tesseract OCR (bge-m3) + CLIP image embedding + thumbnail;
    AUDIO → faster-whisper (tiny) timestamped ASR chunks; VIDEO → ffmpeg audio→whisper + scene
    keyframes→CLIP + poster. Media bytes flow through the connector-hub `/internal/media`
    decrypt/encrypt endpoint (envelope crypto stays in Go; ADR-013). Chunk vectors route to
    embedding vs clip_embedding by length in the index-writer.
  - **Query:** a CLIP text→image arm merged with the bge-m3 text/hybrid arm (dedup by doc_id),
    media Hit fields (start_ms/end_ms/modality/thumbnail_key), reliable transcript-segment
    anchoring, degradation when clip is down ("clip-unavailable", never fail closed).
  - **Gateway/web:** `/v1/media` authed thumbnail proxy; web media result cards (image
    thumbnails via token-fetched object URLs, video/audio timestamp deep-links, modality badges).
  - **Adversarial review (19 agents) + live integration fixes:** clip needed numpy (image
    preprocessing); ffmpeg keyframe extraction degrades to empty (no dead-letter) on
    short/low-motion clips; a CLIP outage degrades (keeps OCR/ASR text) instead of dead-lettering;
    media-hit attribution reliably anchors to the transcript segment; blob.Get/GetByKey reject
    `..` path traversal; upload classifies DocType by content_type. All with regression tests.
- Next: **M4 — production deployment.** Helm charts (cloud-agnostic), Strimzi Kafka, Vespa
  multi-group on K8s, HPA, PodDisruptionBudgets, default-deny NetworkPolicies, Vault (replaces the
  file-KEK + the internal-network trust model), TLS/mTLS, backup/restore runbooks. Exit: deploys
  to kind/k3d in CI; chaos test (kill any pod) shows no failed queries beyond retry. This is
  infra/YAML-heavy and needs a K8s cluster (kind/k3d) — feasible locally but heavy; the chaos-test
  exit needs a running cluster.
- Known issues:
  - The full 17-service stack (clip+torch ~1.2GB, enrich+whisper) is over the 8GB dev VM's budget:
    the clip container OOM-killed mid-e2e on the first attempts. The CLIP-degradation fix means the
    ASR exit criterion indexes regardless, and the suite passes when clip stays up (it did on the
    clean re-run after freeing fake-gmail). On this VM, expect occasional clip OOM under load;
    `make e2e-m3-media` may need a retry, or the Docker memory bump (≥14GB). CI (16GB) runs it via
    the `e2e-m3-media` job. fake-gmail was stopped during the media e2e to free headroom and
    restored after.
  - The media e2e uploads are content-uniquified per run (trailing run-id bytes) so they survive
    ingest dedupe across local re-runs; the committed fixtures are tiny (≈28KB) and CI-portable
    (no `say` needed — `tools/e2e/gen-media-fixtures.sh` regenerates them on macOS).
  - Recurrence (RRULE) expansion, a real in-browser media player, and pure-visual CLIP recall
    tuning remain refinements.

## 2026-06-13 — M2 connector framework + breadth (complete)

- Done: **M2 exit criteria met — contract tests pass for every connector against recorded
  fixtures (no live API), and the "build a connector in <1 day" tutorial was proven by
  implementing MS Teams from it.** Built in three waves (foundation, API connectors, file
  connectors+UI), each integrated, adversarially reviewed, and committed.
  - **SDK finalized (ADR-010):** out-of-process gRPC plugin transport (`connectors/sdk/plugin`,
    server+client adapters, server-streaming Emit/Checkpoint, parity-tested in-proc == over-the-
    wire) alongside the in-process default.
  - **Contract-test harness (ADR-011):** `connectors/sdk/connectortest` cassette record/replay
    (`RunConnectorContract`, `NewReplayServer`, `ASKER_RECORD=1` recorder that redacts secrets) —
    the offline fixture mechanism every connector uses.
  - **13 connectors, all contract-tested (85–100% pkg coverage):** gmail + upload (M1) plus
    outlook-mail, gdrive (+ACL, the ADR-012 reference), gcal, outlook-cal, slack (Events API
    webhook + HMAC), confluence, jira, s3 (minio-go), ical (hand-written RFC 5545 parser),
    whatsapp-export (chat.txt parser), msteams. All 14 in-proc connectors registered in the hub.
  - **iMessage local agent:** Go CLI reading `chat.db` via `sqlite3` shell-out, uploads via
    `/v1/upload`; contract-tested row→Document core (`internal/imsg`, 100%).
  - **Connector management UI:** catalog + connect form + live instance list (status/phase/
    errors)/delete, in the React app; search UI intact; web build/test/lint green.
  - **Adversarial review (22 agents):** 13 findings fixed with regression tests — doc_id-stability
    bugs (jira mutable-key→immutable-id, whatsapp lineIndex→content-hash, ical feed-url→
    instanceID-scoped, all of which would orphan documents on change; fixed pre-production),
    incremental-fidelity bugs (msteams/slack edits dropping ACL+participants; msteams missing new
    chats; s3 resume dropping the deletion keyset), and minors. The MS Teams build surfaced
    concrete tutorial gaps (real harness API, URL rebasing, multi-resource cursors, tenancy.Context
    construction) — all folded back into docs/connectors/building-a-connector.md.
- Next: **M3 — media pipeline.** Image OCR + CLIP embeddings (text→image search), audio/video →
  faster-whisper transcripts with timestamp-anchored chunks (search hit deep-links to the moment),
  thumbnails, ffmpeg keyframes. This is heavy ML (Python enrich extension) — mind the dev-VM memory
  ceiling (CLIP/Whisper models are large; likely needs the Docker memory bump or CI-only e2e, like
  M1's full e2e).
- Known issues:
  - Connectors are validated by offline contract tests (the spec's M2 bar). Live end-to-end
    sync against real Graph/Google/Atlassian/Slack tenants needs the hub's OAuth authorization-code
    flow (token acquisition/refresh) and real API credentials — deferred (spec §6: ask the human
    for paid/live API keys). The hub already consumes a vault token; only OAuth *acquisition* is
    pending. Webhook push for Graph/Google/Atlassian sources is likewise deferred (needs public
    notification endpoints + subscription lifecycle); Slack push is implemented.
  - SSRF: tenant-supplied connector URLs (s3 endpoint, ical feed_url, whatsapp export_url,
    confluence/jira base_url) are fetched server-side without host/IP validation. Accepted under
    the M1 closed-network trust model (ADR-009); harden in M4 (egress policy) and the M6 review.
  - jira issue body caps comments at the search API's first page (documented limitation; full
    comment pagination is a refinement).

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
