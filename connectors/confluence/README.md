# Confluence connector

Indexes **Confluence Cloud** wiki pages into Asker as canonical `WIKI_PAGE`
documents. It is an M2 shared-source connector (ADR-012): it captures per-page
ACL fields but does not enforce them at query time.

## Source & API subset

Talks to the Confluence Cloud REST API (`/wiki/rest/api`) directly over
`net/http` — there is no vendored Atlassian Go SDK in the module. Only a small
read-only subset is used:

| Purpose            | Call                                                                                                   |
|--------------------|--------------------------------------------------------------------------------------------------------|
| Full backfill      | `GET /rest/api/content?type=page&status=current&expand=body.storage,version,space,history&limit&start` |
| Incremental (edit) | `GET /rest/api/content/search?cql=type=page and status=current and lastModified>="…" order by …`       |
| Incremental (del.) | `GET /rest/api/content/search?cql=type=page and status=trashed  and lastModified>="…" order by …`      |
| Validate probe     | `GET /rest/api/content?type=page&limit=1`                                                              |

Backfill uses **offset pagination** (`start`/`limit`, following `_links.next`).
Incremental uses **CQL** filtered on `lastModified`, ordered ascending.

## Configuration

`ConfigJSON` schema (see `Spec().ConfigSchema`):

| Field       | Required | Meaning                                                                          |
|-------------|----------|----------------------------------------------------------------------------------|
| `base_url`  | no       | Site REST root, e.g. `https://<site>.atlassian.net/wiki`. Defaults to a placeholder; real instances always set it. Contract tests point it at the replay server. |
| `space_key` | no       | Scope the sync to a single space key. Omit to sync every readable space.         |

### Authentication

`AuthType` is `AuthOAuth2`. The connector sends `Authorization: Bearer <token>`
on every request, where `<token>` is the already-decrypted credential the hub
places in `Config.Token`. Atlassian Cloud also accepts **Basic** auth
(`email:api_token`); if the hub is configured to pass a Basic credential that is
integrator/hub wiring outside this connector — this connector always uses
Bearer. The token is never logged.

## Cursor model

The incremental cursor is the **`lastModified` watermark** of the newest page
seen so far, formatted as `yyyy-MM-dd HH:mm` (the granularity CQL accepts for a
`lastModified` literal). `IncrementalSync` replays
`lastModified>="<cursor>" order by lastModified asc` for both the `current` and
`trashed` windows, so:

- the comparison is `>=` and ordering is ascending, making a re-run from the
  same minute idempotent (downstream `(doc_id, version_etag)` upserts dedupe);
- a CQL-by-timestamp cursor **never expires at the source**, so this connector
  **never returns `sdk.ErrCursorExpired`**. An unparseable cursor degrades to a
  full window (zero time) rather than failing.

`FullSync` checkpoints an opaque `offset:<n>` cursor after every page for
crash-resume bookkeeping and returns the `lastModified` watermark cursor that
`IncrementalSync` continues from.

## Deletions

For M2, deletions are modeled via the **trash**: `IncrementalSync` runs a second
CQL search with `status=trashed` over the same `lastModified` window and emits a
**tombstone** (`tombstone.deleted=true`, `deleted_at` set, no body) for each
trashed page, using the same `doc_id` so the pipeline removes it. (The
alternative — re-fetching each page and tombstoning on a 404 — was not used.)

## Captured fields

| Document field            | Source                                                              |
|---------------------------|---------------------------------------------------------------------|
| `doc_id`                  | `sdk.DocID("confluence", <page id>)`                                |
| `type`                    | `WIKI_PAGE`                                                          |
| `title`                   | `title`                                                             |
| `body_text`               | `body.storage.value` XHTML stripped to text                         |
| `version_etag`            | `version.number` (changes on every edit)                            |
| `participants`            | `history.createdBy` (role `author`) and `version.by` (role `editor`); each carries display name / email / accountId (as `handle`) |
| `metadata.page_id`        | `id`                                                                |
| `metadata.space_key`      | `space.key`                                                         |
| `metadata.space_name`     | `space.name`                                                        |
| `metadata.web_url`        | `_links.base` + `_links.webui`                                      |
| `metadata.version_number` | `version.number`                                                    |
| `metadata.status`         | `status` (`current` / `trashed`)                                    |
| `ts.created`              | `history.createdDate`                                               |
| `ts.modified`             | `version.when`                                                      |
| `acl`                     | captured per ADR-012 — see below                                    |

### ACLs (ADR-012: capture, do not enforce)

Confluence is a shared source, so the connector populates `AclInfo`. Per-page
and per-space **restrictions** are a separate `/content/{id}/restriction` call
not included in the page expansion, so for M2 the connector records the
conservative default — `is_private=false`, `allowed_principals=[]` — rather than
enumerating restriction principals. This is a documented limitation: capturing
real restriction principals is follow-up work, and ADR-012 already defers
**enforcement** of `allowed_principals` to a later milestone regardless.

## Webhooks

`SupportsWebhook=false`. Confluence push requires an installed Atlassian
Connect/Forge app to register webhooks (hub configuration deferred past M2), so
`HandleWebhook` returns `sdk.ErrWebhookUnsupported` and the hub polls.

## Tests

`go test ./connectors/confluence/...` runs hermetic contract tests against
committed cassettes in `testdata/` (ADR-011 — no live API call). Coverage of the
package is ~87%. The contract test asserts: the full backfill doc-id set and
cursor, an incremental change + tombstone with the advanced cursor, a no-change
incremental, and `ErrWebhookUnsupported`.
