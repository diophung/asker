# Asker dev stack (Docker Compose)

Local development stack for Asker M0. All credentials in this directory are **dev-only** —
never reuse them outside a local environment.

Bring the stack up from the repo root:

```sh
make dev-up      # compose up -d --build --wait, then deploys the Vespa app package
make dev-logs    # tail logs
make dev-down    # tear down
```

`docker compose up --wait` gates on the healthchecks below: it only returns success once
**every** service reports healthy, so `make dev-up` is a reliable barrier before
`vespa/deploy.sh` and `make e2e-smoke` run.

## Services

| Service  | Image                                                   | Host ports                          | Credentials (dev-only)                 | Healthcheck |
| -------- | ------------------------------------------------------- | ----------------------------------- | -------------------------------------- | ----------- |
| gateway  | built from `services/gateway/Dockerfile` (distroless)   | 8080 (HTTP)                         | — (OIDC via Keycloak)                  | `/gateway -healthcheck` self-probe of `http://127.0.0.1:8080/healthz` (no shell in image) |
| keycloak | `quay.io/keycloak/keycloak:26.3`                        | 8081 (HTTP)                         | admin console: `admin` / `admin`       | bash `/dev/tcp` HTTP GET against management port 9000 `/health/ready` (image has bash, no curl) |
| vespa    | `vespaengine/vespa:8`                                   | 8082 (query/doc API), 19071 (config) | —                                      | `curl http://localhost:19071/state/v1/health` (config server only; query port comes alive after app deploy) |
| tei      | `ghcr.io/huggingface/text-embeddings-inference:cpu-1.8` | 8083 (HTTP, container port 80)      | —                                      | `curl http://localhost:80/health` with a 20 min `start_period` |
| minio    | `minio/minio:RELEASE.2025-09-07T16-13-09Z`              | 9000 (S3 API), 9001 (console)       | `asker-minio` / `asker-minio-secret`   | `mc ready local` |
| postgres | `postgres:17`                                           | 15432 (5432 is taken on dev host)   | `asker` / `asker`, db `asker`          | `pg_isready -U asker -d asker` |
| redis    | `redis:7`                                               | 16379 (6379 is taken on dev host)   | —                                      | `redis-cli ping` |
| redpanda | `redpandadata/redpanda:v24.3.11`                        | 19092 (Kafka external), 9644 (admin) | —                                      | `rpk cluster health` + grep `Healthy: true` |

Named volumes: `postgres-data`, `minio-data`, `redpanda-data`, `vespa-var`, `vespa-logs`,
`tei-cache`. `docker compose down -v` wipes all state.

## First start notes

- **TEI downloads the embedding model on first start** (~2.3 GB for the default
  `BAAI/bge-m3`). The healthcheck `start_period` is 20 minutes to accommodate this; the
  model is cached in the `tei-cache` volume so subsequent starts are fast. TEI ships
  **linux/amd64 CPU images only**, so on Apple Silicon it runs under Docker Desktop's
  Rosetta/QEMU emulation (`platform: linux/amd64` is set in the compose file). It works,
  but embedding throughput is modest — fine for dev.
- **Keycloak** imports `keycloak/realm-asker.json` on every start (`--import-realm`,
  strategy IGNORE_EXISTING, so re-imports are no-ops). Realm `asker` ships the public
  client `asker-web` (direct access grants enabled, audience mapper adds `asker-web` to
  access-token `aud`) and dev users `alice` / `password123` and `bob` / `password123`.
- **Vespa** only reports config-server health at `compose up --wait` time. The query/doc
  API on 8082 becomes live after `vespa/deploy.sh` pushes the application package —
  `make dev-up` does both in order.
- **Gateway** waits for Keycloak to be healthy before starting. Token `iss` is the
  host-visible `http://localhost:8081/realms/asker` (pinned via `KC_HOSTNAME`) while the
  gateway fetches JWKS over the compose network from `keycloak:8080` — that is why the
  gateway is configured with an explicit issuer + JWKS URL instead of OIDC discovery.

## Memory footprint

The stack is tuned to fit a small, possibly shared Docker VM (see ADR-003 §5): Vespa runs
with `VESPA_IGNORE_NOT_ENOUGH_MEMORY=true` and trimmed JVM heaps, Keycloak's heap is capped
at 400 MB, Redpanda is pinned to `--memory 512M`, and TEI runs with 2 tokenization workers
and a small max batch. Even so, budget roughly **3–4 GB of free VM memory** for the full
stack. If containers exit with code 137 (OOM-killed), either raise Docker Desktop memory
(Settings → Resources) or set a smaller `TEI_MODEL_ID` (below).

## Overrides

- `TEI_MODEL_ID` — embedding model served by TEI. Default `BAAI/bge-m3`. CI overrides it
  with a small model to avoid the multi-GB download, e.g.:

  ```sh
  TEI_MODEL_ID=sentence-transformers/all-MiniLM-L6-v2 make dev-up
  ```

  For a persistent machine-local override, put it in a gitignored `deploy/compose/.env`
  (compose reads it automatically). The committed default stays `BAAI/bge-m3`; note the
  embedding dimension differs per model, which matters once vector indexing lands (M1).

## Quick smoke

```sh
# password grant for a dev user (dev-only flow)
curl -s -X POST http://localhost:8081/realms/asker/protocol/openid-connect/token \
  -d grant_type=password -d client_id=asker-web \
  -d username=alice -d password=password123
```

The access token has `iss=http://localhost:8081/realms/asker` and `aud=asker-web`.
