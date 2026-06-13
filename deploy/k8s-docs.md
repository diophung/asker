# Deploying Asker to Kubernetes

End-to-end guide for installing Asker on a real Kubernetes cluster with the cloud-agnostic Helm
umbrella chart at `deploy/helm/asker`. This covers prerequisites, ordering (operators → stateful
deps → app chart), the post-install wiring, verification, and the dev-vs-prod gap.

> **Compose stays the inner loop.** `make dev-up` (Docker Compose, ADR-003) is the day-to-day
> developer/CI loop; Kubernetes is the **production and integration surface** (ADR-014 §2). This
> guide is for the cluster path. The chart mirrors the compose stack one-to-one (images, env, ports,
> health, in-cluster DNS), so a service behaves the same in both.

References: `docs/adr/ADR-014-kubernetes-helm.md` (chart design, cloud-agnostic indirection,
secrets strategy), `docs/adr/ADR-015-vault-kek.md` (Vault KEK), `docs/adr/ADR-016-netpol-mtls.md`
(NetworkPolicy + mTLS), and `deploy/helm/asker/README.md` (the authoritative `values.yaml`
reference — read it for every toggle).

---

## 1. Prerequisites

| Requirement | Why | Reference |
| --- | --- | --- |
| A Kubernetes cluster (1.29+) with a **default StorageClass** | PVCs use `persistence.storageClass: ""` ⇒ cluster default; the chart hardcodes no provider class | ADR-014 §5 |
| A **policy-enforcing CNI** (Calico / Cilium) | NetworkPolicies are no-ops on a CNI that does not enforce them (e.g. default kindnet) | ADR-016 |
| An **IngressClass** + ingress controller (e.g. ingress-nginx) | the public edge is the gateway/web Ingress; `ingress.className: ""` ⇒ cluster default | ADR-014 §5 |
| The **Strimzi** Kafka operator installed | the chart renders the Kafka/KafkaTopic CRs only; the operator reconciles them | ADR-003 §1, ADR-014 §4 |
| (Recommended) **cert-manager** | public-edge TLS Secret + the optional internal `Certificate` CRs (`tls.internal`) | ADR-016 |
| (Recommended, prod) an **external HA Vault** | the production envelope-encryption KEK; replaces the dev file-KEK | ADR-015 |
| `helm` 3/4 + `kubectl` | install + operate | — |
| Built+pushed images for the 9 services | `image = global.imageRegistry + repo + ":" + (tag or chart version)`; CI builds+pushes | ADR-014 §8 |

`tools` such as `helm`/`kubeconform` validate the chart with no cluster (see
`deploy/helm/asker/README.md` "Validate"). The CI exit run uses **kind** + a policy-enforcing CNI.

---

## 2. Install operators + the stateful layer first

The umbrella chart does **not** install operators, and the heavy stateful pieces default **off** so
the app tier can be templated/validated in isolation (ADR-014 §4). You opt them in. Two shapes:

### (a) Self-hosted stateful deps via the chart (the gated StatefulSets)

The chart can stand up Postgres, Redis, MinIO, Keycloak, TEI (StatefulSets/Deployments,
`stateful.<dep>.deploy`), a multi-group Vespa StatefulSet (`vespa.deploy`), the Strimzi Kafka CRs
(`kafka.strimzi.enabled`), and a dev Vault (`vault.deploy`). This is the self-contained path the
kind/k3d CI stack uses. **Production should prefer managed/operator-run stateful services** and flip
each `<dep>.deploy=false`, pointing the matching `config.*` address at the managed instance
(ADR-014 §4).

### (b) Strimzi Kafka operator (required when `kafka.strimzi.enabled=true`)

```sh
helm repo add strimzi https://strimzi.io/charts/
helm install strimzi-operator strimzi/strimzi-kafka-operator -n kafka --create-namespace
```

