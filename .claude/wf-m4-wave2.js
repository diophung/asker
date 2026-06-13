export const meta = {
  name: 'asker-m4-wave2',
  description: 'M4 wave 2: CI kind deploy + chaos test (exit criterion), slim CI values, backup/restore runbooks, deploy docs',
  phases: [{ title: 'Build', detail: '2 parallel builders: ci-kind-chaos, runbooks-docs' }],
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
  '# Asker M4 Wave 2 — Shared Build Contract (BINDING)',
  '',
  'Milestone M4 (production deployment), final wave. Repo /Users/dio/works/asker. M4 waves 0-1 are',
  'COMMITTED: deploy/helm/asker is a cloud-agnostic Helm umbrella chart that helm-lints clean and',
  'renders kubeconform-valid (default 68 / dev 51 resources): the 9 stateless app services',
  '(Deployments/Services/HPA/PDB/Ingress), self-hosted stateful deps (postgres/redis/minio/keycloak/',
  'tei StatefulSets+Deployments, gated <dep>.deploy), Strimzi Kafka CRs (gated kafka.strimzi.enabled),',
  'a multi-group Vespa StatefulSet (gated vespa.deploy), a dev Vault (gated vault.deploy) + the Go',
  'crypto.NewVaultKEK, default-deny NetworkPolicies, TLS scaffolding. values.yaml is the authoritative',
  'interface; values-dev.yaml shrinks for a single node. Read FIRST:',
  '- deploy/helm/asker/values.yaml + values-dev.yaml + README.md (every toggle).',
  '- the M4 milestone + EXIT CRITERION in specs/asker-v1-personal-search-engine.md: "full stack',
  '  deploys to a kind/k3d cluster in CI; chaos test (kill any one pod) shows no failed queries',
  '  beyond retry."',
  '- .github/workflows/ci.yml (existing jobs: lint, test, build+trivy, enrich, web, e2e-smoke,',
  '  e2e-m1, e2e-m3-media — mirror their style, action versions, on: triggers).',
  '- tools/e2e/smoke.sh + m1-e2e.sh (bash house style: set -euo pipefail, numbered PASS/FAIL checks,',
  '  python3 for JSON, env-overridable, non-zero exit on failure).',
  '- vespa/README.md (the document/v1 feed + the streaming.groupname query the chaos smoke uses to',
  '  feed+query a tenant-scoped doc directly, like tools/e2e/smoke.sh does).',
  '- the gateway OIDC token flow (Keycloak password grant) used by the e2e suites.',
  '',
  '## Hard rules',
  '- Write ONLY within your owned paths. NEVER modify go.mod/Makefile-EXCEPT the integrator adds',
  '  targets (you may PROPOSE Makefile lines in issues), platform/**, services/**, connectors/**,',
  '  web/**, vespa/**, deploy/compose/**, or the committed deploy/helm/asker chart TEMPLATES/values',
  '  (you MAY add a NEW values-ci.yaml under deploy/helm/asker/ and NEW files under deploy/helm/ and',
  '  docs/). Needs outside scope -> issues.',
  '- helm + kubeconform are on PATH; NO kind/kubectl/cluster locally (the kind deploy + chaos run in',
  '  CI only — you author + statically validate it). Validate what you can: helm template the CI',
  '  values profile + kubeconform; bash -n + careful review of scripts; YAML validity of the workflow',
  '  (ruby -ryaml or python3). Report exact output.',
  '- Cloud-agnostic; the CI cluster is kind (or k3d). Keep the CI job within a GitHub runner budget',
  '  (~7GB/2cpu): a SLIM profile — do NOT bring up clip(torch)/enrich(whisper)/multi-node Vespa/the',
  '  full Strimzi cluster. The chaos test proves the QUERY PATH survives a pod kill.',
  '',
  'Your structured result lists EVERY file written + the validation output observed.',
].join('\n')

phase('Build')

