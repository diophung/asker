# Asker

Asker is "Google for your own data" — a personal search engine. A user connects their personal
data sources (email, chat, files, calendars, wikis, images, video), Asker continuously indexes
them, and the user searches everything from a single query box that blends semantic (vector)
search with keyword search. The system is built for strict per-user isolation at large scale:
up to 10M tenants, 10PB of raw source data, 50K queries/second peak, P90 query latency ≤ 5s,
and new data searchable within 30 minutes. See the full product spec in
[`specs/asker-v1-personal-search-engine.md`](specs/asker-v1-personal-search-engine.md).

## Architecture at a glance

```
 Sources ──OAuth──▶ ┌─────────────────────────────────────────────┐
 (Gmail, Slack, …)  │ CONNECTOR HUB                               │
                    │ scheduler · sync-state store · token vault  │
                    └──────────────┬──────────────────────────────┘
                                   │ canonical Documents
                                   ▼
                       Kafka  docs.raw (keyed by tenant_id)
                                   ▼
                    ┌──────────────────────────────┐
                    │ INGEST WORKERS               │  parse · dedupe · chunk
                    └──────────────┬───────────────┘
                                   ▼  docs.chunked
                    ┌──────────────────────────────┐
                    │ ENRICH WORKERS               │  text embeddings (TEI),
                    └──────────────┬───────────────┘  CLIP/OCR, Whisper ASR
                                   ▼  docs.enriched
                    ┌──────────────────────────────┐
                    │ INDEX WRITERS ──▶ VESPA      │  streaming mode, document
                    └──────────────────────────────┘  groups keyed by tenant_id

 User ──▶ Gateway (OIDC authn, rate-limit) ──▶ Query Service ──▶ Vespa (tenant-scoped)
                                                    │
                                                    ▼
                                              Redis (cache)

 Blobs: MinIO/S3    Control plane: PostgreSQL    AuthN: Keycloak    Observability: OTel
```

The defining design constraint: this is **not** one 10PB index, it is ~10M small private
corpora. Vespa streaming mode scopes every query to a single tenant's document group, and the
shared `platform/tenancy` library is the only way to construct a data-access context. Details
in [docs/architecture.md](docs/architecture.md).

**Current state (M0–M6 complete):** the full V1 is built. The vertical slice (Gmail/upload
connectors → connector hub → Kafka → ingest/chunk → embed → Vespa streaming index → query +
React UI) runs end to end; 13 source connectors with a contract-test harness (M2); the media
pipeline (CLIP/OCR/Whisper, spoken-phrase → video@timestamp) (M3); a cloud-agnostic Helm chart
with a kind chaos test in CI (M4); SLO metrics + Grafana dashboards + k6 load/soak tooling (M5);
and the security hardening — app-layer SSRF guard, per-tenant GDPR delete cascade with crypto-shred,
quotas, a prod fail-closed Vault KEK, an admin API, and the [ship review](docs/ship-review.md) (M6).
The full-scale load/soak, kind chaos, and NetworkPolicy enforcement run in CI (the 8GB dev VM can't
host them). See [MILESTONES.md](MILESTONES.md), [PROGRESS.md](PROGRESS.md), and the
[threat model](docs/security.md).

## Quickstart

Prerequisites: **Go 1.25+**, **Docker** (with Compose v2), and `curl`, `zip`, `python3`
on PATH (used by `vespa/deploy.sh` and `tools/e2e/smoke.sh`; all three ship with macOS
and the GitHub Actions runners).

```sh
make tools      # install dev tooling
make dev-up     # build + start the compose stack, wait healthy, deploy the Vespa app
make e2e-smoke  # end-to-end smoke test against the running stack
```

Notes:
- First `make dev-up` downloads the TEI embedding model (~2.3GB) — allow up to 15+ minutes.
- `make dev-down` tears the stack down; `make dev-logs` tails service logs.
- Other targets: `make build`, `make test`, `make lint`, `make vet`, `make fmt`,
  `make proto`, `make coverage-gate`, `make vespa-deploy`.

## Dev URLs and credentials

All credentials below are **dev-only** and hardcoded in the compose stack. Never reuse them.

| Service | Address | Credentials (dev-only) |
| :--- | :--- | :--- |
| Web UI | http://localhost:3000 | log in as a dev user (below) |
| Gateway | http://localhost:8080 | OIDC bearer token (see below) |
| Keycloak admin | http://localhost:8081 | `admin` / `admin` |
| Vespa query + document API | http://localhost:8082 | — |
| Vespa config server | http://localhost:19071 | — |
| TEI (text embeddings) | http://localhost:8083 | — |
| fake-gmail (dev-only Gmail fake + admin API) | http://localhost:9400 | bearer `fake-gmail-token:<email>` (Gmail API); admin API unauthenticated |
| MinIO S3 API | http://localhost:9000 | `asker-minio` / `asker-minio-secret` |
| MinIO console | http://localhost:9001 | `asker-minio` / `asker-minio-secret` |
| PostgreSQL | localhost:15432 | `asker` / `asker`, db `asker` |
| Redis | localhost:16379 | — |
| Redpanda (Kafka API) | localhost:19092 | — |
| Prometheus (opt-in: `make obs-up`) | http://localhost:19090 | — |
| Grafana SLO dashboards (opt-in: `make obs-up`) | http://localhost:13000 | `admin` / `admin` |