The chart renders the `Kafka` + `KafkaNodePool` + `KafkaTopic` CRs (KRaft / ZK-less). After install,
Strimzi creates the bootstrap Service **`<clusterName>-kafka-bootstrap:9092`** (default cluster name
`asker-kafka` ⇒ `asker-kafka-kafka-bootstrap:9092`). **Set `config.kafkaBrokers` to that address** so
every app worker dials the Strimzi cluster (see [§4](#4-post-install-wiring)).

### (c) Vault (prod KEK, ADR-015)

Run Vault HA out-of-band (HashiCorp Vault Helm chart / operator) with Raft, auto-unseal, audit
logging, and a **least-privilege token** scoped to `transit/encrypt/asker-kek` +
`transit/decrypt/asker-kek`. Provision the `transit` engine + the `asker-kek` **derived** key via
Vault bootstrap (Terraform / config-as-code) — **not** the chart's dev init Job. Then set
`VAULT_ADDR` for control-plane/connector-hub ([§4](#4-post-install-wiring)). Keep `vault.deploy=false`
(the shipped dev Vault is NON-PRODUCTION: in-memory, static root token).

---

## 3. `helm install` with a prod values file

Author a `prod-values.yaml` (do not edit the committed `values.yaml`). Minimum prod-shape:

```yaml
# prod-values.yaml — example (cloud-agnostic; fill in your hostnames/registry)
global:
  imageRegistry: ghcr.io                 # your registry; images tagged with the release/chart version

config:
  embeddingDim: 1024                      # bge-m3 (MUST match the TEI model + the deployed Vespa schema, ADR-005)
  clipDim: 512
  kafkaBrokers: asker-kafka-kafka-bootstrap:9092   # the Strimzi bootstrap (see §2b)
  # If stateful deps are managed/external, point these at them and set <dep>.deploy=false:
  # vespaUrl / teiUrl / redisAddr / postgresHost / minioEndpoint ...

secrets:
  strategy: vault                         # NEVER ship the dev "chart" literals to prod (ADR-014 §7)

ingress:
  enabled: true
  className: nginx                        # your IngressClass
  annotations:                            # per-env (cert-manager issuer, etc.) — none in chart defaults
    cert-manager.io/cluster-issuer: letsencrypt-prod
  gatewayHost: gateway.asker.example
  webHost: app.asker.example
  tls:
    enabled: true
    secretName: asker-edge-tls            # cert-manager / externally provisioned

vault:    { deploy: false, enabled: true, addr: "https://vault.internal:8200", keyName: asker-kek }
networkPolicy: { enabled: true, restrictEgress: true }   # restrictEgress once dep labels are pinned (ADR-016)
tls: { internal: { enabled: true, issuer: asker-internal-ca } }   # if going the cert-manager mTLS path
```

Install (namespaced, dependencies up first):

```sh
helm install asker deploy/helm/asker -n asker --create-namespace -f prod-values.yaml --wait
```

> **CI / kind (the M4 exit criterion).** For the slim CI cluster use the committed CI profile, which
> brings up only the synchronous query path (gateway + query + Vespa + TEI + Redis + Keycloak) sized
> for a single-node runner:
> ```sh
> helm install asker deploy/helm/asker -f deploy/helm/asker/values-ci.yaml --wait
> ```
> See `deploy/helm/asker/values-ci.yaml` (and `deploy/helm/asker/README.md` "Deploying to
> Kubernetes") for what it disables and why.

The post-install `NOTES.txt` prints the rendered edge URLs, dependency addresses, the embedding
dims, and the secrets-strategy warning.

---

## 4. Post-install wiring

These steps run **after** `helm install`, in order:

1. **Wait for Strimzi to reconcile Kafka** (when `kafka.strimzi.enabled=true`):
   ```sh
   kubectl -n asker wait kafka/asker-kafka --for=condition=Ready --timeout=10m
   kubectl -n asker get kafkatopic                       # docs.raw, docs.chunked, docs.enriched, ... Ready
   ```
   Confirm `config.kafkaBrokers` equals `asker-kafka-kafka-bootstrap:9092` (the workers read it as
   `KAFKA_BROKERS` from the shared ConfigMap).

2. **Deploy the Vespa application package** (when `vespa.deploy=true`). The StatefulSet stands up the
   nodes; the package (`services.xml` in ConfigMap `asker-vespa-app` + the committed
   `vespa/app/schemas/doc.sd`) must be activated against the config server — the query port `:8080`
   only serves **after** activation (`vespa/README.md` "How deployment works"). The chart does not
   ship the deploy Job; run the `vespa/deploy.sh` flow against the in-cluster config server (POST the
   package zip to `…/application/v2/tenant/default/prepareandactivate`):
   ```sh
   kubectl -n asker rollout status statefulset asker-vespa
   kubectl -n asker port-forward svc/vespa 19071:19071 &
   VESPA_CFG_URL=http://localhost:19071 EMBEDDING_DIM=1024 CLIP_DIM=512 bash vespa/deploy.sh
   # EMBEDDING_DIM/CLIP_DIM MUST equal config.embeddingDim/clipDim (ADR-005) or Vespa rejects vectors.
   ```

3. **Point envelope crypto at Vault** (prod KEK, ADR-015): set `VAULT_ADDR` (and the least-privilege
   token via Vault Agent / CSI / External Secrets) for **control-plane** and **connector-hub** — the
   two services that own envelope crypto. As of **M6** the provider selection is WIRED in both
   binaries (`platform/crypto.SelectKEK`): with `VAULT_ADDR` set they use `NewVaultKEK`; unset, they
   fall back to the file-KEK (`KEK_FILE`). Both services MUST agree on the same `KeyName`
   (`VAULT_KEK_KEY_NAME`, default `asker-kek`).
   > ✅ **Prod fail-closed guard (M6):** set **`ASKER_ENV=production`** on control-plane + connector-hub.
   > With it set and `VAULT_ADDR` empty, the service **errors at startup** rather than silently minting
   > an ephemeral dev file-KEK — so a misconfigured prod deploy fails loud, not insecure.
   > ⚠️ The chart still does **not** inject `VAULT_ADDR`/`VAULT_TOKEN` into the app workloads (the
   > `vault.*` values configure only the dev Vault Deployment + init Job). You must add `VAULT_ADDR` /
   > `VAULT_TOKEN` / `ASKER_ENV=production` to the control-plane and connector-hub pods yourself (Vault
   > Agent injector annotations, a CSI volume, or an External Secrets-synced env). Note: control-plane
   > also needs the MinIO creds (already in its `secretKeys`) because the M6 GDPR delete cascade purges
   > a deleted tenant's blobs.

4. **Provision credentials out-of-band** when `secrets.strategy` is `vault` or `external` — the chart
   renders no credentials; a Secret named `<release>-secrets` (helper `asker.secretName`) must exist,
   synced by the External Secrets Operator or Vault Agent. All three strategies resolve to the **same
   Secret name**, so workloads never branch on strategy (ADR-014 §7).

---

## 5. Verify the deployment (the make-e2e-smoke equivalent, in-cluster)

`make e2e-smoke` runs `tools/e2e/smoke.sh` against the compose stack. The same checks apply
in-cluster; run them via `kubectl port-forward` (or against the Ingress hosts directly):

1. **All workloads rolled out and Ready:**
   ```sh
   kubectl -n asker rollout status deploy -l app.kubernetes.io/instance=asker
   kubectl -n asker get pods -l app.kubernetes.io/part-of=asker      # all Running/Ready
   ```
2. **Gateway enforces OIDC and derives `tenant_id` from the verified token** (ADR-002): mint a token
   from Keycloak (dev realm users alice/bob, `password123`) and call `/v1/me` — `tenant_id` must
   equal the token `sub`, and a different user must get a different `tenant_id`:
   ```sh
   kubectl -n asker port-forward svc/keycloak 8081:8080 &
   kubectl -n asker port-forward deploy/asker-gateway 8080:8080 &
   TOK=$(curl -fsS -X POST http://localhost:8081/realms/asker/protocol/openid-connect/token \
     -d client_id=asker-web -d grant_type=password -d username=alice -d password=password123 \
     | python3 -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')
   curl -fsS -H "Authorization: Bearer $TOK" http://localhost:8080/v1/me        # -> {"tenant_id": "<alice sub>"}
   curl -s  -o /dev/null -w '%{http_code}\n' http://localhost:8080/v1/me        # no token -> 401
   ```
3. **Vespa streaming isolation** — feed a probe doc to one tenant's group and confirm a different
   tenant sees 0 hits (the sacred isolation check; `vespa/README.md` "Querying",
   `tools/e2e/smoke.sh`):
   ```sh
   kubectl -n asker port-forward svc/vespa 8082:8080 &
   curl -fsS -X POST http://localhost:8082/document/v1/asker/doc/group/probe-a/smoke-1 \
     -H 'Content-Type: application/json' \
     -d '{"fields":{"doc_id":"smoke-1","connector_id":"smoke","type":"FILE","title":"probe xyzzyplugh","body":"rare token xyzzyplugh","created_at":1718000000}}'
   # probe-a sees 1, probe-b sees 0:
   for g in probe-a probe-b; do
     curl -fsS -G http://localhost:8082/search/ \
       --data-urlencode 'yql=select * from sources * where userQuery()' \
       --data-urlencode 'query=xyzzyplugh' --data-urlencode "streaming.groupname=$g"
   done
   curl -fsS -X DELETE http://localhost:8082/document/v1/asker/doc/group/probe-a/smoke-1   # cleanup
   ```
4. **The M4 chaos exit criterion** (kill any one pod → no failed queries beyond retry): with the
   query path at `replicaCount: 2` + `pdb.minAvailable: 1`, delete one `query`/`gateway` pod while a
   query loop runs; queries continue (load balanced to the surviving replica). This is what the CI
   chaos smoke automates against the `values-ci.yaml` profile.

---

## 6. Operate

- **Rollouts / blue-green / canary:** `docs/runbooks/blue-green-deploy.md`.
- **Backup & restore:** `docs/runbooks/backup-restore-postgres.md`, `…-vespa.md`, `…-minio.md`
  (and the cross-cutting **back up the KEK** warning in each — without the KEK every encrypted
  token/blob is unrecoverable).
- **Failure-mode runbooks** (Vespa down, Kafka lag, Keycloak/JWKS, TEI saturation, Redis fail-open,
  query SLO burn, …): `docs/runbooks/README.md` (M6, with the M4 backup/restore + blue-green here).

---

## 7. Dev-vs-prod gap (known, documented)

- **Compose is the inner loop** (ADR-003): boots in seconds on a laptop; the cluster path is for
  prod-shape validation + the chaos test. The two surfaces are kept in sync (compose is the source of
  truth; CI rendering the chart on every change is the drift guardrail, ADR-014 consequences).
- **Secrets:** dev uses the chart-rendered `secrets.strategy: chart` literals (mirroring compose);
  **prod MUST use `vault`/`external`** — never ship the dev literals (ADR-014 §7).
- **KEK:** dev uses the file-KEK shim (and the chart's NON-PRODUCTION dev Vault); **prod uses an
  external HA Vault** with a least-privilege token (ADR-015).
- **Kafka:** dev = Redpanda (compose); prod = Apache Kafka via Strimzi. Same Kafka API, only the
  deployment shape differs (ADR-003 §1).
- **Embedding model/dim:** dev = bge-small (384); prod = bge-m3 (1024). `config.embeddingDim` MUST
  match the TEI model **and** the deployed Vespa schema tensor type (ADR-005) — a mismatch rejects
  vectors.
- **Web URLs are baked at image build time:** `VITE_API_URL`/`VITE_KEYCLOAK_URL` are compiled into
  the `asker/web` bundle (web/Dockerfile ARG/ENV), **not** set at deploy time. A prod web release
  needs an image built with the prod URLs (CI build-arg) or a runtime-config shim (`values.yaml`
  `services.web` note, ADR-014 consequences).
- **mTLS:** NetworkPolicies are enforced (wave 1, `networkPolicy.enabled`), but internal **mTLS** is
  mesh-/cert-manager-provided and off by default — until a mesh is deployed, internal traffic is
  authorized-but-plaintext (ADR-016 "Honest gap"). Enable the mesh (or `tls.internal` + TLS-aware
  services) before a zero-trust go-live.
