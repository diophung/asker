# Outlook Calendar connector (`outlook-cal`)

Indexes a user's **Outlook / Microsoft 365 calendar** into Asker as
`CALENDAR_EVENT` documents, via the **Microsoft Graph v1.0 REST API**. It is an
M2 connector built against the public Connector SDK (`connectors/sdk`); the
worked reference connector is `connectors/gmail`.

## The source and the API subset used

- **Base URL:** `https://graph.microsoft.com/v1.0` (overridable via the
  `base_url` config field; dev/CI point it at a cassette replay server).
- **Auth:** OAuth2 (`sdk.AuthOAuth2`). The hub runs the authorization-code flow
  and delivers a refreshed access token in `sdk.Config.Token`; the connector
  adds `Authorization: Bearer <token>` to every request and never logs it.
- **Endpoints:**
  - `GET /me` — one cheap authenticated round-trip in `Validate` to confirm the
    token works (and, when `user_principal_name` is set, that it matches).
  - `GET /me/events?$select=…&$top=50` — the **FullSync** backfill, paged via
    `@odata.nextLink`.
  - `GET /me/calendarView/delta?$select=…&$top=50` — the **delta query**.
    FullSync primes it once (capturing a delta token that covers the backfill
    window); IncrementalSync replays it. Pages chain via `@odata.nextLink`;
    the query terminates in an `@odata.deltaLink` (the next cursor). Deleted
    events appear as items carrying an `@removed` facet. A `410 Gone` (or a
    `400` with error code `resyncRequired` / `SyncStateNotFound`) means the
    delta token is no longer replayable.

Graph continuation links (`@odata.nextLink` / `@odata.deltaLink`) are absolute
URLs pointing at `graph.microsoft.com`; the connector re-hosts them onto the
configured `base_url` (stripping the API-version path prefix) so the same code
follows them against real Graph in production and a replay server in CI.

## Config schema

```json
{
  "type": "object",
  "properties": {
    "base_url":            {"type": "string"},
    "user_principal_name": {"type": "string"}
  }
}
```

- `base_url` (optional) — API endpoint override. Empty in production (defaults
  to real Graph); set by dev/CI to a cassette replay server.
- `user_principal_name` (optional) — the mailbox owner (UPN or mail). When set,
  `Validate` cross-checks it against the `/me` response.

## Cursor model

Cursors are opaque to the hub and parsed only here. Two shapes:

| shape | meaning |
| :--- | :--- |
| `delta:<deltaLink>` | steady-state incremental cursor — the full Graph `@odata.deltaLink` URL. `IncrementalSync` resumes the delta query from it and advances to the new deltaLink. |
| `page:<nextLink>` | mid-backfill checkpoint — the full `@odata.nextLink` URL. `FullSync` checkpoints it after each completed backfill page; replaying it into `IncrementalSync` re-primes a fresh delta token and finishes the backfill at that page. |

`FullSync` primes the delta token **before** paging the backfill, so any event
change racing the backfill is caught by the first incremental pass and converges
via idempotent `(doc_id, version_etag)` upserts. An unparseable cursor, or a
`410`/resync from the delta query, surfaces as `sdk.ErrCursorExpired`; the hub
then restarts a full sync (the connector never silently full-syncs itself).

There is **no webhook path** in M2 (`Spec.SupportsWebhook = false`): Graph
change-notification subscriptions are deferred (see Issues), so `HandleWebhook`
returns `sdk.ErrWebhookUnsupported` and the hub polls.

## Document mapping (Graph event → `askerv1.Document`)

| Document field | Source |
| :--- | :--- |
| `type` | `CALENDAR_EVENT` |
| `doc_id` | `sdk.DocID("outlook-cal", event.id)` |
| `source_native_id` | `event.id` |
| `title` | `event.subject` (`(no subject)` when blank) |
| `body_text` | `event.body.content` (HTML stripped to text when `contentType=html`; `bodyPreview` fallback) + `event.location.displayName` |
| `participants` | `organizer` (role `organizer`) + each `attendee` (role from attendee `type`: `required`/`optional`/`resource`, default `attendee`), with name + email |
| `metadata` | `event_id`, `web_link`, `location`, `start`/`start_timezone`, `end`/`end_timezone`, `is_all_day`, and `attendee_responses` (a JSON object of attendee address → `status.response`) |
| `version_etag` | `@odata.etag` → `changeKey` → `sha256(subject+body)` fallback (always non-empty) |
| `ts.created` / `ts.modified` | `createdDateTime` / `lastModifiedDateTime` (`ts.ingested` left unset — the hub stamps it) |
| `tombstone` | deletions (delta `@removed`) emit `tombstone.deleted=true` + `deleted_at`, same `doc_id`, no body/chunks |

### ACLs

`Document.acl` is **left unset**. A personal Outlook calendar is single-owner /
private-by-construction, so per-tenant isolation is the whole access-control
story (ADR-012 lists calendars among the connectors that do not populate
`AclInfo`).

## Contract tests (ADR-011, the M2 exit criterion)

Hand-authored cassette fixtures under `testdata/` (no live API, ever):

- `fullsync.json` — prime delta + a two-page backfill of 4 events.
- `incremental.json` — a delta with one **changed** event (upsert) and one
  **deleted** event (`@removed` → tombstone).
- `stale_cursor.json` — a `410 resyncRequired`, asserting `ErrCursorExpired`.
- `resume_backfill.json` — a `page:` checkpoint cursor resuming a backfill.

`TestContract` drives all four through `connectortest.RunConnectorContract`,
asserting emitted `doc_id` sets/counts/types, the change+delete, returned
cursors, and `connectortest.ValidateDocument` on every emitted document, plus
`RunSpecChecks`. Unit tests cover Document mapping, cursor parsing, link
re-hosting, the Graph error→`ErrCursorExpired` mapping, and `Validate`.

Run: `go test -race ./connectors/outlook-cal/...`
