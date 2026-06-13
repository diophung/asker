# ADR-014: Production deployment — cloud-agnostic Helm umbrella chart on Kubernetes

## Status

Accepted (M4).

## Context

M0–M3 run the whole system as a Docker Compose stack (`deploy/compose/`): 9 stateless app
services (gateway, query, ingest, enrich, index-writer, connector-hub, control-plane, clip, web)
plus stateful dependencies (Keycloak, Vespa, TEI, MinIO, Postgres, Redis, Redpanda). The spec's
M4 milestone is production deployment: "Helm charts for everything (cloud-agnostic: no
provider-specific resources; storage via StorageClass, ingress via ingress-class), Strimzi
Kafka, Vespa on K8s with multi-group content clusters, HPA on stateless services,
PodDisruptionBudgets, network policies (default-deny), Vault integration, TLS … *Exit: full
stack deploys to a kind/k3d cluster in CI; chaos test (kill any one pod) shows no failed queries
beyond retry.*"

The architecture is already a clean fit for Kubernetes: everything is stateless except
Vespa/Kafka/Postgres/MinIO, which scale horizontally (spec §2.7), and the internal trust
boundary was always intended to harden into NetworkPolicy + mTLS in M4 (ADR-009). Several
dev/prod deviations were deferred to M4 by ADR-003: Redpanda → Apache Kafka (Strimzi), and the
file-based KEK shim → HashiCorp Vault. The open questions for this ADR are: how to package the
deployment (one chart or many; subcharts vs. operators), how to stay cloud-agnostic (the spec
forbids provider-specific resources), how credentials reach pods in prod vs. dev, and how the
new K8s surface coexists with the compose stack without the two drifting apart.

This ADR is the foundation laid in M4 "wave 0"; it is implemented incrementally across waves
(stateless workloads + edge here; Strimzi/Vespa/Vault/NetworkPolicy/CI in later M4 waves).

## Decision

1. **One cloud-agnostic Helm umbrella chart for the stateless app tier** at
   `deploy/helm/asker` (Helm 3/4, `apiVersion: v2`). It renders the 9 stateless services as
   Kubernetes `Deployment`s, each with a `Service`, an `HorizontalPodAutoscaler`, and a
   `PodDisruptionBudget`. The chart is the single deployment artifact CI installs to a
   kind/k3d cluster for the M4 exit criteria.

2. **The compose stack is the source of truth for service shape, and stays the dev inner
   loop.** Every K8s object mirrors `deploy/compose/docker-compose.yml` — image build, env
   vars, ports, health endpoints, inter-service addresses. Crucially, **`make dev-up` (compose)
   remains the day-to-day developer/CI inner loop (ADR-003); Helm/Kubernetes is the production
   and integration surface.** We deliberately keep two deployment surfaces rather than force
   developers onto a local cluster: compose boots in seconds on a laptop, the cluster path is
   for prod-shape validation and chaos testing.

3. **Service DNS names equal the compose service names.** The Kubernetes `Service`
   `metadata.name` for each app service is the bare service key (`query`, `connector-hub`,
   `clip`, `control-plane`, …), NOT release-prefixed, so the in-cluster addresses other
   services dial (`dns:///query:9200`, `http://connector-hub:9300`, `http://clip:9800`,
   `dns:///control-plane:9100`) are identical to compose. Deployment/HPA/PDB objects ARE
   release-prefixed (`<release>-<svc>`). The `_helpers.tpl` helpers `asker.serviceName`
   (bare) and `asker.serviceFullname` (prefixed) encode this split.

4. **Stateful dependencies are operator-/StatefulSet-managed, NOT Helm subcharts of this
   chart.** Apache **Kafka via the Strimzi operator** (replacing Redpanda — the split ADR-003
   §1 always prescribed), **Vespa on K8s** with multi-group content clusters (spec §2.7),
   Postgres/MinIO/Redis as StatefulSets, Keycloak, TEI, and **HashiCorp Vault** for the KEK
   (replacing the file shim). These are stood up in later M4 waves. This chart references them
   only by their Service addresses, exposed as **`config.*` values** (e.g.
   `config.kafkaBrokers`, `config.vespaUrl`, `config.teiUrl`, `config.redisAddr`,
   `config.postgresHost`). Keeping them out of the umbrella's `dependencies` decouples the app
   rollout (and its chaos test) from operator/CRD lifecycle and lets the app tier be templated
   and validated in isolation.

5. **Cloud-agnostic by construction — provider lock-in is removed via indirection, not
   convention:**
   - **Storage**: persistence is via PVC with a configurable `persistence.storageClass`,
     **default `""`** → the cluster's default StorageClass. No provider class name
     (`gp2`/`standard`/`premium-lrs`) ever appears in chart defaults.
   - **Ingress**: a single `Ingress` with a configurable `ingress.className`, **default `""`**
     → the cluster's default IngressClass. `ingress.annotations` defaults to `{}` — **no
     nginx/traefik/provider-specific annotations in defaults**; class- or cert-manager-specific
     annotations are supplied per environment.
   - **Load balancing**: app `Service`s are `ClusterIP`; public exposure is the Ingress only.
     No cloud LoadBalancer annotations.
   - **TLS**: `ingress.tls.enabled` toggles `spec.tls`; the cert `Secret` is provisioned
     out-of-band (cert-manager/manual). The chart mints no certificates.

