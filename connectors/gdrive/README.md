# gdrive — Google Drive connector

Indexes a connected Google Drive account into Asker as canonical `FILE`
documents, and is the **M2 ACL reference connector** (ADR-012): it populates
`Document.acl` from each file's sharing permissions so the pipeline captures
who-may-see-it for this shared source.

- **Connector ID:** `gdrive` (immutable; baked into every `doc_id`).
- **Auth:** OAuth2 (`sdk.AuthOAuth2`). The hub delivers a decrypted access token
  in `Config.Token`; the connector attaches it as `Authorization: Bearer <token>`
  on every request and never logs it.
- **Webhooks:** unsupported (`SupportsWebhook=false`). Drive push (`changes.watch`)
  needs a publicly reachable, channel-registered webhook endpoint — hub
  infrastructure deferred past M2. The hub polls via `IncrementalSync`.

## API subset (Drive v3, base `https://www.googleapis.com/drive/v3`)

| Call | Endpoint | Used by |
| :--- | :--- | :--- |
| Seed cursor | `GET /changes/startPageToken` | FullSync (captured *before* listing) |
| List files | `GET /files?q=trashed=false&fields=...&pageToken=...` | FullSync backfill |
| Get file | `GET /files/{id}?fields=...` | IncrementalSync (refresh a change with no embedded file) |
| List changes | `GET /changes?pageToken=...&fields=...` | IncrementalSync |
| List permissions | `GET /files/{id}/permissions?fields=...` | ACL for every emitted file |
| Export Google doc | `GET /files/{id}/export?mimeType=text/plain` | body of Google-native docs |
| Download text | `GET /files/{id}?alt=media` | body of `text/*` files |

The client speaks the REST API directly over `net/http` (no vendored SDK) for
precise control of these few endpoints and clean streaming of the non-JSON
export/media bodies.

## Config schema

```json
{
  "type": "object",
  "properties": { "base_url": { "type": "string" } }
}
```

- `base_url` (optional) — API endpoint override. Empty ⇒ real Drive; in dev/CI
  it points at the cassette replay server (or a fake). A Drive instance is
  otherwise fully described by its OAuth token, so no field is required.

`Validate` parses the config and, when a token is present, makes one cheap
authenticated `files.list?pageSize=1` round-trip; errors never contain the token.

## Cursor model

Cursors are opaque to the hub; this connector parses two shapes:

- `page:<changesPageToken>` — the **steady-state** incremental cursor (a Drive
  changes page token). `IncrementalSync` replays `changes.list` from it and
  returns `page:<newStartPageToken>`.
- `start:<changesStartToken>|files:<filesPageToken>` — a **mid-backfill
  checkpoint**. `<changesStartToken>` is the changes start token captured before
  the file listing began (so edits racing the backfill are caught by the first
  incremental pass and converge via idempotent `(doc_id, version_etag)`
  upserts); `<filesPageToken>` is the next `files.list` page. When the hub
  replays this into `IncrementalSync`, the connector resumes the backfill and
  finishes exactly like an uninterrupted `FullSync`.

A Drive `400`/`410` on `changes.list` (invalid page token), or an unparseable
cursor, surfaces as `sdk.ErrCursorExpired`; the hub restarts a full sync.

## Document mapping

| Document field | Source |
| :--- | :--- |
| `type` | `DocType_FILE` |
| `doc_id` | `sdk.DocID("gdrive", file.id)` |
| `source_native_id` | `file.id` |
| `title` | `file.name` (`(untitled)` when blank) |
| `body_text` | exported text (Google docs), downloaded bytes (`text/*`, ≤1 MB), or empty (other binaries — M3 extracts) |
| `participants` | `file.owners` → `{name, email, role:"owner"}` |
| `metadata` | `file_id`, `mime_type`, `web_view_link`, `parents` (comma-joined), `size` |
| `ts.created` / `ts.modified` | `file.createdTime` / `file.modifiedTime` (RFC 3339) |
| `version_etag` | `file.version`, falling back to `sha256(modifiedTime + md5Checksum)` |
| `acl` | from `permissions.list` — see below |

Folders (`application/vnd.google-apps.folder`) are skipped. Deletions
(`change.removed` or `file.trashed`) emit a tombstone: same `doc_id`,
`tombstone.deleted=true`, `deleted_at` set, no body.

## ACLs captured (ADR-012)

`Document.acl` is populated for every file (Drive is a shared source). From the
permissions feed:

- `allowed_principals` — one entry per non-deleted permission, in Drive's own
  identifier scheme: `type=user`/`group` → the `emailAddress`; `type=domain` →
  `domain:<domain>`; `type=anyone` → the literal `anyone` (link/public sharing).
  Sorted and de-duplicated for a stable, diff-able document.
- `is_private` — `true` when the file is shared with no one beyond the
  connecting account (only the owner's own user permission); `false` once any
  other user, group, domain, or `anyone` principal has access.

Per ADR-012, M2 **captures** ACLs but does not yet enforce them at query time;
the per-tenant isolation boundary is unchanged. The data is stored on every
document so turning on intra-tenant ACL filtering later needs no re-sync.

## Tests

`go test ./connectors/gdrive/...` runs the cassette-based contract test
(`contract_test.go`, fixtures under `testdata/`) plus unit and fake-server
tests — **no live API, ever** (ADR-011). The contract covers a multi-page
FullSync, an IncrementalSync with a change + a delete (tombstone), and a stale
cursor that surfaces `ErrCursorExpired`.
