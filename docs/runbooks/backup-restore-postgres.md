# Runbook: Backup & restore — Postgres (control-plane store)

> Scope: the control-plane Postgres database, the system of record for **tenants**,
> **connector_instances**, **connector OAuth/API tokens**, and **tenant_deks** (the wrapped
> per-tenant Data Encryption Keys). Self-hosted by the umbrella chart as the
> `asker-postgres` StatefulSet (`stateful.postgres.deploy=true`); when you run managed Postgres
> (`stateful.postgres.deploy=false`, `config.postgresHost` pointed at it), use your provider's
> snapshot/PITR instead and skip to **Verify**.

## What is in this database (and why it matters)

The control-plane is the tenant/connector registry + token vault (ADR-009, ADR-014 §4). Losing it
loses the ability to drive connectors and to decrypt anything. The relevant tables:

| Table                  | Contents                                                              |
| ---------------------- | --------------------------------------------------------------------- |
| `tenants`              | tenant rows (one per verified OIDC subject, ADR-002)                  |
| `connector_instances`  | per-tenant connector config + sync cursors                            |
| `tokens`               | connector OAuth/API tokens, **encrypted under the tenant DEK**        |
| `tenant_deks`          | the per-tenant DEK, stored **only in wrapped form** (ADR-015)         |

> ⚠️ **`tenant_deks` rows are envelope-encryption DEKs, useless without the KEK.** Each row is a
> DEK wrapped by the Key Encryption Key (file-KEK in dev, **Vault Transit** in prod — ADR-015).
> A Postgres backup alone does **not** let you decrypt `tokens` (or the MinIO blobs that share the
> same DEK). **You MUST also back up the KEK** — see [Back up the KEK](#critical-back-up-the-kek-too)
> below. A Postgres restore against a *different* KEK leaves every `tenant_deks` row un-unwrappable
> and every token/blob permanently unrecoverable.

## RPO / RTO

| Strategy                                  | RPO (data loss window) | RTO (time to restore) | Notes                                              |
| ----------------------------------------- | ---------------------- | --------------------- | -------------------------------------------------- |
| Nightly logical dump (CronJob, below)     | up to 24h              | minutes–tens of mins  | simplest; default recommendation for V1            |
| Hourly dump                               | up to 1h               | minutes–tens of mins  | bump the CronJob schedule                          |
| PVC volume snapshot (CSI `VolumeSnapshot`)| snapshot cadence       | minutes               | crash-consistent; restore = new PVC from snapshot  |
| Managed Postgres PITR                     | seconds (WAL)          | provider-dependent     | when `stateful.postgres.deploy=false`              |

The control-plane is small (tenant/connector metadata + tokens, not document corpora), so logical
dumps are fast and compact; a nightly `pg_dump` is the recommended V1 baseline (RPO ≤ 24h). Tighten
the schedule if connector churn warrants it.

---

## Backup

The dev creds match `secrets.dev` (`asker`/`asker`); in prod the password lives in the Secret
(`asker-secrets`, key `POSTGRES_PASSWORD`) or Vault. Replace `-n asker` with your release namespace.

### Option A — on-demand logical dump (`pg_dump`)

```sh
NS=asker
POD=$(kubectl -n "$NS" get pod -l app.kubernetes.io/component=postgres -o jsonpath='{.items[0].metadata.name}')

# Custom-format dump (compressed, restorable with pg_restore; -Fc is self-describing).
kubectl -n "$NS" exec "$POD" -- \
  env PGPASSWORD="$(kubectl -n "$NS" get secret asker-secrets -o jsonpath='{.data.POSTGRES_PASSWORD}' | base64 -d)" \
  pg_dump -U asker -d asker -Fc \
  > "asker-pg-$(date -u +%Y%m%dT%H%M%SZ).dump"

# Sanity: the file is non-empty and is a pg custom-format archive.
ls -lh asker-pg-*.dump
```

Store the dump off-cluster (object storage in a *different* failure domain than the cluster and the
MinIO/blob store). Encrypt at rest — it contains wrapped tokens/DEKs.

### Option B — scheduled dump to a PVC (CronJob)

Drop this alongside the release (it is **not** part of the chart — add it to your ops manifests, or
propose it to the integrator). It dumps nightly to a dedicated PVC; ship that PVC's contents to
off-cluster storage with your existing backup agent (or extend the container to `mc cp` / `aws s3
cp`). Adjust `image` to the same Postgres major version as `stateful.postgres.image` (`postgres:17`).

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: asker-pg-backup
  namespace: asker
spec:
  schedule: "0 2 * * *"            # 02:00 UTC nightly  -> RPO <= 24h
  concurrencyPolicy: Forbid
  successfulJobsHistoryLimit: 7
  failedJobsHistoryLimit: 3
  jobTemplate:
    spec:
      ttlSecondsAfterFinished: 86400
      template:
        spec:
          restartPolicy: OnFailure
          securityContext: { runAsNonRoot: true, runAsUser: 999, fsGroup: 999, seccompProfile: { type: RuntimeDefault } }
          containers:
            - name: pgdump
              image: postgres:17       # MUST match stateful.postgres.image major version
              command: ["/bin/sh", "-ec"]
              args:
                - |
                  ts=$(date -u +%Y%m%dT%H%M%SZ)
                  pg_dump -h postgres -U asker -d asker -Fc > "/backup/asker-pg-${ts}.dump"
                  # Retain 7 days locally; off-cluster shipping is your backup agent's job.
                  find /backup -name 'asker-pg-*.dump' -mtime +7 -delete
              env:
                - name: PGPASSWORD
                  valueFrom: { secretKeyRef: { name: asker-secrets, key: POSTGRES_PASSWORD } }
              securityContext: { allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: { drop: ["ALL"] } }
              volumeMounts:
                - { name: backup, mountPath: /backup }
          volumes:
            - name: backup
              persistentVolumeClaim: { claimName: asker-pg-backup }
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: asker-pg-backup, namespace: asker }
spec:
  accessModes: ["ReadWriteOnce"]
  resources: { requests: { storage: 5Gi } }     # storageClassName "" => cluster default (ADR-014)
