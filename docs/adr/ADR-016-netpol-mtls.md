# ADR-016: M4 network trust model — default-deny NetworkPolicies + internal mTLS

## Status

Accepted (M4, wave 1). Refines ADR-009 §4 and ADR-014 (the consequence "the internal trust
boundary becomes enforced, not topological").

## Context

ADR-009 made tenant isolation structural on the wire: internal services trust the
`x-asker-tenant` gRPC metadata (and the Kafka `tenant_id` header) *because only trusted services
can reach them*. ADR-009 §4 was explicit that this trust is **topological** in M0–M3 — "everything
on one compose network", host ports bound to `127.0.0.1` — and named its own honest limitation:

> until M4, any process that reaches the internal network can assert any tenant — the metadata is
> trusted, not proven. This is acceptable only while the network is closed … it is exactly what
> NetworkPolicy + mTLS … harden in M4.

M4 (ADR-014) moves the system to Kubernetes, where a flat pod network is the default: absent
policy, every pod can dial every other pod and every dependency. That is *weaker* than the closed
compose network, not stronger — so the ADR-009 assumption would silently degrade unless M4
re-establishes the boundary as an **enforced** one. The spec's M4 line item is "network policies
(default-deny) … TLS"; the exit criterion (chaos: kill any one pod, no failed queries beyond
retry) also benefits from a policy graph that documents exactly which dependencies each service
needs.

Two distinct controls are in scope, and they are often conflated:

1. **Authorization at L3/L4 — who may open a connection to whom.** Kubernetes `NetworkPolicy`
   (default-deny + explicit allow) answers this with no application changes.
2. **Transport security + peer identity — encryption and cryptographic authentication of the
   peer (mTLS).** This requires a CA, per-service certificates, and TLS termination *in the
   services* or in sidecars. The M0–M3 binaries speak plaintext gRPC/HTTP internally; retrofitting
   in-process mTLS into nine services (two of them Python) is a large, separate change, and the
   idiomatic K8s answer is a service mesh.

The question for this ADR: what does M4 wave 1 *enforce now* vs. *wire as prod-config*, and how
do we keep the chart cloud-agnostic (ADR-014) while doing it.

## Decision

### 1. Default-deny NetworkPolicies are the enforced boundary (replaces ADR-009 §4 topology)

The umbrella chart ships, gated behind `networkPolicy.enabled` (**default `true`**):

- **`default-deny-ingress`** — an empty-podSelector `NetworkPolicy` with `policyTypes: [Ingress]`
  and no rules, so every pod in the release namespace rejects all ingress unless an allow policy
  re-opens a path. This is the foundation; everything else is additive.
- **Per-target ingress allow policies**, one per pod that legitimately accepts traffic, each
  naming exactly the permitted peers and ports. The call graph is mirrored from
  `deploy/compose/docker-compose.yml` (the source of truth, ADR-014 §2). Expressed as
  *callee accepts from caller* (the canonical least-privilege NetworkPolicy shape):

  | Target (accepts)   | Allowed source(s)                                  | Port(s)        |
  | ------------------ | -------------------------------------------------- | -------------- |
  | gateway            | ingress controller                                 | 8080           |
  | web                | ingress controller                                 | 80             |
  | query              | gateway                                            | 9200, 9201     |
  | control-plane      | gateway, connector-hub                             | 9100, 9101     |
  | connector-hub      | gateway, enrich (internal media)                   | 9300           |
  | clip               | query, enrich                                      | 9800           |
  | redis              | gateway, query, ingest                             | 6379           |
  | vespa              | query, index-writer                                | 8080           |
  | tei                | query, enrich                                      | 80             |
  | postgres           | control-plane                                      | 5432           |
  | minio              | connector-hub                                      | 9000           |
  | vault              | connector-hub, control-plane                       | 8200           |
  | kafka (Strimzi)    | ingest, enrich, index-writer, connector-hub        | 9092           |

  `ingest`, `enrich`, and `index-writer` accept **no** in-cluster app traffic (they are Kafka
  consumers that initiate connections and serve only a node-local health port that the kubelet
  probes — kubelet probe traffic is node-local and not subject to NetworkPolicy), so they get no
  ingress allow policy and stay default-denied.

- **Peer selectors.** App peers are matched by the wave-0 `app.kubernetes.io/component` label via
  `asker.serviceSelectorLabels`, so a policy's selectors match exactly the pods the wave-0
  Deployment/Service select. Dependency pods (postgres/redis/minio/vespa/tei/kafka/vault) are
  matched by a **configurable** selector (`networkPolicy.deps.<dep>.podSelector`, default
  `part-of: asker` + `component: <dep>`) because the sibling stateful-layer builder owns those
  StatefulSets' exact labels; this keeps the two waves decoupled. The public **ingress controller**
  peer is environment-specific (`networkPolicy.ingressController.{namespaceSelector,podSelector}`,
  default: the `ingress-nginx` namespace).

- **Egress is NOT default-denied by default.** A symmetric egress deny would also block DNS and
  every dependency dial, and the dependency pods' labels are owned by another wave. The chart ships
  a complete, opt-in egress posture (`networkPolicy.restrictEgress`, **default `false`**):
  default-deny-egress + DNS-for-all + per-service egress mirroring the call graph +
  connector-hub's external egress. The connector-hub external egress is the **SSRF surface
  (ADR-012)**: connectors fetch arbitrary tenant-configured URLs, so connector-hub is the one
  service granted broad external egress, restricted at the network layer to non-cluster
  destinations via `ipBlock.except` (cluster/link-local/metadata ranges); application-layer SSRF
  defenses remain connector-hub's responsibility.

