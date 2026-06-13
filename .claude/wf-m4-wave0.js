export const meta = {
  name: 'asker-m4-wave0',
  description: 'M4 wave 0: ADR + Helm umbrella chart for the 9 stateless services, helm-lint + kubeconform validated',
  phases: [{ title: 'Build', detail: '3 parallel builders: ADR+chart skeleton, stateless workloads, gateway/web edge+ingress' }],
}

const RESULT = {
  type: 'object',
  required: ['files', 'summary', 'decisions', 'issues'],
  properties: {
    files: { type: 'array', items: { type: 'string' } },
    summary: { type: 'string' },
    decisions: { type: 'array', items: { type: 'string' } },
    issues: { type: 'array', items: { type: 'string' } },
  },
}

const CONTRACT = [
  '# Asker M4 Wave 0 — Shared Build Contract (BINDING)',
  '',
  'Milestone M4 (production deployment) of Asker, a multi-tenant personal search engine. Repo',
  '/Users/dio/works/asker. M0-M3 are COMMITTED and green: the full system runs in Docker Compose',
  '(deploy/compose/docker-compose.yml) — 17 services: gateway, query, ingest, enrich, index-writer,',
  'connector-hub, control-plane, clip, web (the 9 STATELESS app services), plus stateful deps',
  'keycloak, vespa, tei, minio, postgres, redis, redpanda. M4 brings this to Kubernetes via Helm,',
  'cloud-agnostically. Read FIRST:',
  '- specs/asker-v1-personal-search-engine.md (M4 milestone + exit criteria; section 2 architecture; 2.7 scale).',
  '- deploy/compose/docker-compose.yml — the SOURCE OF TRUTH for each service image build, env vars,',
  '  ports, healthchecks, volumes, dependencies. Your K8s objects mirror these exactly.',
  '- docs/adr/ADR-003 (dev/prod deviations: Redpanda dev / Strimzi prod), ADR-005 (EMBEDDING_DIM),',
  '  ADR-009 (internal trust boundary -> mTLS/NetworkPolicy in M4), ADR-013 (CLIP_DIM, clip service).',
  '- README "Dev URLs" + each service health endpoints.',
  '',
  '## Hard rules',
  '- Write ONLY within your owned paths (deploy/helm/** subtrees named in your task, docs/adr/**).',
  '  NEVER modify go.mod, Makefile (the integrator adds make targets), .github, deploy/compose/**,',
  '  platform/**, services/**, connectors/**, web/**, vespa/**. Needs outside scope -> issues.',
  '- Cloud-agnostic: NO provider-specific resources (no cloud LB annotations, no provider StorageClass',
  '  names). Persistence via PVC + a configurable storageClass (default unset -> cluster default).',
  '  Ingress via a configurable ingressClassName (no nginx/traefik-specific annotations in defaults).',
  '- Charts target Helm 3/4 (apiVersion v2). Validate EVERY chart: "helm lint" clean and',
  '  "helm template" renders without error; pipe the rendered manifests through "kubeconform"',
  '  (-strict -ignore-missing-schemas -kubernetes-version 1.29.0) with 0 errors. helm + kubeconform',
  '  are installed (on PATH). Run these and report the exact output in your summary. NO cluster is',
  '  available — do NOT attempt helm install / kubectl apply to a live cluster (that runs in CI, M4',
  '  wave 2). Validation = lint + template + kubeconform only.',
  '- Images: for K8s use image repo+tag VALUES (image.repository / image.tag, default e.g.',
  '  ghcr.io/asker/<svc> + a chart-version tag). Document that CI builds/pushes images; the chart',
  '  references them.',
  '',
  '## Topology the chart must encode (from compose)',
  '9 stateless services. Internal addresses other services dial (must match the compose service DNS,',
  'which becomes the K8s Service name): gateway :8080 (HTTP, public via Ingress); query gRPC :9200 +',
  'health :9201; ingest health :9501; enrich health :9601; index-writer health :9701; connector-hub',
  'HTTP :9300 + health :9301; control-plane gRPC :9100 + health :9101; clip HTTP :9800; web :80',
  '(public via Ingress). They reference deps by Service DNS: redpanda:9092 (Strimzi in M4 wave 1 — a',
  'configurable kafka broker value for now), vespa:8080, tei:80, redis:6379, postgres:5432,',
  'minio:9000, keycloak:8080, clip:9800, connector-hub:9300, control-plane:9100, query:9200.',
  'EMBEDDING_DIM/CLIP_DIM/TEI etc are values. Health probes use each service documented endpoint',
  '(Go services have a -healthcheck binary flag AND a /healthz+/readyz on their health port; clip GET',
  '/health :9800; enrich GET /healthz :9601; web GET /healthz :80).',
  '',
  'Your structured result lists EVERY file written + the helm lint / kubeconform output you observed.',
].join('\n')