```

> `postgres` is the in-cluster Service DNS name (bare, == compose name — ADR-014 §3), so the CronJob
> dials `-h postgres` exactly as the control-plane does.
>
> ⚠️ **NetworkPolicy:** with `networkPolicy.enabled=true` (default) on a policy-enforcing CNI, the
> `asker-postgres-allow` policy permits ingress to Postgres **only from `component: control-plane`**.
> This backup pod carries no such label, so its `pg_dump` connection is denied. Give the CronJob pod
> a permitted identity — either label its pod template `app.kubernetes.io/component: control-plane`
> (+ `part-of: asker`), or add a dedicated allow policy targeting Postgres from the backup pod's
> labels. The `kubectl exec`-based options above are unaffected (exec is not subject to NetworkPolicy).

### Option C — PVC volume snapshot (crash-consistent)

If your CSI driver supports `VolumeSnapshot`, snapshot the StatefulSet's PVC
(`data-asker-postgres-0`). This is crash-consistent (Postgres recovers WAL on next start), not a
logical export; restore re-creates a PVC from the snapshot. Cloud-agnostic (CSI `VolumeSnapshot`),
but the `VolumeSnapshotClass` is provider-specific — keep it in per-environment ops manifests, not
the chart (ADR-014 §5).

```sh
kubectl -n asker get pvc -l app.kubernetes.io/component=postgres   # -> data-asker-postgres-0
```

---

## Restore

> **Stop writers first.** Scale control-plane to 0 so it does not write during the restore (it is the
> only Postgres client — ADR-016 netpol allows Postgres ingress *only* from control-plane).

```sh
NS=asker
kubectl -n "$NS" scale deploy asker-control-plane --replicas=0
```

### Restore from a logical dump (Option A/B)

```sh
NS=asker
POD=$(kubectl -n "$NS" get pod -l app.kubernetes.io/component=postgres -o jsonpath='{.items[0].metadata.name}')
PGPW=$(kubectl -n "$NS" get secret asker-secrets -o jsonpath='{.data.POSTGRES_PASSWORD}' | base64 -d)

