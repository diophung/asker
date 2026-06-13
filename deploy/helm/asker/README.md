# Asker Helm umbrella chart

Cloud-agnostic Helm chart that deploys Asker's **9 stateless application services** to
Kubernetes: `gateway`, `query`, `ingest`, `enrich`, `index-writer`, `connector-hub`,
`control-plane`, `clip`, `web`. It mirrors the Docker Compose stack
(`deploy/compose/docker-compose.yml`, the source of truth) so the same service shape — images,
env, ports, health endpoints, inter-service DNS — runs in production.

Stateful dependencies (Kafka via **Strimzi**, **Vespa**, Postgres, Redis, MinIO, Keycloak, TEI,
**Vault**) are operator-/StatefulSet-managed (M4 later waves) and are **not** subcharts here; the
chart references them by Service address via `config.*` values. See `docs/adr/ADR-014-kubernetes-helm.md`.

> Compose (`make dev-up`) is the dev inner loop (ADR-003); this chart is the production / kind /
> k3d integration surface.

## Layout

```
deploy/helm/asker/
├── Chart.yaml              # umbrella chart (apiVersion v2)
├── values.yaml             # AUTHORITATIVE interface — documented below
├── values-dev.yaml         # kind/k3d overrides (embeddingDim 384, 1 replica, no HPA/PDB)
├── .helmignore
├── README.md               # this file
└── templates/
    ├── _helpers.tpl        # fullname / labels / selectorLabels / serviceAccountName + per-service helpers
    ├── NOTES.txt           # post-install summary
    └── config/             # shared ConfigMap + dev Secret (edge wave)
```

The per-service `Deployment`/`Service`/`HPA`/`PDB`/`ServiceAccount` and the `Ingress` are
provided by the stateless-workloads and edge waves; they all code against `values.yaml`.

## Validate (no cluster required)

```sh
helm lint deploy/helm/asker
helm template asker deploy/helm/asker | kubeconform -strict -ignore-missing-schemas -kubernetes-version 1.29.0
# dev profile:
helm template asker deploy/helm/asker -f deploy/helm/asker/values-dev.yaml | kubeconform -strict -ignore-missing-schemas -kubernetes-version 1.29.0
```

## Install (later M4 waves; CI uses kind/k3d)

```sh
# Stand up operators + stateful deps first (Strimzi, Vespa, Vault, Postgres, ...), then:
helm install asker deploy/helm/asker -n asker --create-namespace -f deploy/helm/asker/values-dev.yaml
```

## Images

CI builds and pushes each service image (from the compose Dockerfiles) and the chart references
them. Image reference = `global.imageRegistry` + `/` + `services.<svc>.image.repository` + `:` +
(`services.<svc>.image.tag` or, if empty, the chart `version`). Helper: `asker.image`.

---

## Helpers (`templates/_helpers.tpl`)

The workload waves MUST use these so naming/labels are uniform.

| Helper | Signature | Returns |
| :--- | :--- | :--- |
| `asker.name` | `.` | chart name (override via `nameOverride`) |
| `asker.fullname` | `.` | release-scoped fullname (override via `fullnameOverride`) |
| `asker.chart` | `.` | `name-version` for the `helm.sh/chart` label |
| `asker.labels` | `.` | chart-wide common labels |
| `asker.selectorLabels` | `.` | chart-wide selector labels (stable) |
| `asker.serviceAccountName` | `.` | chart-wide SA name |
| `asker.serviceName` | `(list . "query")` | **bare** Service name (== compose name; dialable DNS) |
| `asker.serviceFullname` | `(list . "query")` | release-prefixed name for Deployment/HPA/PDB |
| `asker.serviceLabels` | `(list . "query")` | common labels + `app.kubernetes.io/component` |
| `asker.serviceSelectorLabels` | `(list . "query")` | selector labels + component |
| `asker.serviceAccountNameFor` | `(list . "query")` | per-service SA name (falls back to chart-wide) |
| `asker.image` | `(list . "query")` | fully qualified image ref (registry/repo:tag) |
| `asker.imagePullPolicy` | `(list . "query")` | per-service pull policy (global fallback) |
| `asker.secretName` | `.` | name of the credentials Secret (all strategies) |
| `asker.configMapName` | `.` | name of the shared ConfigMap (defined in `templates/config/configmap.yaml`) |

**Naming contract:** `asker.serviceName` is the bare key (e.g. `query`) so cross-service DNS
(`query:9200`, `connector-hub:9300`, `clip:9800`, `control-plane:9100`) is identical to compose.
Workload object names use `asker.serviceFullname` (`<release>-query`).

---

## Values reference (`values.yaml` — the authoritative interface)

### `global`
| Key | Default | Meaning |
| :--- | :--- | :--- |
| `global.imageRegistry` | `ghcr.io` | Registry prefix for every image. `""` => repositories used verbatim. |
| `global.imagePullPolicy` | `IfNotPresent` | Default pull policy. |
| `global.imagePullSecrets` | `[]` | dockerconfigjson Secret names for every pod. |
| `global.clusterDomain` | `cluster.local` | Cluster DNS suffix (for FQDN dialing). |