phase('Build')

const tasks = [
  {
    label: 'chart-skeleton',
    body: 'Your task: Helm umbrella chart skeleton + ADR-014 + shared templates + values. Owned paths: '
      + 'deploy/helm/asker/Chart.yaml, deploy/helm/asker/values.yaml, deploy/helm/asker/values-dev.yaml, '
      + 'deploy/helm/asker/templates/_helpers.tpl, deploy/helm/asker/templates/NOTES.txt, '
      + 'deploy/helm/asker/.helmignore, deploy/helm/asker/README.md, docs/adr/ADR-014-kubernetes-helm.md. '
      + 'DO NOT write the per-service Deployment/Service templates (the stateless-workloads + edge builders '
      + 'own those). You provide: the chart scaffolding; _helpers.tpl with fullname/labels/selectorLabels/'
      + 'serviceAccountName helpers (the style "helm create" generates); a comprehensive values.yaml '
      + '(global image registry + per-service image repo/tag/replicas/resources/HPA min-max/PDB '
      + 'minAvailable; EMBEDDING_DIM=1024 and CLIP_DIM=512 defaults with a values-dev.yaml override at '
      + '384/512; the dep addresses as values; ingress className+hosts+TLS toggle; storageClass="" '
      + 'default; a Secrets strategy value); the README; and ADR-014 (Status/Context/Decision/'
      + 'Consequences): cloud-agnostic Helm umbrella chart; stateless app services as Deployments with '
      + 'HPA+PDB; stateful deps via operators/StatefulSets (Strimzi Kafka, Vespa, Vault — wave 1); '
      + 'storageClass/ingressClass indirection (no provider lock-in); Secrets via Vault/External Secrets '
      + 'in prod and a chart-managed Secret for dev; the compose stack stays the dev inner loop (ADR-003) '
      + 'while Helm/K8s is prod; consequences (two deployment surfaces kept in sync, image build/push in '
      + 'CI). Validate: once the sibling templates land "helm lint deploy/helm/asker" must be clean; to '
      + 'validate YOUR files in isolation, add a tiny throwaway template referencing the helpers, render '
      + 'with "helm template", then remove it, and report the result. Make values.yaml the authoritative '
      + 'interface the other two builders code against — document every key in the README.',
  },
  {
    label: 'stateless-workloads',
    body: 'Your task: Deployment/Service/HPA/PDB templates for the 7 internal stateless services. Owned '
      + 'paths: deploy/helm/asker/templates/workloads/** (one file per service, or a shared _deployment.tpl '
      + 'driven by per-service values — keep it DRY and readable). Services: query, ingest, enrich, '
      + 'index-writer, connector-hub, control-plane, clip. For EACH: '
      + '(1) Deployment: image from values (repository:tag), replicas from values, the EXACT env vars from '
      + 'docker-compose.yml (KAFKA_BROKERS, the *_ADDR / *_HEALTH_ADDR, VESPA_URL, TEI_URL, REDIS_ADDR, '
      + 'DATABASE_URL, MINIO_*, CLIP_URL, HUB_MEDIA_URL, EMBEDDING_DIM, CLIP_DIM, CONTROL_PLANE_GRPC_ADDR, '
      + 'etc.) sourced from a shared ConfigMap (envFrom) + values; secrets (MinIO keys, DB creds, KEK path) '
      + 'from a Secret ref (dev Secret is chart-managed; prod uses Vault in wave 1); resources requests/'
      + 'limits from values; liveness+readiness probes using each service real mechanism — prefer httpGet '
      + 'on the documented health port (query :9201, connector-hub :9301, control-plane :9101, ingest '
      + ':9501, index-writer :9701, /healthz + /readyz), clip httpGet /health :9800, enrich httpGet '
      + '/healthz :9601; generous initialDelay for enrich/clip (model load); securityContext '
      + '(runAsNonRoot, drop ALL caps, readOnlyRootFilesystem where the image allows), '
      + 'terminationGracePeriodSeconds, a ServiceAccount from the helpers. '
      + '(2) Service: ClusterIP exposing the gRPC/HTTP/health ports each serves, named to MATCH the compose '
      + 'service DNS the others dial (query, control-plane, connector-hub, clip, ...). '
      + '(3) HPA (autoscaling/v2, CPU target from values, behind a values toggle) for the scalable ones '
      + '(query, ingest, enrich, index-writer, connector-hub) and a PodDisruptionBudget (minAvailable from '
      + 'values) for each. '
      + '(4) connector-hub mounts the shared KEK at /keys (dev Secret/emptyDir; Vault in wave 1). '
      + 'Validate: "helm lint deploy/helm/asker", "helm template deploy/helm/asker" renders, pipe to '
      + 'kubeconform (-strict -ignore-missing-schemas -kubernetes-version 1.29.0) = 0 errors; report the '
      + 'output. Code against the chart-skeleton values.yaml; if a key is missing add it under your service '
      + 'block and note it.',
  },
  {
    label: 'edge-ingress',
    body: 'Your task: the public edge — gateway + web Deployments/Services + Ingress + shared ConfigMap + '
      + 'dev Secret + ServiceAccounts. Owned paths: deploy/helm/asker/templates/edge/** and '
      + 'deploy/helm/asker/templates/config/** (the shared ConfigMap, dev Secret, ServiceAccounts). '
      + '(1) gateway Deployment/Service: env from compose (GATEWAY_ADDR :8080, OIDC_ISSUER, OIDC_JWKS_URL, '
      + 'OIDC_AUDIENCE, QUERY_GRPC_ADDR=dns:///query:9200, CONTROL_PLANE_GRPC_ADDR=dns:///control-plane:9100, '
      + 'HUB_HTTP_URL=http://connector-hub:9300, REDIS_ADDR, RATE_LIMIT_PER_MINUTE, CORS_ALLOWED_ORIGINS, '
      + 'MAX_UPLOAD_MB, MAX_MEDIA_MB), httpGet liveness/readiness on /healthz and /readyz :8080, '
      + 'resources/HPA/PDB/securityContext, ServiceAccount. OIDC_ISSUER/JWKS + CORS origins are VALUES '
      + '(prod uses real ingress hostnames, not localhost:8081/3000 — dev defaults keep the compose values). '
      + '(2) web Deployment/Service: nginx image, httpGet /healthz :80; note in values/README that VITE_* '
      + 'are baked at image build (prod needs an image built with prod URLs or runtime config — the known '
      + 'M3 limitation). '
      + '(3) Ingress (networking.k8s.io/v1): ingressClassName from values (no provider annotations in '
      + 'defaults), host-based routing gateway-host -> gateway:8080 and web-host -> web:80; a TLS section '
      + 'gated behind a values toggle referencing a Secret (cert-manager/issuer is prod config — note it). '
      + '(4) a shared ConfigMap for non-secret config (dep addresses, EMBEDDING_DIM/CLIP_DIM, etc.) that '
      + 'workloads envFrom, and a dev Secret (MinIO asker-minio/asker-minio-secret, Postgres asker/asker, '
      + 'Keycloak admin) clearly labelled dev-only with a "replace with Vault in prod" comment; '
      + 'ServiceAccounts for the app services. '
      + 'Validate: helm lint + helm template + kubeconform (-strict -ignore-missing-schemas '
      + '-kubernetes-version 1.29.0) = 0 errors; report output. Coordinate with the chart-skeleton values.yaml.',
  },
]

const results = await parallel(
  tasks.map((t) => () => agent(CONTRACT + '\n\n' + t.body, { label: t.label, phase: 'Build', schema: RESULT }))
)
const out = {}
tasks.forEach((t, i) => { out[t.label] = results[i] || { summary: 'AGENT FAILED', files: [], decisions: [], issues: ['no return'] } })
return out