# Recreate a clean database and restore into it. --clean --if-exists drops objects first; -1 wraps
# the restore in a single transaction so a failure leaves nothing half-applied.
kubectl -n "$NS" exec -i "$POD" -- env PGPASSWORD="$PGPW" \
  pg_restore -U asker -d asker --clean --if-exists -1 \
  < asker-pg-20260613T020000Z.dump
```

### Restore from a PVC snapshot (Option C)

Re-create `data-asker-postgres-0` from the `VolumeSnapshot` while the StatefulSet is scaled to 0
(delete the StatefulSet keeping the pods absent, recreate the PVC `dataSource:` the snapshot, then
re-apply / `helm upgrade`). Postgres replays WAL on first boot.

### Bring writers back

```sh
kubectl -n asker scale deploy asker-control-plane --replicas=2     # or your prod replicaCount
kubectl -n asker rollout status deploy asker-control-plane
```

---

## Verify the restore

1. **Postgres is healthy:**
   ```sh
   kubectl -n asker exec "$POD" -- pg_isready -U asker -d asker        # -> accepting connections
   ```
2. **Row counts are sane** (compare against your pre-incident expectation):
   ```sh
   kubectl -n asker exec "$POD" -- env PGPASSWORD="$PGPW" \
     psql -U asker -d asker -c \
     "select 'tenants' t, count(*) from tenants
        union all select 'connector_instances', count(*) from connector_instances
        union all select 'tokens', count(*) from tokens
        union all select 'tenant_deks', count(*) from tenant_deks;"
   ```
3. **control-plane comes ready:** `kubectl -n asker rollout status deploy asker-control-plane` and
   its `/readyz` (`:9101`) passes.
4. **End-to-end decrypt works** (proves the restored `tenant_deks` unwrap under the *current* KEK —
   the real test that the KEK and the dump are a matched pair): run the smoke/e2e flow
   (`docs/runbooks/blue-green-deploy.md` and `deploy/k8s-docs.md` cover the in-cluster smoke). A
   tenant whose connector token decrypts and lists/syncs proves the DEK→token chain end to end. If
   decrypt fails with an unwrap/AAD error, you restored against the **wrong KEK** — see below.

---

## CRITICAL: back up the KEK too

`tenant_deks` holds DEKs **wrapped by the KEK**; `tokens` (here) and the MinIO blobs
(`backup-restore-minio.md`) are encrypted under those DEKs. **A Postgres backup without the matching
KEK is undecryptable.** The KEK is bound to the tenant ID as AAD/derivation context (ADR-015), so a
DEK wrapped under one KEK cannot be unwrapped under another — restoring a dump against a fresh KEK
bricks every token and blob.

- **Prod (Vault Transit, `vault.deploy=false` + external HA Vault, ADR-015):** the `asker-kek`
  Transit key never leaves Vault. **Back up Vault** (Raft snapshot: `vault operator raft snapshot
  save`, plus the unseal/recovery keys / auto-unseal KMS material) on at least the same cadence as
  Postgres, and store it in a *separate* trust domain. Restoring Postgres is meaningless without the
  Vault that holds `asker-kek`.
- **Dev/CI (file-KEK):** the 32-byte key is in the `asker-...-kek` Secret (mounted at `/keys/kek.bin`
  on control-plane/connector-hub). Back up that Secret material alongside the dump.

Treat the KEK backup and the Postgres backup as a **single recoverable unit**: test restore of both
together (the e2e decrypt in step 4 above is that test). See `docs/adr/ADR-015-vault-kek.md`.
