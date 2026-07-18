# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Asker is "Google for your own data" — a personal search engine that indexes a user's private
sources (email, chat, files, calendars, media) and answers blended semantic + keyword queries
with strict per-user isolation. Go monorepo (single module `github.com/asker/asker`, Go 1.25+),
plus a React/TS web UI and Python ML enrich workers.

**Status:** V1 is built and shipped — milestones M0–M6 are all complete (see `MILESTONES.md`),
and a v3.2 personalized, intent-aware search layer has since landed on top (see `DECISIONS.md`).
The full vertical slice runs end to end: connectors → Kafka → ingest/chunk → enrich → Vespa
streaming index → query + React UI. Work is still milestone-logged: **read `PROGRESS.md` at the
start of every session** (it is the session-by-session log and source of current truth) and
**append to it at the end**.

## Commands (use the Makefile, not raw go commands)

- Build / vet / lint / test: `make build` · `make vet` · `make lint` · `make test`
- Single Go test: `go test -race -run '^TestName$' ./services/query/...`
- `make fmt` (gofmt) · `make proto` (regenerate protobuf — generated code is committed and
  CI fails on drift) · `make tools` (installs buf, protoc plugins, golangci-lint into `./bin`)
- `make coverage-gate` enforces floors: `platform/tenancy` == 100%, other `platform/*` >= 75%
- Dev stack: `make dev-up` (serial image build + compose up + Vespa deploy; first run downloads
  a ~2.3GB TEI model, allow 15+ min) · `make dev-down` · `make dev-clean` (wipe volumes) ·
  `make dev-logs`
- E2E suites (need a running stack): `make e2e-smoke` · `make e2e-leakage` (**the sacred
  cross-tenant leakage suite — it must always pass**) · `make e2e-m1` · `make e2e-m3-media` ·
  `make e2e-oauth` · `make e2e-gdpr` (DESTRUCTIVE: erases the test tenant)
- Web (`cd web`): `npm run build` (tsc + vite) · `npm test` (vitest) · `npm run lint` (eslint)
- Deploy/scale (rarely needed locally): `make helm-lint` · `make e2e-k8s` (M4 kind chaos) ·
  `make obs-up`/`obs-down` (opt-in Prometheus+Grafana) · `make load-query`/`soak` (M5, need k6) ·
  `make synthgen-dry`. Two-host GPU deploy uses the `pc-*` (RTX engine) and `mac-*` (workers)
  targets — see `docs/two-host-deploy.md`.

## Critical conventions

- **NEVER use bare `go build/test/lint ./...`.** `web/node_modules` contains vendored Go files
  that must not enter the walk. Use the Makefile's `GO_PKGS`:
  `./platform/... ./services/... ./connectors/... ./tools/...`.
- Lint: golangci-lint v2 (errcheck, govet, staticcheck, revive, etc.); US-locale misspell.
  The golangci-lint version is pinned in both `Makefile` and `.github/workflows/ci.yml` — keep
  them in sync. Generated code (`platform/proto/gen`) and `web/` are excluded.
- Languages by concern (do not cross): **Go** for all services; **Python** only for ML enrich
  workers (`services/enrich`); **TypeScript/React** for `web/`.
- **Small Docker VMs OOM easily.** `make dev-up` builds the ~11 images *serially* on purpose
  (parallel BuildKit builds spike memory enough to OOM-kill Vespa, exit 137). The 8GB dev VM
  can't fit the default `BAAI/bge-m3` model or run the full-scale e2e — a gitignored
  `deploy/compose/.env` pins a smaller `TEI_MODEL_ID` locally; heavy suites run in CI. The web
  container can lose a startup DNS race — bring the stack up via the Makefile (web comes up last),
  and run compose scripts under bash, not zsh.

## Architecture (the one thing to internalize)

The data is NOT one corpus — it is ~10M small, strictly private per-tenant corpora; no query
ever spans tenants. Everything routes by `tenant_id`: Kafka topics keyed by it, Vespa documents
grouped by it (streaming search mode, near-zero per-tenant cost at rest), every query carries a
tenant group selector. Isolation is **structural, not disciplinary**:

