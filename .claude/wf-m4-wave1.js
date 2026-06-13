export const meta = {
  name: 'asker-m4-wave1',
  description: 'M4 wave 1: Strimzi Kafka + Vespa multi-group, stateful deps, Vault + Go KEKProvider, default-deny NetworkPolicies, TLS',
  phases: [{ title: 'Build', detail: '4 parallel builders: stateful deps, messaging+search, vault+crypto, netpol+tls' }],
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
  '# Asker M4 Wave 1 — Shared Build Contract (BINDING)',
  '',
  'Milestone M4 (production deployment), wave 1. Repo /Users/dio/works/asker. M4 wave 0 is COMMITTED:',
  'deploy/helm/asker is a cloud-agnostic Helm umbrella chart with the 9 stateless app services',
  '(Deployments/Services/HPA/PDB/Ingress), a shared ConfigMap asker-env, a dev Secret asker-secrets,',
  'and _helpers.tpl (asker.serviceName = bare compose-DNS name; asker.serviceFullname = release-',
  'prefixed; asker.image; asker.secretName; asker.labels/selectorLabels). values.yaml is the',
  'authoritative interface (read it fully). Wave 1 adds the STATEFUL layer + security. Read FIRST:',
  '- deploy/helm/asker/values.yaml, templates/_helpers.tpl, templates/config/** (the existing contract).',
  '- deploy/compose/docker-compose.yml (the dep images/ports/env/healthchecks: postgres:17 5432,',
  '  redis:7 6379, minio 9000/9001, keycloak 26 8080/9000, tei 80, vespa 8080/19071, redpanda 9092,',
  '  the shared kek-keys volume at /keys, EMBEDDING_DIM/CLIP_DIM).',
  '- docs/adr/ADR-003 (Redpanda dev / Strimzi prod), ADR-013 (clip), ADR-014 (the chart architecture),',
  '  and the M1/security model (per-tenant envelope encryption; platform/crypto KEKProvider +',
  '  FileKEK; the internal-network trust boundary that M4 hardens with NetworkPolicy/mTLS, ADR-009).',
  '- platform/crypto/kek.go + cipher.go (the KEKProvider interface: WrapDEK/UnwrapDEK(ctx, tenantID,',
  '  bytes); FileKEK is the dev shim; you add a Vault impl with the SAME interface).',
  '',
  '## Hard rules',
  '- Write ONLY within your owned paths. NEVER modify go.mod/go.sum/Makefile/.github/deploy/compose/**/',
  '  services/**/connectors/**/web/**/vespa/** or other builders paths or the wave-0 chart files',
  '  (Chart.yaml, values.yaml, _helpers.tpl, templates/workloads|edge|config). You MAY add NEW values',
  '  keys by writing a values fragment ONLY in your own templates default()/comments and noting the',
  '  needed values.yaml additions in issues (the integrator merges them). Go deps are FROZEN — for a',
  '  Vault client use net/http against the Vault HTTP API (no vault SDK in go.mod), like the M2',
  '  connectors did for REST. stdlib testing only.',
  '- Cloud-agnostic: self-hosted StatefulSets + PVCs with a configurable storageClass (default "" =',
  '  cluster default); NO provider-specific resources/annotations. Stateful deps gated behind a',
  '  values toggle (e.g. <dep>.deploy true) so a managed/external instance can be used by flipping it',
  '  off and pointing the address value at the external host.',
  '- Validate K8s YAML: "helm lint deploy/helm/asker" clean; "helm template deploy/helm/asker"',
  '  renders; pipe to "kubeconform -strict -ignore-missing-schemas -kubernetes-version 1.29.0"',
  '  (CRDs like Strimzi Kafka / Vault / Vespa operator kinds are skipped via -ignore-missing-schemas,',
  '  core resources must be Valid, 0 Errors). helm + kubeconform are on PATH. NO live cluster (kind +',
  '  chaos run in CI, wave 2). Report the exact lint/kubeconform output. Go: gofmt, go build + go',
  '  test -race on your package, ./bin/golangci-lint run on it (0 issues, US-locale "canceled").',
  '',
  'Your structured result lists EVERY file written + the validation output you observed.',
].join('\n')

phase('Build')

