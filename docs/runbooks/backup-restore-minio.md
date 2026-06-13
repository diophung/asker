# Runbook: Backup & restore — MinIO (blob store)

> Scope: the MinIO (S3-compatible) blob store that holds connector-fetched raw bytes — file
> attachments, media (images/video/audio), thumbnails. Self-hosted by the umbrella chart as the
> `asker-minio` StatefulSet (`stateful.minio.deploy=true`), bucket `asker-blobs`
> (`config.blobBucket`), API on `minio:9000` (`config.minioEndpoint`). When you run managed S3
> (`stateful.minio.deploy=false`, `config.minioEndpoint` at the managed endpoint), use the
> provider's replication/versioning and skip to **Verify**.

## What is in the blob store (and the encryption shape)

connector-hub writes every fetched blob to MinIO, **envelope-encrypted under the tenant's DEK**
(ADR-015): blob bytes are encrypted with the per-tenant DEK, the DEK is wrapped by the KEK, and the
wrapped DEK lives in Postgres `tenant_deks`. The blob object key is tenant-scoped
(e.g. `tenant42/thumbs/abc123.jpg`, per `vespa/README.md` media examples). The bucket is therefore
a set of **opaque ciphertext objects** — a MinIO backup preserves ciphertext faithfully, but the
plaintext is unrecoverable without the DEK (in Postgres) **and** the KEK that unwraps it.

