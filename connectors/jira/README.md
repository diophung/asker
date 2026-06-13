# Jira connector (`jira`)

Indexes issues from a **Jira Cloud** site (Atlassian) into Asker as canonical
`Document`s of type `TICKET`. Implements the `sdk.Connector` interface
(`connectors/sdk`); the hub owns OAuth, token refresh, scheduling, retries, and
Kafka — this connector only reads Jira and emits Documents.

## Source & API subset

The connector talks to the Jira Cloud REST v3 API using exactly **one** read
endpoint:

| Call | Used for |
| :--- | :--- |
| `POST /rest/api/3/search` | both FullSync and IncrementalSync, driven by JQL, paginated with `startAt` / `maxResults` / `total` |

Each search requests only the fields the mapping reads
(`fields: summary, description, status, issuetype, priority, project, reporter,
assignee, creator, created, updated, comment`) to keep responses small.

- **FullSync** issues `jql = "order by updated asc"` and pages through the whole
  result set.
- **IncrementalSync** issues `jql = "updated >= '<cursor>' order by updated
  asc"`, where `<cursor>` is the newest issue's `updated` time formatted to
  Jira's documented JQL minute precision (`yyyy/MM/dd HH:mm`).

The `Validate` credential check is a single-page search (`maxResults=1`).

## Auth

`AuthType = AuthOAuth2`. The hub runs the OAuth flow and delivers a decrypted
access token in `Config.Token`; the connector adds `Authorization: Bearer
<token>` to every request via a custom `http.RoundTripper`. The token is never
logged, persisted, or placed in an error message. OAuth scope provisioning
(e.g. `read:jira-work`) is hub/integrator work.

## Config schema

```json
{
  "type": "object",
  "properties": {
    "base_url":         {"type": "string"},
    "project_keys":     {"type": "array", "items": {"type": "string"}},
    "deleted_statuses": {"type": "array", "items": {"type": "string"}}
  }
}
```

- `base_url` — the Jira site, e.g. `https://acme.atlassian.net`. Defaults to a
  placeholder; contract tests inject the replay server URL here. The REST path
  `/rest/api/3/search` is appended to it.
- `project_keys` — optional allow-list of project keys; when set, a
  `project in ('A', 'B')` clause is ANDed into the JQL so only those projects
  sync. Empty means every project the token can read.
- `deleted_statuses` — optional list of status names that mean "deleted" at the
  source (see **Deletions** below).

All fields are optional; an empty config is valid.

## Cursor model

Cursors are opaque to the hub. The wire shape is:

```
updated:<yyyy/MM/dd HH:mm>|key:<lastIssueKey>
```

`<updated>` is the JQL-formatted `updated` time of the newest issue seen so far;
`<key>` is that issue's key. Because JQL `updated >=` is **inclusive** and only
**minute-granular**, the boundary issue is re-returned by the next poll — the
connector dedupes it by skipping any issue whose key equals the cursor's
boundary key, so a steady-state poll with no changes emits nothing and returns
the same cursor.

A mid-backfill checkpoint uses the same shape: `FullSync` checkpoints the cursor
of the last issue on each completed page, and the hub replays that verbatim into
`IncrementalSync`, which simply continues the `updated >=` scan from there.
Overlap is harmless — convergence is via idempotent `(doc_id, version_etag)`
upserts. The empty cursor means "from the beginning of time".

A cursor bound the source rejects (HTTP 400 from search on a malformed/expired
`updated` bound) is returned wrapped as `sdk.ErrCursorExpired`, so the hub
restarts a full sync rather than looping on a poison cursor.

## Field mapping

| Document field | Source |
| :--- | :--- |
| `type` | `TICKET` |
| `doc_id` | `sdk.DocID("jira", <issue key>)` |
| `source_native_id` | issue key (e.g. `DEMO-1`) |
| `title` | `"<key>: <summary>"` (just `<key>` when summary is empty) |
| `body_text` | ADF `description` rendered to plain text, then each comment as `"<author>: <text>"`, separated by blank lines |
| `participants` | `reporter`, `assignee`, `creator` → `Participant{name=displayName, email=emailAddress, handle=accountId, role}` |
| `metadata` | `issue_key`, `issue_id`, `project` (key), `status` (name), `issue_type`, `priority`, `web_url` (`<base>/browse/<key>`) |
| `version_etag` | `fields.updated` — bumps on any issue change, so upserts are idempotent |
| `ts.created` / `ts.modified` | `fields.created` / `fields.updated` |
| `ts.ingested` | left unset — the hub stamps it |

### ADF → text extraction

Jira Cloud returns issue descriptions and comments as **Atlassian Document
Format** (ADF): a JSON node tree (`{"type":"doc","content":[...]}`). The
connector walks the tree (`adf.go`), concatenating the text of every leaf in
document order and inserting a newline between block-level nodes (paragraphs,
headings, list items, blockquotes, code blocks, table cells, ...) so structure
survives for downstream chunking. Inline nodes without a `text` leaf still
contribute their label: `mention` → its `attrs.text`, `emoji` → `attrs.text` or
`attrs.shortName`, `inlineCard` → its `attrs.url`, `hardBreak` → a newline.
Marks (bold, links, color), media, and layout produce no markup — only
human-readable text is kept. A `description` delivered as a bare JSON string
(older Jira, or a wiki-renderer-disabled site) is handled too.

## ACLs

`Document.acl` is **not** populated. Per ADR-012, shared-source connectors
populate `AclInfo` "as their permission model maps onto principals", but the
`/rest/api/3/search` response does not carry per-issue permission/visibility
data — obtaining it would require an extra `GET
/rest/api/3/issue/{key}/permissions`-style call per issue, which is out of scope
for M2's read-only search path. Per-issue ACL capture is left to a follow-up
(see issues). Per-tenant isolation (the boundary M2 actually enforces) is
unaffected: the connector only emits what its scoped token can read.

## Deletions (partial — honest about API limits)

Jira's search API **does not return deleted issues**, and re-checking every
known issue key each poll is too heavy for M2. Deletion detection is therefore
**partial** and limited to what the API surfaces:

- If `deleted_statuses` is configured, an issue whose `status.name` matches (case-
  insensitively) is emitted as a **tombstone** (`tombstone.deleted=true`,
  `deleted_at` set, no body). This covers workflows that model deletion as a
  terminal "Removed"/"Cancelled" status.
- True hard-deletes (issues removed from Jira entirely) are **not** detected in
  M2: the documented "recently deleted" JQL is not generally available, so
  incremental sync focuses on created/updated issues. A hard-deleted issue's
  Document remains in the index until a full re-sync or a future enhancement.

This matches exactly what the REST search API allows; the limitation is
recorded here and in the package doc rather than faked.

## Webhooks

`SupportsWebhook = false`. Jira webhooks are deferred for M2;
`HandleWebhook` returns `sdk.ErrWebhookUnsupported` and the hub schedules
polling only (backing the freshness SLA via `IncrementalSync`).

## Tests

`TestContract` (`contract_test.go`) is the M2 exit-criterion contract test: it
drives FullSync, IncrementalSync (a change + a tombstone, boundary issue
deduped), and a stale cursor entirely against committed cassettes under
`testdata/` — no live API. Unit tests cover ADF extraction, the Document
mapping (golden), the cursor round-trip, `Validate`, the project filter, and
deleted-status detection. Package coverage is ~92%.