const tasks = [
  {
    label: 'ci-kind-chaos',
    body: 'Your task: the kind deploy + chaos CI job (the M4 exit criterion), a slim CI values profile, '
      + 'and the chaos/smoke script. Owned paths: deploy/helm/asker/values-ci.yaml, tools/e2e/k8s-chaos.sh, '
      + '.github/workflows/k8s.yml (a NEW workflow file — do not edit ci.yml; the integrator may fold it '
      + 'in). '
      + '(1) deploy/helm/asker/values-ci.yaml — a SLIM profile for a single-node kind cluster on a 7GB '
      + 'runner that brings up ONLY the query path needed for the chaos test: gateway (2 replicas + PDB '
      + 'minAvailable 1), query (2 replicas + PDB 1), Vespa (vespa.deploy=true, groups=1, nodesPerGroup=1, '
      + 'tiny heap/storage), TEI (stateful.tei small model bge-small + EMBEDDING_DIM 384), Redis, Keycloak '
      + '(for the token), control-plane (1) + Postgres (1) if the query path needs it (query itself does '
      + 'not — keep minimal). DISABLE the heavy/irrelevant ones for the chaos test: clip (vault/clip off, '
      + 'query degrades its CLIP arm — fine), enrich, ingest, index-writer, connector-hub, web, '
      + 'kafka.strimzi (the chaos smoke feeds Vespa directly via the document API, no Kafka pipeline '
      + 'needed), networkPolicy.enabled=false (kind default CNI does not enforce NetworkPolicy — note '
      + 'it), HPA off (single node), images: imagePullPolicy Never (kind loads locally-built images). '
      + 'Tune resources tiny. Validate it renders + kubeconform-valid; report the resource count. '
      + '(2) tools/e2e/k8s-chaos.sh — runs against the deployed kind cluster (kubectl + the gateway via '
      + 'port-forward or a NodePort; env-overridable). Steps: wait for gateway+query+vespa rollouts '
      + 'ready; deploy the Vespa app package (the chart notes a Job, or the script execs vespa/deploy.sh-'
      + 'equivalent against the in-cluster config server via kubectl exec/port-forward — feed a '
      + 'tenant-scoped doc with a rare token via the Vespa document API, like tools/e2e/smoke.sh); get a '
      + 'Keycloak token; query the rare token through the gateway -> assert 1 hit (baseline). THEN the '
      + 'CHAOS: in a background loop, fire N gateway /v1/search requests continuously WITH a bounded '
      + 'client retry (e.g. 3 retries on 5xx/connection error) while `kubectl delete pod` kills one query '
      + 'pod (and separately one gateway pod); assert EVERY query ultimately succeeds within its retries '
      + '(no failed queries beyond retry) and the killed pods are rescheduled Ready. Summary table; '
      + 'non-zero exit on any failed-beyond-retry query. bash -n clean; document the kubectl/kind deps. '
      + '(3) .github/workflows/k8s.yml — job e2e-k8s on ubuntu-latest (needs nothing or [build]): '
      + 'free disk; install kind + kubectl + helm; `kind create cluster`; build the gateway+query+'
      + 'control-plane images (reuse their Dockerfiles) + `kind load docker-image`; (TEI/Vespa/Redis/'
      + 'Keycloak images are pulled by kind from registries — they are upstream images, fine); '
      + '`helm install asker deploy/helm/asker -f values-ci.yaml --wait --timeout 15m`; run '
      + '`bash tools/e2e/k8s-chaos.sh`; always() dump `kubectl get pods -A` + describe + logs on '
      + 'failure; teardown. timeout-minutes ~40. Validate the YAML (ruby -ryaml / python3) + bash -n. '
      + 'Be honest in issues that this runs in CI only (no local kind); note the Makefile target to '
      + 'propose (e.g. make e2e-k8s shelling to a local kind if present).',
  },
  {
    label: 'runbooks-docs',
    body: 'Your task: the M4 operational docs — backup/restore runbooks, blue/green deploy doc, the K8s '
      + 'deploy guide, and a CI helm-lint step proposal. Owned paths: docs/runbooks/** (backup-restore-'
      + 'postgres.md, backup-restore-vespa.md, backup-restore-minio.md, blue-green-deploy.md), '
      + 'deploy/k8s-docs.md, and you may refine deploy/helm/asker/README.md ONLY by appending a '
      + '"Deploying to Kubernetes" section (do not alter the values docs the wave-0 builder wrote — '
      + 'append). '
      + '(1) Backup/restore runbooks (concrete, copy-pasteable kubectl/CLI commands, RPO/RTO notes, '
      + 'verification steps): Postgres (pg_dump/pg_restore or a CronJob + PVC snapshot; the control-plane '
      + 'tenants/connector_instances/tokens/tenant_deks tables — note the tenant_deks rows are the '
      + 'envelope-encryption DEKs, useless without the Vault/file KEK, so KEK backup is called out); '
      + 'Vespa (content cluster: vespa-backup / document export, or PVC volume snapshots of /opt/vespa/'
      + 'var per content node + redeploy the app package; note streaming-mode per-tenant groups); MinIO '
      + '(mc mirror to a second bucket/site, or PVC snapshot; note blobs are per-tenant envelope-'
      + 'encrypted so the KEK must be backed up too — cross-reference). Each: backup procedure, restore '
      + 'procedure, how to VERIFY the restore, and the cross-cutting "back up the KEK (Vault/file) or '
      + 'all encrypted blobs+tokens are unrecoverable" warning. '
      + '(2) blue-green-deploy.md: blue/green (or canary) rollout of the stateless services via two Helm '
      + 'releases / label-swung Services or the Deployment rollout strategy; how to cut over the gateway '
      + 'Ingress; rollback; how the stateful layer (Kafka/Vespa/Postgres) is NOT blue/green (shared, '
      + 'migrated forward) and what that means for a deploy. '
      + '(3) deploy/k8s-docs.md: the end-to-end K8s deploy guide — prerequisites (a cluster, a '
      + 'policy-enforcing CNI for NetworkPolicies per ADR-016, the Strimzi operator, optionally '
      + 'cert-manager + an external Vault), `helm install` with a prod values file, the post-install '
      + 'steps (Strimzi waits, the Vespa app-package deploy Job, pointing config.kafkaBrokers at the '
      + 'Strimzi bootstrap, setting VAULT_ADDR so control-plane/connector-hub use Vault), how to verify '
      + '(make e2e-smoke-equivalent against the cluster), and the dev-vs-prod gap (dev compose stays the '
      + 'inner loop). Reference ADR-014/015/016. '
      + '(4) In issues, propose the .github/workflows/ci.yml helm-lint step (helm lint deploy/helm/asker '
      + '+ helm template | kubeconform) for the integrator to add to the lint job, and the make targets '
      + '(helm-lint, e2e-k8s). Validate any rendered examples with helm template where applicable; keep '
      + 'docs accurate to the actual chart values (read values.yaml).',
  },
]

const results = await parallel(
  tasks.map((t) => () => agent(CONTRACT + '\n\n' + t.body, { label: t.label, phase: 'Build', schema: RESULT }))
)
const out = {}
tasks.forEach((t, i) => { out[t.label] = results[i] || { summary: 'AGENT FAILED', files: [], decisions: [], issues: ['no return'] } })
return out