6. **Stateless services autoscale and tolerate disruption.** Each app service gets an HPA
   (`services.<svc>.autoscaling` → min/max replicas + CPU/memory targets) and a PDB
   (`services.<svc>.pdb.minAvailable`), satisfying the M4 "HPA on stateless services,
   PodDisruptionBudgets" requirement and underpinning the chaos exit criterion. Pods run with
   hardened defaults (`runAsNonRoot`, dropped capabilities, `readOnlyRootFilesystem` where the
   image allows). `values-dev.yaml` turns autoscaling/PDB off for single-node kind/k3d.

7. **Secrets: Vault/External Secrets in prod, a chart-managed Secret for dev — selected by one
   strategy value.** `secrets.strategy` is one of:
   - `chart` — the chart renders a `Secret` from `secrets.dev.*` (dev-only literals mirroring
     compose: MinIO, Postgres, Keycloak bootstrap). For kind/k3d/CI only.
   - `external` — an **External Secrets Operator** `ExternalSecret` syncs the Secret from a
     backend; the chart renders no credentials.
   - `vault` — **HashiCorp Vault** Agent/CSI injects credentials (wave 1). Recommended prod
     default. Replaces the M0–M3 file-based KEK shim with the real Vault interface the spec
     prescribed.
   All three resolve to a `Secret` of the **same fixed name** (`asker.secretName`,
   `<fullname>-secrets`), so workloads reference one name and never branch on strategy.
   Embedding dims and dependency addresses are **non-secret** config in a shared `ConfigMap`
   (`config.*`) that every workload `envFrom`s; only credentials live in the Secret.

8. **Images: repo+tag are values; CI builds and pushes.** Each service has
   `image.repository` + `image.tag` (default: the chart `version`, so chart and images move
   together) with an optional `global.imageRegistry` prefix (default `ghcr.io`). **CI builds
   and pushes the images** (from the same Dockerfiles compose uses) on release; the chart only
   references them.

9. **Embedding dimension stays deploy-time config (ADR-005), now a chart value.**
   `config.embeddingDim` (default 1024) and `config.clipDim` (default 512) flow into enrich,
   index-writer, and query as `EMBEDDING_DIM`/`CLIP_DIM` and must match the models and the
   deployed Vespa schema. `values-dev.yaml` sets `embeddingDim: 384` for the dev bge-small model.

10. **Validation without a cluster.** Charts are validated by `helm lint`, `helm template`, and
    piping rendered manifests through `kubeconform -strict -ignore-missing-schemas
    -kubernetes-version 1.29.0`. No `helm install`/`kubectl apply` to a live cluster in
    wave 0 — that runs in CI against kind/k3d in a later M4 wave (the exit criterion).

## Consequences

- **Two deployment surfaces must be kept in sync.** Compose (dev inner loop, ADR-003) and the
  Helm chart (prod) both describe the same 9 services. Compose remains the source of truth;
  any new env var, port, or dependency added to a service must land in both. This is the
  accepted cost of a fast laptop loop plus a prod-shape cluster path; CI rendering the chart on
  every change is the guardrail against drift.
- **`values.yaml` is the authoritative interface between the build waves.** The chart-skeleton
  wave owns it; the stateless-workloads and edge waves code their templates against its keys
  (`services.<svc>.*`, `config.*`, `secrets.*`, `ingress.*`, `persistence.*`). Every key is
  documented in `deploy/helm/asker/README.md`; changing a key shape is a cross-wave change.
- **The internal trust boundary becomes enforced, not topological (ADR-009).** Default-deny
  NetworkPolicies + mTLS (a later M4 wave) replace "everything on one compose network" — the
  `x-asker-tenant` metadata stops being merely trusted-by-isolation and becomes
  trusted-behind-a-policy. Until those land, the chart alone does not harden the boundary; that
  is explicitly a subsequent wave.
- **Operator/StatefulSet dependencies are a separate lifecycle.** Because Strimzi/Vespa/Vault
  are not subcharts, installing this chart does not install them; the deploy runbook orders
  operators → stateful deps → app chart. The benefit is that the app tier (and its chaos test)
  is decoupled from CRD upgrades.
- **Switching cloud provider is a values change, not a chart change.** StorageClass, IngressClass,
  TLS cert source, registry, and dependency addresses are all values; no template references a
  provider resource. This is what makes the kind/k3d CI run representative of any target cluster.
- **Dev/prod parity has known, documented gaps.** The web image bakes `VITE_*` URLs at build
  time, so a prod web release needs an image built with prod URLs (or a runtime-config shim) —
  recorded in `values.yaml`. The embedding-dim and Kafka-flavour deviations (ADR-003/005) are
  closed structurally here (config values + Strimzi).
- **Future ADRs refine the later waves.** Strimzi topic/cluster topology, Vespa multi-group
  content cluster sizing, the NetworkPolicy matrix, the Vault auth method, and blue/green +
  backup/restore runbooks are M4 work that this ADR sets the frame for but does not fully
  specify.