The observability stack (Prometheus + Grafana) is **opt-in** and not part of `make dev-up` (it
would crowd the dev VM). Start it alongside the running stack with `make obs-up` (stop with
`make obs-down`); the SLO dashboards (query P90 vs the 5 s line, ingest freshness, cache hit rate,
degradation ladder, dead-letter / data-loss) provision automatically. See ADR-017 and `docs/capacity.md`.

Dev users in the Keycloak `asker` realm: `alice`, `bob`, and `carol`, all with password
`password123`. The public client `asker-web` has Direct Access Grants enabled, so a dev token
is one curl away:

```sh
curl -s http://localhost:8081/realms/asker/protocol/openid-connect/token \
  -d grant_type=password -d client_id=asker-web \
  -d username=alice -d password=password123
```

## Try it

Run `make dev-up`, open http://localhost:3000, and log in as `alice` / `password123` — that
is the search UI. To give Alice something to search, seed the local fake Gmail and connect it
through the API (connector sync starts within ~30 seconds; a new document is typically
searchable in under a minute):

```sh
# 1. Seed the dev fake Gmail with 50 deterministic synthetic emails for alice.
curl -s -X POST http://localhost:9400/admin/users/alice@example.com/seed \
  -H 'Content-Type: application/json' -d '{"count":50,"seed":1}'

# 2. Get a dev bearer token for alice (password grant, dev-only).
TOKEN=$(curl -s http://localhost:8081/realms/asker/protocol/openid-connect/token \
  -d grant_type=password -d client_id=asker-web \
  -d username=alice -d password=password123 \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')

# 3. Create a Gmail connector instance. base_url is the IN-NETWORK address of
#    the fake — connectors run inside the compose network, not on the host.
ID=$(curl -s -X POST http://localhost:8080/v1/connectors \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"connector_id":"gmail","display_name":"Alice mail","config":{"base_url":"http://fake-gmail:9400","user_email":"alice@example.com"}}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')

# 4. Hand it the (fake) OAuth token — the hub schedules the first sync.
curl -s -X PUT "http://localhost:8080/v1/connectors/$ID/token" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"token":"fake-gmail-token:alice@example.com"}'

# 5. Watch sync progress (phase, docsEmitted), then search — here or in the UI.
curl -s -H "Authorization: Bearer $TOKEN" http://localhost:8080/v1/connectors
curl -s -H "Authorization: Bearer $TOKEN" 'http://localhost:8080/v1/search?q=roadmap'
```

## Repository layout

```
asker/
├── README.md  MILESTONES.md  PROGRESS.md
├── specs/              # product spec
├── docs/               # architecture, capacity, security, ADRs, runbooks
├── platform/           # shared Go libraries
│   ├── proto/          #   protobuf definitions (buf)
│   ├── tenancy/        #   tenant context — the single data-access chokepoint
│   ├── telemetry/      #   OpenTelemetry init + HTTP middleware
│   └── config/         #   env-based config loading
├── services/
│   └── gateway/        # authn edge; query/ingest/enrich/connector-hub/… in M1+
├── vespa/              # Vespa application package + deploy script
├── deploy/
│   └── compose/        # Docker Compose dev stack (helm/ arrives in M4)
├── tools/
│   ├── ci/             # coverage gate
│   └── e2e/            # smoke test
├── connectors/         # (M2) connector SDK + connectors
└── web/                # (M1) React search UI
```

## More documentation

- [MILESTONES.md](MILESTONES.md) — the M0–M6 plan and exit criteria
- [PROGRESS.md](PROGRESS.md) — session-by-session log (read this first each session)
- [docs/architecture.md](docs/architecture.md) — system architecture and technology decisions
- [docs/capacity.md](docs/capacity.md) — scale model (finalized in M5)
- [docs/security.md](docs/security.md) — threat model, authz matrix, SSRF & GDPR guarantees (M6)
- [docs/ship-review.md](docs/ship-review.md) — the M6 exit-criterion sign-off (milestone status, SLO posture, accepted risks, deploy-from-docs checklist)
- [docs/adr/](docs/adr/) — architecture decision records (ADR-001 … ADR-017)
- [docs/runbooks/](docs/runbooks/) — operational runbooks, incl. the [top-10 failure modes](docs/runbooks/top-10-failure-modes.md) (M6)
- [deploy/k8s-docs.md](deploy/k8s-docs.md) — deploy to Kubernetes (Helm umbrella chart, M4)
