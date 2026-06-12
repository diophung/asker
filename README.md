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

**Current state (M0):** monorepo scaffold, platform libraries, the full dev infrastructure
stack in Docker Compose, and a hello-world gateway with OIDC token validation. The connector
hub, ingest/enrich/index pipeline, query service, and web UI arrive in later milestones — see
[MILESTONES.md](MILESTONES.md).

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
| Gateway | http://localhost:8080 | OIDC bearer token (see below) |
| Keycloak admin | http://localhost:8081 | `admin` / `admin` |
| Vespa query + document API | http://localhost:8082 | — |
| Vespa config server | http://localhost:19071 | — |
| TEI (text embeddings) | http://localhost:8083 | — |
| MinIO S3 API | http://localhost:9000 | `asker-minio` / `asker-minio-secret` |
| MinIO console | http://localhost:9001 | `asker-minio` / `asker-minio-secret` |
| PostgreSQL | localhost:15432 | `asker` / `asker`, db `asker` |
| Redis | localhost:16379 | — |
| Redpanda (Kafka API) | localhost:19092 | — |

Dev users in the Keycloak `asker` realm: `alice` / `password123` and `bob` / `password123`.
The public client `asker-web` has Direct Access Grants enabled, so a dev token is one curl away:

```sh
curl -s http://localhost:8081/realms/asker/protocol/openid-connect/token \
  -d grant_type=password -d client_id=asker-web \
  -d username=alice -d password=password123
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
- [docs/security.md](docs/security.md) — tenancy model and dev-vs-prod security gaps
- [docs/adr/](docs/adr/) — architecture decision records
- [docs/runbooks/](docs/runbooks/) — operational runbooks (written in M6)
