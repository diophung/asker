# Runbook: Backup & restore — Vespa (search/index content cluster)

> Scope: the Vespa **streaming-mode content cluster** that holds every tenant's indexed documents.
> Self-hosted by the umbrella chart as the `asker-vespa` StatefulSet (`vespa.deploy=true`,
> multi-group; see `templates/search/*` and `vespa/README.md`). When you run Vespa Cloud or an
> operator-managed cluster, use its native backup and skip to **Verify**.

## What is in Vespa (and what "restore" can mean here)

Each tenant's documents live in **one streaming group** keyed by `tenant_id`
(`id:asker:doc:g=<tenant_id>:<doc_id>` — `vespa/README.md` "Tenant-group model"). Streaming mode
builds **no per-tenant inverted index or HNSW** — the per-tenant cost at rest is ~zero, so the
content node's document store *is* the data: backing up `/opt/vespa/var` (where the document store
lives) backs up all tenants' corpora.

Crucially, **Vespa is downstream of the Kafka pipeline and rebuildable.** A document in Vespa was
produced from `docs.raw` → ingest → enrich → index-writer (ADR-004). The authoritative inputs are
the connector sources + the blobs (MinIO) + the control-plane state (Postgres); Vespa is a
*derived* index. That gives two recovery strategies with very different RPO/RTO trade-offs.

## RPO / RTO

| Strategy                                           | RPO                          | RTO                                  | Notes                                                                 |
| -------------------------------------------------- | ---------------------------- | ------------------------------------ | --------------------------------------------------------------------- |
| PVC volume snapshot of each content node's `/opt/vespa/var` (CSI `VolumeSnapshot`) | snapshot cadence | minutes (re-attach) + redeploy package | fastest restore; crash-consistent per node                           |
| `vespa visit` document export (logical, per group or whole corpus) | export cadence | hours for large corpora (re-feed)    | portable, version-independent; the cloud-agnostic option             |
| **Rebuild from source** (re-run connectors → pipeline → re-index) | last successful sync | hours–days (full re-ingest + re-enrich) | no Vespa backup needed; the ultimate fallback, but slow & re-embeds  |

Recommended posture: **PVC snapshots** for fast operational recovery of the derived index, with the
**rebuild-from-source** path documented as the disaster fallback (because the Kafka pipeline can
always reconstruct the index). A document export is the portable middle option for migrations or
cross-cluster moves.

---

## Backup

Replace `-n asker` with your release namespace. Object names below are what the chart renders:
StatefulSet `asker-vespa`, pods `asker-vespa-0..N`, headless Service `asker-vespa-headless`, the
client-facing Service `vespa` (`config.vespaUrl=http://vespa:8080`), config server on `:19071`,
query/document API on `:8080`, and the application package in ConfigMap `asker-vespa-app`.

### Option A — PVC volume snapshot of each content node (recommended)

Each pod's data is one PVC from the `vespa-var` volumeClaimTemplate (`vespa-var-asker-vespa-<ord>`).

```sh
NS=asker
kubectl -n "$NS" get pvc -l app.kubernetes.io/component=search
#   -> vespa-var-asker-vespa-0  (one per content node: ...-0, ...-1, ... for multi-group)
```

Snapshot **every** content node's PVC at (approximately) the same time — in a multi-group cluster
each group holds a full copy, so a consistent set lets you restore any group. Use your CSI
`VolumeSnapshot` (cloud-agnostic API; the `VolumeSnapshotClass` is provider-specific — keep it in
per-env ops manifests, ADR-014 §5):

```sh
for pvc in $(kubectl -n "$NS" get pvc -l app.kubernetes.io/component=search -o name); do
  name=$(basename "$pvc")
  cat <<EOF | kubectl -n "$NS" apply -f -
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: ${name}-$(date -u +%Y%m%dT%H%M%SZ)
spec:
  source:
    persistentVolumeClaimName: ${name}
EOF
done
```

Snapshots are crash-consistent; Vespa recovers its store on restart. Pause feeding (scale
`index-writer` to 0) for a few seconds around the snapshot if you want a quieter point-in-time.

### Option B — logical document export (`vespa visit`)

Portable and version-independent — export documents as JSON. Run inside a content pod (the
`vespa` CLI / `vespa-visit` ships in the image). Export the whole corpus or one tenant group.

```sh
NS=asker
POD=$(kubectl -n "$NS" get pod -l app.kubernetes.io/component=search -o jsonpath='{.items[0].metadata.name}')

# Whole corpus (all tenant groups), newline-delimited document/v1 JSON:
kubectl -n "$NS" exec "$POD" -- vespa-visit > asker-vespa-export-$(date -u +%Y%m%dT%H%M%SZ).jsonl

# A single tenant's group (streaming selection by group id; mirrors the document/v1 group path):
kubectl -n "$NS" exec "$POD" -- vespa-visit --selection 'id.group=="<tenant_id>"' > tenant-<tenant_id>.jsonl
```

Store exports off-cluster, **encrypted at rest** (document bodies are tenant content). Also archive
the **application package** so the schema that produced the export is restorable verbatim:

```sh
kubectl -n asker get configmap asker-vespa-app -o yaml > asker-vespa-app-$(date -u +%Y%m%dT%H%M%SZ).yaml
```

### What is NOT backed up here

The Vespa application package (`services.xml` topology + `vespa/app/schemas/doc.sd`) is **config, not
data** — it is version-controlled in the repo and rendered by the chart (ConfigMap `asker-vespa-app`
+ committed `doc.sd`). It is redeployed as part of restore, not backed up as data. The
`EMBEDDING_DIM`/`CLIP_DIM` the package was deployed with **must match** the backed-up vectors
(ADR-005) — record the dims with every backup (they are `config.embeddingDim`/`config.clipDim`).