const tasks = [
  {
    label: 'stateful-deps',
    body: 'Your task: self-hosted stateful dependency StatefulSets in the umbrella chart. Owned path: '
      + 'deploy/helm/asker/templates/stateful/** (postgres, redis, minio, keycloak, tei). For EACH: a '
      + 'StatefulSet (or Deployment for stateless-ish keycloak/tei with a model-cache PVC) using the '
      + 'compose image+env+ports, a headless+ClusterIP Service named to match the compose DNS '
      + '(postgres, redis, minio, keycloak, tei), volumeClaimTemplates / PVCs with storageClass from '
      + 'values (default ""), readiness/liveness probes (postgres pg_isready exec, redis redis-cli '
      + 'ping exec, minio GET /minio/health/live, keycloak the management-port /health/ready, tei GET '
      + '/health), resources from values, and a values toggle <dep>.deploy (default true) so each can '
      + 'be disabled to use a managed instance (the address value then points elsewhere). Postgres '
      + 'creds + MinIO keys come from the existing asker-secrets Secret (asker.secretName); keep dev '
      + 'defaults matching compose (asker/asker, asker-minio/asker-minio-secret). TEI MODEL_ID + a '
      + 'model-cache PVC; the dev profile sets the small model. Memory/heap caps like compose '
      + '(keycloak JAVA_OPTS_KC_HEAP, etc.) as values. Note in issues the values.yaml keys you need '
      + 'added (e.g. postgres.deploy, postgres.storage, redis.*, minio.*, keycloak.*, tei.*). Validate '
      + 'helm lint + template + kubeconform and report. These are the cloud-agnostic self-hosted '
      + 'defaults; a managed-DB deployment flips <dep>.deploy=false.',
  },
  {
    label: 'messaging-search',
    body: 'Your task: Kafka (Strimzi) + Vespa-on-K8s in the umbrella chart. Owned path: '
      + 'deploy/helm/asker/templates/messaging/** and deploy/helm/asker/templates/search/**. '
      + '(1) Strimzi Kafka: a Kafka custom resource (kafka.strimzi.io/v1beta2, KRaft or ZK-less per '
      + 'current Strimzi) sized for prod (3 brokers default via values, ephemeral or persistent-claim '
      + 'storage with storageClass from values), plus KafkaTopic CRs for docs.raw/docs.chunked/'
      + 'docs.enriched/docs.deadletter (partitions from values, default >=512 per spec 2.7 but a much '
      + 'smaller dev default, e.g. 12, in values-dev — note the override needed), gated behind a values '
      + 'toggle kafka.strimzi.enabled. Document that the Strimzi operator must be installed in the '
      + 'cluster (the chart provides the CRs, not the operator) and that the app services bootstrap '
      + 'address (KAFKA_BROKERS) value must point at the Strimzi-created bootstrap Service. Since the '
      + 'app services read KAFKA_BROKERS from the shared config (wave 0), note the value the integrator '
      + 'must set to the Strimzi bootstrap (e.g. asker-kafka-kafka-bootstrap:9092). '
      + '(2) Vespa multi-group content cluster on K8s: a StatefulSet of Vespa nodes (config server + '
      + 'content) — model a configurable number of content groups (values: vespa.groups, '
      + 'vespa.nodesPerGroup) so a hot tenant query touches one group (spec 2.7); a headless Service '
      + '(vespa) for the document/query API :8080 and config :19071, PVCs (storageClass from values) '
      + 'for /opt/vespa/var. Keep it simple but real: document that the application package is deployed '
      + 'by an init/job running vespa/deploy.sh-equivalent against the config server (reference '
      + 'vespa/deploy.sh; the actual deploy Job can be a follow-up — a ConfigMap carrying the app or a '
      + 'note is acceptable, but the StatefulSet + Services + grouping values are the deliverable). '
      + 'Gate behind vespa.deploy. CRDs (Strimzi Kafka/KafkaTopic) are kubeconform-skipped via '
      + '-ignore-missing-schemas. Validate helm lint + template + kubeconform; report. List needed '
      + 'values.yaml keys in issues.',
  },
  {
    label: 'vault-crypto',
    body: 'Your task: HashiCorp Vault as the prod KEK provider — Go impl + Helm + ADR. Owned paths: '
      + 'platform/crypto/vault.go + platform/crypto/vault_test.go (NEW files; do NOT modify kek.go/'
      + 'cipher.go/dekstore.go), deploy/helm/asker/templates/vault/**, docs/adr/ADR-015-vault-kek.md. '
      + '(1) Go: implement a Vault Transit-engine KEKProvider with the SAME interface as FileKEK '
      + '(crypto.KEKProvider: WrapDEK(ctx, tenantID, dek)([]byte,error) and UnwrapDEK(ctx, tenantID, '
      + 'wrapped)([]byte,error)). Use net/http against the Vault HTTP API (no vault SDK in go.mod): '
      + 'POST {VAULT_ADDR}/v1/transit/encrypt/<keyName> and /v1/transit/decrypt/<keyName> with the '
      + 'X-Vault-Token header; bind the tenant ID as Transit "context" (base64) for per-tenant key '
      + 'derivation (convergent/derived keys) so a wrapped DEK is tenant-scoped (mirrors the FileKEK '
      + 'AAD binding); func NewVaultKEK(cfg VaultConfig) (KEKProvider, error) with VaultConfig{Addr, '
      + 'Token, KeyName, HTTPClient optional}. Tests (stdlib): an httptest server emulating the Transit '
      + 'encrypt/decrypt endpoints — round-trip wrap/unwrap, the tenant context is sent (cross-tenant '
      + 'unwrap with the wrong context fails like FileKEK), HTTP error -> error, token sent. >=85% '
      + 'coverage. Document that control-plane/connector-hub select FileKEK vs VaultKEK by env (KEK_FILE '
      + 'vs VAULT_ADDR) — note the small main.go wiring the integrator must add (out of your scope: '
      + 'services/** is frozen). '
      + '(2) Helm: a dev Vault (Deployment, dev mode, the transit engine enabled via an init Job or '
      + 'documented), a Service (vault:8200), and values (vault.deploy toggle, vault.addr, a Secret/'
      + 'serviceaccount for the token) so control-plane/connector-hub can point KEK at Vault in prod; '
      + 'clearly mark dev-mode Vault as NON-production (no unseal/HA) with a prod-hardening note. '
      + '(3) ADR-015 (Status/Context/Decision/Consequences): Vault Transit replaces the file-KEK shim '
      + 'for the prod envelope-encryption KEK; per-tenant Transit context = the M1 AAD binding; '
      + 'consequences (Vault availability is now on the token/blob decrypt path; dev keeps FileKEK). '
      + 'Validate: Go battery on platform/crypto; helm lint + template + kubeconform for the vault '
      + 'templates; report both. Note needed values.yaml keys in issues.',
  },
  {
    label: 'netpol-tls',
    body: 'Your task: default-deny NetworkPolicies + internal TLS scaffolding + the trust-model ADR. '
      + 'Owned paths: deploy/helm/asker/templates/netpol/** and deploy/helm/asker/templates/tls/**, '
      + 'docs/adr/ADR-016-netpol-mtls.md. '
      + '(1) NetworkPolicies (networking.k8s.io/v1), gated behind a values toggle networkPolicy.enabled '
      + '(default true): a default-deny-all-ingress policy for the namespace, then explicit allow '
      + 'policies encoding the real call graph — ingress controller -> gateway:8080 + web:80; gateway '
      + '-> query:9200, control-plane:9100, connector-hub:9300, redis:6379; query -> vespa:8080, '
      + 'tei:80, clip:9800, redis:6379; ingest -> redpanda/kafka:9092, redis:6379; enrich -> kafka, '
      + 'tei:80, clip:9800, connector-hub:9300 (internal media); index-writer -> kafka, vespa:8080; '
      + 'connector-hub -> kafka, control-plane:9100, minio:9000, vault:8200, external (egress for '
      + 'connectors — note SSRF caveat from M2/ADR-012); control-plane -> postgres:5432, vault:8200; '
      + 'clip -> (none inbound except query+enrich); allow DNS egress (kube-dns) for all. Use pod '
      + 'selectors matching the wave-0 asker.selectorLabels (component label per service). Be precise '
      + 'and minimal (least privilege). '
      + '(2) TLS scaffolding: values (tls.internal.enabled, tls.issuer for cert-manager, a per-service '
      + 'cert Secret naming convention) + documentation/templates for internal mTLS between services — '
      + 'since full mTLS needs a mesh/cert-manager, deliver the cert-manager Certificate CRs (gated, '
      + 'kubeconform-skipped) OR a clear values+doc scaffold and document that a service mesh / '
      + 'cert-manager provides the actual mTLS in prod; the gateway ingress TLS (wave 0) is the public '
      + 'edge. Be honest about what is wired vs documented. '
      + '(3) ADR-016 (Status/Context/Decision/Consequences): the M4 network trust model — default-deny '
      + 'NetworkPolicies make the internal network least-privilege (replacing ADR-009 flat-trust); '
      + 'mTLS via mesh/cert-manager as the in-cluster transport security; consequences + what is '
      + 'enforced now vs prod-config. Validate helm lint + template + kubeconform (report). Note needed '
      + 'values.yaml keys in issues.',
  },
]

const results = await parallel(
  tasks.map((t) => () => agent(CONTRACT + '\n\n' + t.body, { label: t.label, phase: 'Build', schema: RESULT }))
)
const out = {}
tasks.forEach((t, i) => { out[t.label] = results[i] || { summary: 'AGENT FAILED', files: [], decisions: [], issues: ['no return'] } })
return out