- `tenant_id` derives ONLY from the verified OIDC token (ADR-002), never from request bodies.
  Asker is a *personal* search engine, so `tenant_id` IS the user — per-tenant and per-user
  coincide, and user-scoped data (preferences, learned weights) inherits the same boundary.
- `platform/tenancy` is the single chokepoint for constructing a data-access context — code
  that forgets the tenant should not compile against the data-access APIs. Touch it carefully.

**Ingest pipeline** (async, event-driven over Kafka): connector-hub (scheduler + token vault) →
`docs.raw` → ingest (parse/dedupe/chunk) → `docs.chunked` → enrich (TEI text embeddings,
CLIP/OCR/Whisper ASR) → `docs.enriched` → index-writer → Vespa. The canonical `Document`
protobuf in `platform/proto` is the contract every stage depends on.

**Query path** (sync, latency-budgeted, P90 ≤ 5s): Gateway (OIDC authn, REST) → query service →
Vespa, with Redis cache. Vespa stays **user-agnostic** (global rank inputs only): it does
candidate generation (hybrid dense + keyword retrieval, hard filters), and **per-user re-ranking
happens in the query service after retrieval** (mirrors the `rerankByRecency` pattern). The v3.2
personalization layer lives here:

- **Query understanding** (`services/query`: `temporal.go`, `intent.go`, `scope.go`) — a
  deterministic rules layer (no LLM) resolves temporal scope to a concrete range and classifies
  intent (`schedule_lookup` / `needs_attention` / `find_item` / `freeform`), applied as hard
  filters + ranking-profile selection.
- **Hybrid retrieval + RRF** (`rrf.go`) — keyword and vector arms fused by Reciprocal Rank
  Fusion, gated by `QUERY_HYBRID_RRF`, personalized path only.
- **Combined relevance** (`platform/personalization`, a *pure, no-I/O* package at the ≥75%
  coverage floor): `w_sem·semantic + w_pref·preference + w_behav·behavioral + w_attn·attention
  − w_fatigue·repetition`, with an online logistic learning-to-rank model, MMR diversification,
  and cold-start defaults (never empty, never random).
- **Persistence split**: preferences/feedback/learned weights are durable in Postgres
  (control-plane, `REFERENCES tenants ON DELETE CASCADE` so GDPR erasure covers them) and
  write-through cached in Redis for the query hot path; a Redis miss falls back to cold-start
  defaults, never an error.
- Toggle the whole layer off with `QUERY_PERSONALIZATION_ENABLED=false` (clean rollback to the
  non-personalized pipeline, which stays byte-for-byte unchanged).

## Layout

`platform/` shared Go libs (proto, tenancy, telemetry, config, crypto, kafkautil, oauth, blob,
safehttp, personalization) · `services/` (gateway, control-plane, connector-hub, ingest,
enrich [Python], index-writer, query, clip) · `connectors/` (sdk + ~13 connectors: gmail,
gdrive, gcal, slack, jira, confluence, outlook-mail/cal, ical, s3, whatsapp-export,
imessage-agent, msteams, upload) · `vespa/` (streaming-mode app package + `deploy.sh`) ·
`deploy/` (`compose/` dev stack, `helm/` umbrella chart) · `tools/` (ci gate, e2e suites, load,
synthgen, fake-gmail, fake-oauth) · `docs/` (architecture.md, capacity.md, security.md,
ship-review.md, ADRs in `docs/adr/` [ADR-001…017], runbooks, openapi) · `specs/` (product specs).

Dev infra (all bound to 127.0.0.1, dev-only creds — see README "Dev URLs" table): Web UI 13001,
Gateway 8080, Keycloak 8081 (realm `asker`, users alice/bob/carol `password123`), Vespa 8082,
TEI 8083, fake-gmail 9400, MinIO 9000/9001, Postgres 15432, Redis 16379, Redpanda 19092. The
Prometheus/Grafana overlay is opt-in (`make obs-up`; Grafana 13000, Prometheus 19090).
