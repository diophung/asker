# Runbook: Blue/green (and canary) deploys of the stateless tier

> Scope: zero-/low-downtime rollout of a new version of Asker's **9 stateless application services**
> (gateway, query, ingest, enrich, index-writer, connector-hub, control-plane, clip, web) deployed
> by the umbrella chart (`deploy/helm/asker`). The **stateful layer is explicitly NOT
> blue/green** — see [The stateful layer is shared](#the-stateful-layer-is-shared-and-migrated-forward).
> Reference: ADR-014 (stateless tier, image=values, Service-name contract), ADR-016 (NetworkPolicy
> by `app.kubernetes.io/component` label).

## The key fact that makes this safe

Every app service is **stateless and horizontally scalable** (ADR-014 §1/§6); all durable state
lives in the shared stateful layer (Vespa, Kafka, Postgres, MinIO, Redis). So two versions of a
stateless service can run **side by side against the same backing stores** with no per-version state.
The contract `Document` protobuf in `platform/proto` is the wire/format contract every stage depends
on, so as long as both versions speak a compatible contract (see
[Schema/contract compatibility](#schemacontract-compatibility-the-real-constraint)), blue and green
are interchangeable from the data layer's point of view.

There are three viable rollout mechanisms; pick by how much isolation you want:

| Mechanism | Isolation | Cost | When |
| --- | --- | --- | --- |
| **A. Rolling update** (default Deployment strategy) | none (in-place) | cheapest | routine patch releases; the chart's default |
| **B. Canary** (a second small Deployment behind the same Service) | partial (shares the Service) | low | gradual traffic shift, fast blast-radius limit |
| **C. Blue/green** (two full Helm releases, swing the edge) | full (separate releases) | 2x stateless footprint | risky/major releases needing instant rollback |

> **Image = value, chart version = default tag (ADR-014 §8).** A release is selecting a new image:
> bump `services.<svc>.image.tag` (or the chart `version`, which is the default tag) to the new
> immutable tag CI built+pushed. Never reuse a tag.

---

## A. Rolling update (the default)

The chart renders plain `Deployment`s; Kubernetes' default `RollingUpdate` strategy plus the per-
service **readiness probes** (`/readyz`/`/healthz`, `values.yaml` `services.<svc>.health`) and
**PDBs** (`pdb.minAvailable`) already give a safe in-place rollout: new pods must pass readiness
before old pods are torn down, and the PDB keeps `minAvailable` serving during the roll.

```sh
helm upgrade asker deploy/helm/asker -n asker -f prod-values.yaml \
  --set services.gateway.image.tag=1.5.0 --set services.query.image.tag=1.5.0 # ... per service, or bump chart version
kubectl -n asker rollout status deploy asker-query
kubectl -n asker rollout status deploy asker-gateway
# Roll back a bad rollout (per Deployment) or the whole release:
kubectl -n asker rollout undo deploy asker-query
helm rollback asker -n asker            # previous release revision
```

This is the recommendation for routine releases. Use B or C when you want traffic control or a fully
isolated standby.

---

## C. Blue/green — two full Helm releases, swing the edge

Run two complete stateless releases that share one stateful layer. **Disable the in-chart stateful
deps in BOTH releases** and point both at the already-running shared layer, so neither release owns
Vespa/Postgres/etc.

### 1. Blue is live (current)

```sh
# Blue: the running release. Its app Services are named bare (query, gateway, ...) per ADR-014 §3.
helm ls -n asker     # asker-blue  deployed  1.4.0
```

### 2. Stand up green alongside blue

Green is a *second release* in the same namespace. Because app `Service` names are **bare and
unprefixed** (ADR-014 §3 — `asker.serviceName` returns `query`, not `<release>-query`), two releases
in one namespace would collide on Service names. Two clean ways to avoid that:

- **(C1) Separate namespace** (recommended for true isolation): install green in `asker-green`,
  pointing its `config.*` at the shared stateful layer via fully-qualified cross-namespace DNS
  (e.g. `config.postgresHost=postgres.asker-data.svc.cluster.local`, `config.vespaUrl` likewise).
  The edge cutover is then an Ingress backend swing (below).
- **(C2) Distinct release with `fullnameOverride`** so all *prefixed* objects (Deployment/HPA/PDB)
  differ; you must still give green its own Service names (override the service keys) — heavier, only
  if a single namespace is mandatory.

```sh
# C1: green in its own namespace, deps disabled, pointed at the shared stateful layer.
helm install asker-green deploy/helm/asker -n asker-green --create-namespace \
  -f prod-values.yaml \
  --set services.gateway.image.tag=1.5.0 --set services.query.image.tag=1.5.0 \   # ... new tags per service
  --set 'stateful.postgres.deploy=false' --set 'stateful.redis.deploy=false' \
  --set 'stateful.minio.deploy=false'    --set 'stateful.keycloak.deploy=false' \
  --set 'stateful.tei.deploy=false'      --set 'vespa.deploy=false' \
  --set 'kafka.strimzi.enabled=false'    --set 'vault.deploy=false' \
  --set config.postgresHost=postgres.asker-data.svc.cluster.local \
  --set config.vespaUrl=http://vespa.asker-data.svc.cluster.local:8080 \
  --set config.redisAddr=redis.asker-data.svc.cluster.local:6379 \
  --set config.kafkaBrokers=asker-kafka-kafka-bootstrap.asker-data.svc.cluster.local:9092 \
  --set ingress.enabled=false            # green has no public Ingress yet
kubectl -n asker-green rollout status deploy asker-green-query
```

### 3. Smoke green privately (before any user traffic)

Port-forward green's gateway and run the in-cluster smoke against it (the same flow as
`deploy/k8s-docs.md` "Verify" / `tools/e2e/smoke.sh`): mint a Keycloak token, `GET /v1/me`, feed a
probe doc to a probe tenant's Vespa group, query it back, and confirm isolation. Green must pass
before cutover.

```sh
kubectl -n asker-green port-forward deploy/asker-green-gateway 18080:8080 &
GATEWAY_URL=http://localhost:18080 bash tools/e2e/smoke.sh   # or the k8s smoke equivalent
```

### 4. Cut over the gateway/web Ingress to green

The public edge is the gateway/web `Ingress` (ADR-014 §5; `ingress.gatewayHost`/`webHost`). Cut over
by repointing the **same hostnames** from blue's Services to green's:

- **(C1, cross-namespace):** enable green's Ingress with the production hosts and **delete/disable
  blue's** Ingress in the same step so the hostname binds to green (`--set ingress.enabled=true` on a
  green `helm upgrade`, then `--set ingress.enabled=false` on blue). With nginx-ingress, the host's
  backend now resolves to green's gateway/web Services. DNS/hostnames do not change — only the
  Ingress backend does, so client URLs and the **gateway OIDC issuer origin are unchanged** (the
  issuer must stay constant across the swing — `gateway.config.oidcIssuer`).
- Alternatively, if you front both releases with one Service whose selector you control, swing the
  Service `selector` from blue's pod labels to green's (`app.kubernetes.io/instance: asker-green`) —
  an atomic label swap.

```sh
helm upgrade asker-green deploy/helm/asker -n asker-green --reuse-values \
  --set ingress.enabled=true --set ingress.gatewayHost=gateway.asker.example \
  --set ingress.webHost=app.asker.example
helm upgrade asker-blue  deploy/helm/asker -n asker --reuse-values --set ingress.enabled=false
```

### 5. Watch, then retire blue

Watch green's golden signals (query P90 ≤ 5s per the spec SLO, gateway 5xx, query error rate) for a
soak window. Keep blue **running but dark** (no Ingress) so rollback is instant.

```sh
# Rollback (instant): swing the Ingress back to blue.
helm upgrade asker-blue  deploy/helm/asker -n asker --reuse-values --set ingress.enabled=true
helm upgrade asker-green deploy/helm/asker -n asker-green --reuse-values --set ingress.enabled=false
# Or, once green is proven, retire blue:
helm uninstall asker-blue -n asker
```

---

## B. Canary (lighter than blue/green)

Run a small second Deployment of the new version **behind the same Service** so a fraction of traffic
hits it. With plain K8s Services (no mesh), traffic split ≈ replica ratio: e.g. blue=9 replicas,
canary=1 ⇒ ~10% to canary. The canary's pods carry the same `app.kubernetes.io/component` label so
the Service (and the ADR-016 NetworkPolicies, which select by component) include them automatically.
Scale the canary up / blue down to shift traffic; delete the canary to abort. A mesh (Istio/Linkerd,
the same one ADR-016 leaves for prod mTLS) gives precise weight-based splitting if you need it.

---

## The stateful layer is shared, and migrated forward

**Vespa, Kafka, Postgres, MinIO, Redis are NOT blue/green.** They are the single shared system of
record; there is exactly one of each and **both** blue and green talk to it (ADR-014 §4: stateful
deps are a separate lifecycle from the app chart). Consequences a deploy MUST respect:

- **Schema/data changes migrate forward, in their own step, decoupled from the app rollout.** You do
  not get a fresh stateful copy per color. So:
  - **Postgres (control-plane):** run DB migrations **before** the new app version that needs them,
    and make them **backward-compatible** (expand-then-contract): the old (blue) app must keep
    working against the migrated schema during the overlap, and the new (green) app against the
    pre-migration schema until cutover. Never ship a destructive migration in the same step as the
    cutover.
  - **Vespa (index):** the application package (`services.xml` + `doc.sd`) is shared, deployed once.
    A schema/field addition is applied to the shared cluster ahead of the app that uses it; both
    colors query the same groups. **`EMBEDDING_DIM`/`CLIP_DIM` are immutable for a corpus** (ADR-005)
    — changing them is a re-index, **not** a blue/green flip (see `backup-restore-vespa.md`).
  - **Kafka (Strimzi):** topics + the `Document` proto contract are shared. The pipeline is
    event-sourced (ADR-004): blue and green producers/consumers must agree on a compatible message
    schema. Add proto fields backward-compatibly; never renumber/remove during overlap.
- **The KEK (Vault/file) is shared and version-independent** (ADR-015) — the same `asker-kek` serves
  both colors; envelope crypto is orthogonal to the app rollout.
- **Redis is a cache** — both colors share it; on a contract change to cached values, namespace the
  cache key by version or accept cold misses. It fails open (degradation ladder), so a flush is safe.

### Schema/contract compatibility (the real constraint)

Blue/green is only safe when blue and green are **mutually compatible** against the shared stores for
the entire overlap window. The discipline:

1. **Expand** (migration / additive proto+schema change) — deploy first, alone; both versions work.
2. **Roll** the app (rolling / canary / blue-green) — both versions run.
3. **Contract** (remove the old column/field) — only after the old version is fully retired.

This expand→migrate→roll→contract order — not the rollout mechanism — is what actually prevents a bad
deploy. The rollout mechanism only controls how fast you can flip and roll back; the shared stateful
layer is why you cannot skip the compatibility discipline.

---

## Pre-flight & rollback checklist

- [ ] New images built+pushed by CI with **immutable tags** (ADR-014 §8); chart helm-lints +
      kubeconform-validates (`make helm-lint`, see `deploy/helm/asker/README.md`).
- [ ] DB migrations are backward-compatible and applied **before** the dependent app version.
- [ ] Vespa `EMBEDDING_DIM`/`CLIP_DIM` unchanged (else it's a re-index, not a deploy).
- [ ] Green smoked privately (token → `/v1/me` → feed+query a probe tenant → isolation holds) before
      any user traffic.
- [ ] Gateway OIDC issuer origin unchanged across the swing.
- [ ] Blue kept dark (running, no Ingress) for the soak window → **rollback = swing Ingress back**.
- [ ] Golden signals watched: query P90 ≤ 5s, gateway/query 5xx, error rate — for the full soak.
