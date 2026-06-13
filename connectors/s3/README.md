# s3 — S3-compatible object storage connector

Indexes a connected S3-compatible bucket (AWS S3, MinIO, Cloudflare R2,
Backblaze B2, ...) into Asker as canonical `FILE` documents. It speaks the S3
REST API through the [`minio-go/v7`](https://github.com/minio/minio-go) client,
which addresses any S3-compatible endpoint uniformly.

- **Connector ID:** `s3` (immutable; baked into every `doc_id`).
- **Auth:** static key pair (`sdk.AuthToken`). The hub vault stores the
  credential as the single string `"<accessKeyID>:<secretAccessKey>"` and
  delivers it, already decrypted, in `Config.Token`. The connector splits that
  pair into a minio-go static-V4 credential and never logs or persists either
  half.
- **Webhooks:** unsupported (`SupportsWebhook=false`). S3 event notifications
  need source-side wiring (SQS/SNS/Lambda) and a publicly reachable endpoint —
  hub infrastructure deferred past M2. The hub polls via `IncrementalSync`.

## Config schema

```json
{
  "type": "object",
  "properties": {
    "endpoint": {"type": "string"},
    "bucket":   {"type": "string"},
    "prefix":   {"type": "string"},
    "region":   {"type": "string"},
    "use_ssl":  {"type": "boolean"}
  },
  "required": ["endpoint", "bucket"]
}
```

- `endpoint` (**required**) — host[:port] of the S3 service, e.g.
  `s3.amazonaws.com`, `127.0.0.1:9000` (MinIO), `<account>.r2.cloudflarestorage.com`.
  It may also carry an `http(s)://` scheme; the connector strips it to the
  host[:port] minio-go wants and infers `use_ssl` from it. In dev/CI the contract
  harness injects the replay server's full URL into this single field, which is
  why a scheme is tolerated.
- `bucket` (**required**) — the bucket to index.
- `prefix` (optional) — list only objects under this key prefix.
- `region` (optional) — the bucket's region. Setting it avoids a
  `GetBucketLocation` round-trip; when absent the connector defaults to
  `us-east-1` so a fake/replay endpoint needs no extra interaction.
- `use_ssl` (optional) — use HTTPS to the endpoint (default `false`).

`Validate` parses the config, checks the credential shape, and — when a token is
present — makes one cheap `ListObjects(MaxKeys=1)` round-trip; an empty but
reachable bucket passes. Errors never contain the credential.

## Credential format

The token is the two-part S3 key pair joined by a colon:

```
<accessKeyID>:<secretAccessKey>
```

The hub vault stores and delivers it as this single opaque string.

## Cursor model

S3 has **no change feed**, so the cursor is a small JSON blob the connector both
produces and parses, carrying everything `IncrementalSync` needs to reconcile a
fresh re-list against the previous pass:

```json
{"k": "<last object key listed>", "m": "<RFC3339 max LastModified>", "keys": ["a", "b", ...]}
```

- `k` — the lexicographically-last key listed so far. A **mid-backfill
  checkpoint** sets it; the hub replays the checkpoint verbatim into
  `IncrementalSync`, which resumes the listing past `k` (S3 lists keys sorted, so
  `ListObjects StartAfter` continues exactly where the interrupted backfill
  stopped). Empty once a pass has completed.
- `m` — the maximum `LastModified` observed across the keyset. The next
  `IncrementalSync` re-lists and emits an **upsert** for any object whose
  `LastModified` is strictly newer than `m` (a create or overwrite).
- `keys` — the compact, sorted set of keys present at the end of the pass.
  `IncrementalSync` diffs it against the freshly-listed keyset and emits a
  **tombstone** for every key that has since vanished.

This bounded-keyset approach detects deletions on **every** incremental re-list
(not only on a periodic full re-list) at the cost of carrying the keyset in the
cursor — the honest M2 trade-off for a source with no delete feed. An
unparseable cursor surfaces as `sdk.ErrCursorExpired`, so the hub restarts a full
sync rather than looping on a poison cursor.

## Document mapping

| Document field | Source |
| :--- | :--- |
| `type` | `DocType_FILE` |
| `source_native_id` | `"<bucket>/<key>"` |
| `doc_id` | `sdk.DocID("s3", "<bucket>/<key>")` |
| `title` | the key's basename |
| `body_text` | the object bytes for small (`≤ 1 MiB`) text objects; empty for larger or binary objects (M3 extracts) |
| `metadata` | `bucket`, `key`, `etag`, `size`, `storage_class`, `content_type` |
| `ts.created` / `ts.modified` | the object's `LastModified` (S3 exposes no creation time, so both mirror it) |
| `version_etag` | the object `ETag` (unquoted), falling back to `sha256(key+size)` |
| `acl` | unset — an S3 bucket is single-owner from the connecting credential's view (ADR-012) |

**Body extraction.** Only objects that are both small (`≤ bodyCap`, 1 MiB) and
likely text (MIME type derived from the key extension: `text/*`, `application/json`,
`application/xml`, `application/yaml`, `application/csv`) are downloaded — one
capped `GetObject` per such object, using the GET response's `Content-Type` for
metadata. Everything else is indexed metadata-only. A body-fetch error degrades
to a metadata-only document rather than failing the whole sync.

**Deletions** emit a tombstone: the same `doc_id`, `tombstone.deleted=true`,
`deleted_at` set, a `deleted-<unixnano>` `version_etag` (newer than any prior
upsert so the delete wins the idempotent merge), and no body.

## Tests

`go test ./connectors/s3/...` runs the cassette-based contract test
(`contract_test.go`, fixtures under `testdata/`) plus unit and fake-server
tests — **no live API, ever** (ADR-011). The cassettes hold hand-authored S3
REST XML (`ListObjectsV2` results with `Contents`/`Key`/`ETag`/`LastModified`/
`Size`/`StorageClass`, `IsTruncated`, `NextContinuationToken`) and `GetObject`
bodies. The contract covers a multi-page `FullSync` (with text bodies fetched
and binary/oversized objects left empty), an `IncrementalSync` with a change
(newer `LastModified`) plus a deletion (a vanished key → tombstone), and a stale
cursor that surfaces `ErrCursorExpired`.

## Integrator note

`New(opts ...sdk.Option)` returns the connector as `sdk.Connector` with a
`WithLogger(*slog.Logger)` option, matching the wave-1 connectors so the
connector-hub registry wires it uniformly. Registering it in the hub registry is
integrator work (outside this connector's directory).