---

## Restore

### From a PVC snapshot (Option A)

1. Stop feeding so nothing writes during restore:
   ```sh
   kubectl -n asker scale deploy asker-index-writer --replicas=0
   ```
2. For each content node, re-create its PVC from the snapshot. Because PVCs are owned by the
   StatefulSet's `volumeClaimTemplate`, delete the StatefulSet with `--cascade=orphan` (keeps pods),
   delete the target PVC, re-create it with `spec.dataSource` referencing the `VolumeSnapshot`, then
   re-apply the chart (`helm upgrade`) so the StatefulSet re-adopts the restored PVCs:
   ```sh
   kubectl -n asker delete statefulset asker-vespa --cascade=orphan
   kubectl -n asker delete pod -l app.kubernetes.io/component=search
   # delete + recreate vespa-var-asker-vespa-<ord> with dataSource: <VolumeSnapshot>, then:
   helm upgrade asker deploy/helm/asker -n asker -f <your-values> --reuse-values
   ```
3. **Redeploy the application package** so the streaming content cluster has its schema/topology
   active (the query port `:8080` only serves after a package is activated — `vespa/README.md`).
   See [Redeploy the application package](#redeploy-the-application-package) below.

### From a document export (Option B)

1. Ensure the cluster is up and the **application package is deployed** (same dims as the export).
2. Re-feed the exported documents with `vespa feed` (document/v1 POST is idempotent — it creates or
   fully replaces, so replays are safe; `vespa/README.md` "Feeding"):
   ```sh
   POD=$(kubectl -n asker get pod -l app.kubernetes.io/component=search -o jsonpath='{.items[0].metadata.name}')
   kubectl -n asker exec -i "$POD" -- vespa feed - < asker-vespa-export-20260613T020000Z.jsonl
   ```
   The group is encoded in each document id, so per-tenant isolation is preserved on re-feed.

### From source (disaster fallback — no Vespa backup)

If neither a snapshot nor an export exists, rebuild the derived index from the authoritative inputs:
re-run connectors for each tenant (control-plane resets the sync cursor) so `docs.raw` → ingest →
enrich → index-writer repopulates Vespa. This requires Postgres (connector state) and MinIO (blobs)
to be intact, re-embeds everything (TEI/CLIP), and is the slowest path (hours–days at scale) — but
it needs **zero Vespa-specific backup**, which is the point of an event-sourced pipeline (ADR-004).

### Redeploy the application package

The chart ships the package in ConfigMap `asker-vespa-app` (`services.xml`) + the committed
`vespa/app/schemas/doc.sd`, but does **not** ship the deploy Job — the package is activated by a
one-shot step that POSTs the package zip to the config server's `prepareandactivate`, exactly the
`vespa/deploy.sh` flow (`vespa/README.md` "How deployment works"):

```sh
# Port-forward the config server (asker-vespa-0) and run the repo's deploy flow against it, OR run
# a one-shot pod in-cluster that zips services.xml (from the ConfigMap) + doc.sd and POSTs to
# http://asker-vespa-0.asker-vespa-headless:19071/application/v2/tenant/default/prepareandactivate
kubectl -n asker port-forward svc/vespa 19071:19071 &
VESPA_CFG_URL=http://localhost:19071 EMBEDDING_DIM=384 CLIP_DIM=512 bash vespa/deploy.sh
```

> `EMBEDDING_DIM`/`CLIP_DIM` MUST equal the dims the backed-up vectors were produced with (ADR-005);
> a mismatch changes the tensor type and Vespa rejects the existing/re-fed embeddings.

---

## Verify the restore

1. **Config server + container up:**
   ```sh
   kubectl -n asker exec asker-vespa-0 -- curl -fsS localhost:19071/state/v1/health   # status "up"
   kubectl -n asker exec asker-vespa-0 -- curl -fsS localhost:8080/state/v1/health    # status "up" (after package activate)
   ```
2. **A known tenant's documents are queryable, and isolation holds** — the same streaming-group
   query the M0 smoke uses (`tools/e2e/smoke.sh`, `vespa/README.md` "Querying"):
   ```sh
   kubectl -n asker port-forward svc/vespa 8080:8080 &
   # expect the known totalCount for the tenant:
   curl -fsS -G localhost:8080/search/ \
     --data-urlencode 'yql=select * from sources * where userQuery()' \
     --data-urlencode 'query=<a-known-term>' \
     --data-urlencode 'streaming.groupname=<tenant_id>'
   # a DIFFERENT tenant must see 0 of the first tenant's docs (the sacred isolation check):
   curl -fsS -G localhost:8080/search/ \
     --data-urlencode 'yql=select * from sources * where userQuery()' \
     --data-urlencode 'query=<a-known-term>' \
     --data-urlencode 'streaming.groupname=<other_tenant_id>'
   ```
3. **End-to-end through the gateway:** run the in-cluster smoke (`deploy/k8s-docs.md` "Verify") — a
   real OIDC-authenticated query returns the tenant's hits via gateway → query → Vespa.

## KEK note (cross-reference)

Vespa stores indexed document **text/vectors in cleartext** within the content node's store — it is
**not** envelope-encrypted at the application layer (unlike MinIO blobs and Postgres tokens). So a
Vespa backup does not require the KEK to be *readable*. But the **rebuild-from-source** restore path
re-reads MinIO blobs, which **are** DEK-encrypted, so that path still depends on the KEK being
intact — see `docs/runbooks/backup-restore-minio.md` and `docs/runbooks/backup-restore-postgres.md`
(the "back up the KEK or all encrypted blobs+tokens are unrecoverable" warning). Protect Vespa
backups (encrypt at rest, restrict access) since they contain plaintext tenant content.