### 2. Internal mTLS is mesh-/cert-manager-provided; the chart wires certs, not handshakes

mTLS is **documented as prod transport security and scaffolded**, not performed by this chart:

- The chart ships per-service **cert-manager `Certificate` CRs** (`cert-manager.io/v1`), gated
  behind `tls.internal.enabled` (**default `false`**), one per enabled service, issued by
  `tls.issuer` (a cert-manager `Issuer`/`ClusterIssuer`), producing a keypair Secret named by the
  convention **`<release>-<svc>-tls`** with SANs covering the service's in-cluster DNS (the bare
  `asker.serviceName`, plus the `.<ns>`, `.<ns>.svc`, `.<ns>.svc.<clusterDomain>` forms). Usages
  are `server auth` + `client auth` (mutual).
- **What actually performs mTLS in prod is a service mesh (Istio/Linkerd) or cert-manager + a
  TLS-aware sidecar/server.** A mesh does transparent mTLS with no chart-rendered certs at all
  (the operator enables strict-mTLS mesh-wide and enrols the namespace); cert-manager + these
  Certificate CRs is the meshless path, where each service must be configured to load its
  `*-tls` Secret. This chart does **not** terminate TLS inside the app containers — the M0–M3
  Go/Python binaries speak plaintext gRPC/HTTP on the internal network, and changing that is a
  services-layer change outside this chart's owned paths.
- The **public edge** TLS is unchanged and lives in wave 0: the gateway/web `Ingress`
  `spec.tls` (`ingress.tls.enabled`), terminated at the ingress controller with a cert-manager- or
  manually-provisioned Secret. ADR-016 governs only *internal* transport.

### 3. Cloud-agnostic and decoupled

No provider-specific resources or annotations. NetworkPolicy is core `networking.k8s.io/v1`;
the Certificate CRs are cert-manager CRDs (skipped by `kubeconform -ignore-missing-schemas`, like
the Strimzi/Vespa-operator kinds). Dependency selectors, the ingress-controller selector, the
kube-dns selector, the issuer, and the egress posture are all values, so adapting to a different
CNI label scheme, mesh, or managed dependency is a values change, not a template change. Every
policy is gated so an external/managed dependency is handled by flipping its selector or disabling
the in-namespace dep policies.

## Consequences

- **The ADR-009 trust assumption is now enforced, not merely topological.** With
  `networkPolicy.enabled=true` a pod that is not named as an allowed peer cannot even open a
  connection to a service, so `x-asker-tenant` is trusted *behind a default-deny policy* rather
  than trusted *because the network happens to be closed*. The cross-tenant leakage suite's wire
  assumption (only trusted services reach internal endpoints) is restored on Kubernetes.
- **What is enforced now (wave 1):** L3/L4 authorization — default-deny ingress + the
  least-privilege allow graph above, on by default; the dependency-side ingress policies that make
  each dep accept only its real callers.
- **What is prod-config (opt-in / mesh-provided):**
  - the egress least-privilege posture (`networkPolicy.restrictEgress`, default off until the
    stateful builder's dep labels are pinned and validated end-to-end);
  - internal **mTLS** — transport encryption + peer identity — provided by a service mesh or by
    cert-manager + TLS-aware services; the chart only emits the Certificate CRs
    (`tls.internal.enabled`, default off) and documents the wiring. The app binaries do not yet
    terminate internal TLS.
- **Honest gap:** until a mesh (or in-service TLS) is deployed, internal traffic is
  authorized-but-plaintext: NetworkPolicy stops *unauthorized* pods from connecting, but a peer
  that IS allowed is not cryptographically authenticated and the bytes are not encrypted on the
  wire. For the V1 per-user tenancy model on a single-tenant-per-cluster or trusted-CNI
  deployment this is the accepted wave-1 posture; multi-tenant-cluster or zero-trust deployments
  must enable the mesh before go-live. This limitation travels with the mechanism (the
  `templates/tls/_tls.tpl` header states it) so it cannot be forgotten.
- **NetworkPolicy enforcement depends on the CNI.** Policies are no-ops on a CNI that does not
  enforce them (some default kind setups); the M4 CI exit run (kind + chaos, wave 2) must use a
  policy-enforcing CNI (e.g. Calico/Cilium) for these to be meaningful. This is a cluster
  prerequisite, recorded here.
- **Drift risk with the call graph.** The allow matrix mirrors compose; a new inter-service call
  added to a service must be added here too, or it will be silently blocked once policies are on.
  CI rendering the chart catches syntax drift, not semantic drift — the matrix above is the
  reviewable record, and a blocked call surfaces fast in the chaos/e2e run.
- **New values keys (interface additions for the integrator to merge into `values.yaml`):**
  `networkPolicy.{enabled,restrictEgress}`, `networkPolicy.deps.<dep>.podSelector`,
  `networkPolicy.ingressController.{namespaceSelector,podSelector}`,
  `networkPolicy.dns.{namespaceSelector,podSelector}`,
  `networkPolicy.egress.{connectorHubExternal,externalExcept}`, and
  `tls.internal.{enabled,duration,renewBefore,keyAlgorithm,keySize}`, `tls.{issuer,issuerKind}`.
  All have safe in-template defaults so the chart renders and validates without them.
