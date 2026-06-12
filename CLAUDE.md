# CLAUDE.md

Asker is "Google for your own data" — a personal search engine that indexes a user's private
sources (email, chat, files, calendars, media) and answers blended semantic + keyword queries
with strict per-user isolation. Go monorepo (single module `github.com/asker/asker`, Go 1.25+),
plus a React/TS web UI and Python ML enrich workers. Milestone-driven (M1 in progress); read
`PROGRESS.md` and `MILESTONES.md` at the start of every session and append to `PROGRESS.md` at
the end.

## Commands (use the Makefile, not raw go commands)

- Build / vet / lint / test: `make build` · `make vet` · `make lint` · `make test`
- Single Go test: `go test -race -run '^TestName$' ./services/gateway/...`
- `make fmt` (gofmt) · `make proto` (regenerate protobuf — generated code is committed and
  CI fails on drift) · `make tools` (installs buf, protoc plugins, golangci-lint into `./bin`)
- `make coverage-gate` enforces floors: `platform/tenancy` == 100%, other `platform/*` >= 75%
- Dev stack: `make dev-up` (compose + Vespa deploy; first run downloads a ~2.3GB TEI model) ·
  `make dev-down` · `make dev-clean` (wipe volumes) · `make dev-logs` · `make e2e-smoke`
- Web (`cd web`): `npm run build` (tsc + vite) · `npm test` (vitest) · `npm run lint` (eslint)

## Critical conventions

- **NEVER use bare `go build/test/lint ./...`.** `web/node_modules` contains vendored Go files
  that must not enter the walk. Use the Makefile's `GO_PKGS`:
  `./platform/... ./services/... ./connectors/... ./tools/...`.
- Lint: golangci-lint v2 (errcheck, govet, staticcheck, revive, etc.); US-locale misspell.
  The golangci-lint version is pinned in both `Makefile` and `.github/workflows/ci.yml` — keep
  them in sync. Generated code (`platform/proto/gen`) and `web/` are excluded.
- Languages by concern (do not cross): **Go** for all services; **Python** only for ML enrich
  workers (`services/enrich`); **TypeScript/React** for `web/`.

## Architecture (the one thing to internalize)

The data is NOT one corpus — it is ~10M small, strictly private per-tenant corpora; no query
ever spans tenants. Everything routes by `tenant_id`: Kafka topics keyed by it, Vespa documents
grouped by it (streaming search mode, near-zero per-tenant cost at rest), every query carries a
tenant group selector. Isolation is **structural, not disciplinary**:

- `tenant_id` derives ONLY from the verified OIDC token (ADR-002), never from request bodies.
- `platform/tenancy` is the single chokepoint for constructing a data-access context — code
  that forgets the tenant should not compile against the data-access APIs. Touch it carefully.

Pipeline (async, event-driven over Kafka): Connectors → `docs.raw` → ingest (parse/chunk) →
`docs.chunked` → enrich (TEI embeddings, CLIP/OCR/ASR) → `docs.enriched` → index writers →
Vespa. Query path (sync, latency-budgeted, P90 ≤ 5s): Gateway (OIDC authn) → query service →
Vespa, with Redis cache. The canonical `Document` protobuf in `platform/proto` is the contract
every stage depends on.

## Layout

`platform/` shared libs (proto, tenancy, telemetry, config, crypto, kafkautil) ·
`services/` (gateway, control-plane, enrich) · `connectors/sdk` · `vespa/` (streaming-mode app
package) · `deploy/compose` (dev stack) · `tools/` (ci gate, e2e smoke, fake-gmail) ·
`docs/` (architecture.md, ADRs in `docs/adr/`, runbooks) · `specs/` (product spec).

Dev infra (all bound to 127.0.0.1, dev-only creds — see README "Dev URLs" table): Gateway 8080,
Keycloak 8081 (realm `asker`, users alice/bob `password123`), Vespa 8082, TEI 8083, MinIO
9000/9001, Postgres 15432, Redis 16379, Redpanda 19092.