### Top-level naming / SA
| Key | Default | Meaning |
| :--- | :--- | :--- |
| `nameOverride` | `""` | Override chart name. |
| `fullnameOverride` | `""` | Override release fullname. |
| `serviceAccount.create` | `true` | Render ServiceAccount(s). |
| `serviceAccount.name` | `""` | Chart-wide SA name (`""` => fullname). |
| `serviceAccount.annotations` | `{}` | Applied to every SA (no provider defaults). |
| `serviceAccount.automountServiceAccountToken` | `false` | Token mount (off for distroless). |

### `config` — shared, non-secret ConfigMap (every workload `envFrom`s it)
Keys are the **exact env var names** the binaries read (compose is the source of truth).
| Key | Default | Env var | Meaning |
| :--- | :--- | :--- | :--- |
| `config.embeddingDim` | `1024` | `EMBEDDING_DIM` | bge-m3 text dim (ADR-005). Must match TEI model + Vespa schema. |
| `config.clipDim` | `512` | `CLIP_DIM` | CLIP image/text dim (ADR-013). |
| `config.kafkaBrokers` | `redpanda:9092` | `KAFKA_BROKERS` | Kafka API. Strimzi bootstrap in prod (ADR-003 §1). |
| `config.vespaUrl` | `http://vespa:8080` | `VESPA_URL` | Vespa query + document API. |
| `config.teiUrl` | `http://tei:80` | `TEI_URL` | TEI text-embeddings server. |
| `config.redisAddr` | `redis:6379` | `REDIS_ADDR` | Redis (host:port). |
| `config.clipUrl` | `http://clip:9800` | `CLIP_URL` | CLIP model service. |
| `config.hubHttpUrl` | `http://connector-hub:9300` | `HUB_HTTP_URL`, `HUB_WEBHOOK_BASE`, `HUB_MEDIA_URL` | connector-hub HTTP base. |
| `config.queryGrpcAddr` | `dns:///query:9200` | `QUERY_GRPC_ADDR` | query gRPC target. |
| `config.controlPlaneGrpcAddr` | `dns:///control-plane:9100` | `CONTROL_PLANE_GRPC_ADDR` | control-plane gRPC target. |
| `config.postgresHost` | `postgres` | — | Postgres host (DATABASE_URL is composed in the Secret). |
| `config.postgresPort` | `5432` | — | Postgres port. |
| `config.postgresDb` | `asker` | — | Postgres database. |
| `config.minioEndpoint` | `minio:9000` | `MINIO_ENDPOINT` | MinIO/S3 endpoint (host:port). |
| `config.minioUseSsl` | `"false"` | `MINIO_USE_SSL` | TLS to MinIO. |
| `config.blobBucket` | `asker-blobs` | `BLOB_BUCKET` | Blob bucket name. |
| `config.otelExporterOtlpEndpoint` | `""` | `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP collector (empty disables export). |

### `sharedEnv` — names of the shared ConfigMap/Secret workloads `envFrom`
Cross-wave contract: the edge wave's `templates/config/**` render the shared ConfigMap + Secret;
the stateless-workloads templates `envFrom` them by these names. Leave both `""` to use the chart
defaults.
| Key | Default | Meaning |
| :--- | :--- | :--- |
| `sharedEnv.configMapName` | `""` | Shared ConfigMap name (`""` => chart default). Workloads dereference this. |
| `sharedEnv.secretName` | `""` | Shared Secret name (`""` => `asker.secretName`). |

### `services.<svc>` — per-service (svc ∈ gateway, query, ingest, enrich, index-writer, connector-hub, control-plane, clip, web)
The **map key is the Service name and the dialable DNS name** (== compose name). Common shape:
| Key | Type / default | Meaning |
| :--- | :--- | :--- |
| `enabled` | bool, `true` | Render this service (gateway/web have no explicit flag; default true). |
| `image.repository` | string | Image repo (e.g. `asker/query`). |
| `image.tag` | string, `""` | Tag; `""` => chart `version`. |
| `image.pullPolicy` | string, `""` | `""` => `global.imagePullPolicy`. |
| `replicaCount` | int | Deployment baseline / floor before HPA. |
| `port` | int | Single primary port (gateway 8080, web 80) — convenience alongside `ports`. |
| `ports[]` | list | `{ name, port, protocol, appProtocol? }`. Service exposes the same. |
| `service.type` | string, `ClusterIP` | Service type (always ClusterIP; Ingress fronts public svcs). |
| `health.liveness` / `health.readiness` | object | `{ type: httpGet\|exec\|grpc, port: <name>, path? }`. |
| `serviceAccount.name` | string, `""` | Per-service SA override (`""` => chart-wide). |
| `resources.requests` / `resources.limits` | object | CPU/memory. |
| `autoscaling.enabled` | bool | Render an HPA. |
| `autoscaling.minReplicas` / `maxReplicas` | int | HPA bounds. |
| `autoscaling.targetCPUUtilizationPercentage` | int | HPA CPU target. |
| `autoscaling.targetMemoryUtilizationPercentage` | int | HPA memory target. |
| `pdb.enabled` | bool | Render a PodDisruptionBudget. |
| `pdb.minAvailable` | int | PDB floor. |
| `podSecurityContext` / `securityContext` | object | Hardened defaults; `readOnlyRootFilesystem` false where the image writes (enrich, clip, web). |
| `nodeSelector` / `tolerations` / `affinity` / `podAnnotations` | — | Scheduling / annotations. |
| `config` | object | Per-service literal env beyond the shared ConfigMap (see below). |

**Per-service ports & health (from compose / topology):**
| svc | ports | health probe | notes |
| :--- | :--- | :--- | :--- |
| `gateway` | http 8080 | httpGet `/healthz`,`/readyz` :8080 | public via Ingress; Go `-healthcheck` flag |
| `query` | grpc 9200, health 9201 | httpGet `/healthz`,`/readyz` :9201 | gRPC server |
| `ingest` | health 9501 | httpGet `/healthz`,`/readyz` :9501 | Kafka consumer |
| `enrich` | health 9601 | httpGet `/healthz` :9601 | Python worker (no `-healthcheck`) |
| `index-writer` | health 9701 | httpGet `/healthz`,`/readyz` :9701 | Kafka consumer |
| `connector-hub` | http 9300, health 9301 | httpGet `/healthz`,`/readyz` :9301 | |
| `control-plane` | grpc 9100, health 9101 | httpGet `/healthz`,`/readyz` :9101 | gRPC server |
| `clip` | http 9800 | httpGet `/health` :9800 | Python model service |
| `web` | http 80 | httpGet `/healthz` :80 | nginx; public via Ingress |

**Per-service `config` literal env:**
- `gateway.config`: `oidcIssuer`, `oidcJwksUrl`, `oidcAudience`, `rateLimitPerMinute`,
  `corsAllowedOrigins`, `maxUploadMb`, `maxMediaMb`.
- `query.config`: `queryAddr`, `queryHealthAddr`.
- `ingest.config`: `ingestHealthAddr`.
- `enrich.config`: `enrichHealthAddr`.
- `index-writer.config`: `indexWriterHealthAddr`.
- `connector-hub.config`: `hubAddr`, `hubHealthAddr`, `connectorSyncInterval`, `schedulerTick`.
- `control-plane.config`: `controlPlaneAddr`, `controlPlaneHealthAddr`.
- `clip.config`: `clipPort`, `clipModel`, `clipPretrained`.
- `web`: no `config` (VITE_* URLs are baked at image build time — see note in `values.yaml`).

### `ingress` — public edge (edge wave)
| Key | Default | Meaning |
| :--- | :--- | :--- |
| `ingress.enabled` | `true` | Render the Ingress. |
| `ingress.className` | `""` | IngressClass; `""` => cluster default. **No provider class in defaults.** |
| `ingress.annotations` | `{}` | Per-env annotations (cert-manager/class) — **none by default.** |
| `ingress.gatewayHost` | `gateway.asker.local` | Host routed to the gateway Service :8080. |
| `ingress.webHost` | `app.asker.local` | Host routed to the web Service :80. |
| `ingress.tls.enabled` | `false` | Emit `spec.tls`. |
| `ingress.tls.secretName` | `asker-edge-tls` | Pre-provisioned cert Secret (chart mints no certs). |

### `persistence` — chart-wide storage default
| Key | Default | Meaning |
| :--- | :--- | :--- |
| `persistence.storageClass` | `""` | `""` => cluster default StorageClass. **No provider class in defaults.** App services are stateless; this is the interface for wave-1 stateful charts. |

### `secrets` — credential delivery strategy (ADR-014)
| Key | Default | Meaning |
| :--- | :--- | :--- |
| `secrets.strategy` | `chart` | `chart` (dev Secret rendered) \| `external` (External Secrets Operator) \| `vault` (Vault Agent/CSI — prod). |
| `secrets.name` | `""` | Secret name override; `""` => `<fullname>-secrets` (`asker.secretName`). |
| `secrets.dev.minioAccessKey` | `asker-minio` | DEV-ONLY (used only when `strategy: chart`). |
| `secrets.dev.minioSecretKey` | `asker-minio-secret` | DEV-ONLY. |
| `secrets.dev.postgresUser` | `asker` | DEV-ONLY (composes `DATABASE_URL`). |
| `secrets.dev.postgresPassword` | `asker` | DEV-ONLY. |
| `secrets.dev.keycloakAdminUser` | `admin` | DEV-ONLY. |
| `secrets.dev.keycloakAdminPassword` | `admin` | DEV-ONLY. |

> All three strategies resolve to a Secret of the **same name** (`asker.secretName`), so
> workloads reference one name and never branch on strategy. **Never ship the dev literals to a
> real cluster** — set `secrets.strategy=vault` (or `external`) in production.

## `values-dev.yaml` (kind/k3d)
Overrides for a single-node cluster: `config.embeddingDim: 384` (bge-small, ADR-005),
`global.imageRegistry: ""` (locally built images), one replica per service, and
`autoscaling`/`pdb` disabled (a 1-node cluster cannot satisfy a PDB during drain).