> ⚠️ **The blobs are per-tenant envelope-encrypted; the KEK must be backed up too.** See
> [CRITICAL: back up the KEK](#critical-back-up-the-kek-too) and cross-reference
> `docs/runbooks/backup-restore-postgres.md` (which holds the wrapped DEKs and the same warning).
> Backing up MinIO **alone** gets you bytes you cannot decrypt.

## RPO / RTO

| Strategy                                         | RPO                  | RTO                          | Notes                                                       |
| ------------------------------------------------ | -------------------- | ---------------------------- | ----------------------------------------------------------- |
| Continuous `mc mirror --watch` to a 2nd site/bucket | seconds–minutes  | minutes (mirror back)        | best RPO; needs a second MinIO/S3 target                    |
| Scheduled `mc mirror` (CronJob)                  | schedule interval    | minutes (mirror back)        | simple; default V1 recommendation                           |
| Bucket versioning + replication (MinIO/S3 native)| seconds              | minutes                      | also protects against overwrite/delete corruption           |
| PVC volume snapshot (CSI `VolumeSnapshot`)       | snapshot cadence     | minutes (re-attach)          | crash-consistent; whole `/data`                             |

Blobs are immutable once written (content-addressed style keys), so corruption is mainly
delete/overwrite — **bucket versioning** is the cheapest protection against that, complemented by an
off-site `mc mirror` for site loss. Recommended V1 baseline: enable versioning + a scheduled `mc
mirror` to a second bucket in a different failure domain.

---

## Backup

`mc` (the MinIO client) is the workhorse. Run it from a one-shot pod / CronJob in-cluster (it reaches
`minio:9000` by Service DNS) or from a host with the API port-forwarded. Creds are the dev
`secrets.dev.minioAccessKey`/`minioSecretKey` (`asker-minio`/`asker-minio-secret`), or in prod from
the Secret (`asker-secrets`, keys `MINIO_ACCESS_KEY`/`MINIO_SECRET_KEY`) / Vault.

### Option A — `mc mirror` to a second bucket or site (recommended)

```sh
# Configure mc aliases for the source (in-cluster) and a backup target (different site/bucket).
mc alias set src    http://minio:9000        "$MINIO_ACCESS_KEY" "$MINIO_SECRET_KEY"
mc alias set backup https://backup.example    "$BACKUP_ACCESS_KEY" "$BACKUP_SECRET_KEY"

# One-shot mirror of the blob bucket (add --remove to make the target an exact copy):
mc mirror --overwrite src/asker-blobs backup/asker-blobs-mirror

# Or continuous mirroring (lowest RPO):
mc mirror --watch --overwrite src/asker-blobs backup/asker-blobs-mirror
```

Scheduled form (CronJob — add to ops manifests, not the chart). The `minio/mc` image dials the
in-cluster `minio` Service; the backup target is your off-site bucket:

```yaml
apiVersion: batch/v1
kind: CronJob
metadata: { name: asker-minio-backup, namespace: asker }
spec:
  schedule: "0 */6 * * *"             # every 6h
  concurrencyPolicy: Forbid
  jobTemplate:
    spec:
      ttlSecondsAfterFinished: 86400
      template:
        spec:
          restartPolicy: OnFailure
          securityContext: { runAsNonRoot: true, runAsUser: 1000, seccompProfile: { type: RuntimeDefault } }
          containers:
            - name: mc
              image: minio/mc:latest
              command: ["/bin/sh", "-ec"]
              args:
                - |
                  mc alias set src    http://minio:9000 "$SRC_AK" "$SRC_SK"
                  mc alias set backup "$BACKUP_URL"      "$BK_AK"  "$BK_SK"
                  mc mirror --overwrite src/asker-blobs backup/asker-blobs-mirror
              env:
                - { name: SRC_AK, valueFrom: { secretKeyRef: { name: asker-secrets, key: MINIO_ACCESS_KEY } } }
                - { name: SRC_SK, valueFrom: { secretKeyRef: { name: asker-secrets, key: MINIO_SECRET_KEY } } }
                # BACKUP_URL/BK_AK/BK_SK from your off-site target's Secret (provision separately).
                - { name: BACKUP_URL, valueFrom: { secretKeyRef: { name: asker-minio-backup-target, key: url } } }
                - { name: BK_AK,      valueFrom: { secretKeyRef: { name: asker-minio-backup-target, key: accessKey } } }
                - { name: BK_SK,      valueFrom: { secretKeyRef: { name: asker-minio-backup-target, key: secretKey } } }
              securityContext: { allowPrivilegeEscalation: false, readOnlyRootFilesystem: false, capabilities: { drop: ["ALL"] } }
```

> The mirrored objects are **still ciphertext** — the off-site copy is as safe as the source and is
> also useless without the KEK + the Postgres `tenant_deks`. Encrypt the transport (`https://`
> target) and restrict the target's access regardless, since it is tenant data.

### Option B — enable versioning (corruption / accidental-delete protection)

```sh
mc version enable src/asker-blobs       # keeps prior object versions; restore a version with `mc cp --version-id`
```

### Option C — PVC volume snapshot (crash-consistent)

Snapshot the StatefulSet PVC `data-asker-minio-0` via your CSI `VolumeSnapshot` (cloud-agnostic API;
provider-specific `VolumeSnapshotClass` lives in ops manifests, ADR-014 §5):

```sh
kubectl -n asker get pvc -l app.kubernetes.io/component=minio    # -> data-asker-minio-0
```

---

## Restore

> **Quiesce the writer first.** connector-hub is the only blob writer (ADR-016 netpol allows MinIO
> ingress *only* from connector-hub). Scale it to 0 during a restore-in-place:
> `kubectl -n asker scale deploy asker-connector-hub --replicas=0`.

### From a mirror (Option A)

Mirror back into the bucket. `mc mirror` only transfers missing/changed objects, so this is safe to
re-run:

```sh
mc alias set src    http://minio:9000     "$MINIO_ACCESS_KEY" "$MINIO_SECRET_KEY"
mc alias set backup https://backup.example "$BACKUP_ACCESS_KEY" "$BACKUP_SECRET_KEY"

# Ensure the bucket exists, then mirror the backup back into it:
mc mb --ignore-existing src/asker-blobs
mc mirror --overwrite backup/asker-blobs-mirror src/asker-blobs
```

### From a version (Option B)

```sh
mc ls --versions src/asker-blobs/<object-key>
mc cp --version-id <vid> src/asker-blobs/<object-key> src/asker-blobs/<object-key>
```

### From a PVC snapshot (Option C)

Re-create `data-asker-minio-0` from the `VolumeSnapshot` while `asker-minio` is scaled to 0 (delete
the StatefulSet `--cascade=orphan`, delete + recreate the PVC with `dataSource:` the snapshot, then
`helm upgrade` to re-adopt). MinIO serves the restored objects on restart.

### Bring the writer back

```sh
kubectl -n asker scale deploy asker-connector-hub --replicas=2
kubectl -n asker rollout status deploy asker-connector-hub
```

---

## Verify the restore

1. **MinIO is healthy:**
   ```sh
   kubectl -n asker exec asker-minio-0 -- curl -fsS localhost:9000/minio/health/ready   # 200
   ```
2. **Bucket + object counts are sane** (compare against the source / your expectation):
   ```sh
   mc ls src/asker-blobs --recursive --summarize | tail -3      # object count + total size
   ```
3. **Object integrity** — spot-check a known key's ETag/size against the backup:
   ```sh
   mc stat src/asker-blobs/<known-key>
   mc stat backup/asker-blobs-mirror/<known-key>
   ```
4. **End-to-end decrypt** (the real test — proves the restored ciphertext decrypts under the current
   DEK+KEK): authenticate a tenant whose document references a restored blob and fetch the blob
   through the gateway media path. A successful, correctly-decrypted fetch proves the
   blob → DEK (Postgres) → KEK chain is whole. A decrypt failure means either the blob bytes are
   wrong (restore them) or the KEK/DEK don't match the blobs (wrong KEK — see below).

---

## CRITICAL: back up the KEK too

Every blob is encrypted under a tenant DEK; the DEK is wrapped by the KEK (ADR-015). A MinIO backup
captures **ciphertext only**. Without the matching KEK (and the wrapped DEKs in Postgres
`tenant_deks`) the entire blob store is unrecoverable plaintext-wise. The KEK binds the tenant ID as
AAD/derivation context, so a DEK cannot be unwrapped under a different KEK — restoring blobs against
a fresh KEK leaves them permanently undecryptable.

- **Prod (Vault Transit, ADR-015):** back up **Vault** (Raft snapshot + unseal/recovery material) on
  the same cadence — the `asker-kek` Transit key is what makes every blob readable.
- **Dev/CI (file-KEK):** back up the `asker-...-kek` Secret material.

Treat **MinIO + Postgres + the KEK as one recoverable unit**: restoring any one without the others
yields either undecryptable bytes or orphaned keys. The end-to-end decrypt in **Verify** step 4 is
the test that all three are a matched set. See `docs/runbooks/backup-restore-postgres.md` and
`docs/adr/ADR-015-vault-kek.md`.
