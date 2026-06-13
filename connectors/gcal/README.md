# gcal — Google Calendar connector

Indexes a tenant's Google Calendar events into Asker. Spec ID `gcal`; document
type `CALENDAR_EVENT`. One configured instance syncs one calendar (default the
connecting account's `primary`).

## Source & API subset

Google Calendar API **v3** (`https://www.googleapis.com/calendar/v3`). The
connector uses a single endpoint, over a plain `net/http` client (no generated
SDK needed for two GETs):

| Purpose            | Call |
|--------------------|------|
| Backfill           | `GET /calendars/{calendarId}/events?singleEvents=true&maxResults=250[&pageToken=...]` |
| Incremental sync   | `GET /calendars/{calendarId}/events?singleEvents=true&syncToken=...` |
| Credential check   | `GET /calendars/{calendarId}/events?singleEvents=true&maxResults=1` |

`singleEvents=true` expands recurring events into individual instances, so each
emitted document is one concrete occurrence (the ingester treats every event as
a single chunk). The final backfill page carries `nextSyncToken`; subsequent
incremental calls pass it back as `syncToken` and Calendar returns only what
changed since.

## Auth

`AuthOAuth2`. The hub runs the OAuth2 flow, refreshes the token, and delivers
the decrypted access token in `Config.Token`. The connector's RoundTripper adds
`Authorization: Bearer <token>` to every request; it never logs or persists the
token. **OAuth scopes are hub/integrator work** — `calendar.readonly` (or
`calendar.events.readonly`) is sufficient for this connector.

## Config schema

```json
{
  "type": "object",
  "properties": {
    "base_url":    {"type": "string"},
    "calendar_id": {"type": "string"}
  }
}
```

- `calendar_id` — the calendar to sync; defaults to `"primary"`.
- `base_url` — API endpoint override. Empty in production (real API); in dev/CI
  the contract harness injects the replay-server URL here.

`Validate` parses the config and, when a token is present, makes one cheap
authenticated `events.list?maxResults=1` round-trip; failures return a
credential-free error.

## Cursor model

Cursors are opaque to the hub; only this connector interprets them. Two shapes:

| Cursor             | Meaning |
|--------------------|---------|
| `sync:<syncToken>` | Steady-state incremental position. `FullSync` returns it (the `nextSyncToken` of the final backfill page); `IncrementalSync` replays `events.list?syncToken=<syncToken>` from it. |
| `page:<pageToken>` | Mid-backfill checkpoint. `FullSync` checkpoints it after every completed page. When the hub replays it into `IncrementalSync`, the backfill resumes at `<pageToken>` and finishes exactly like `FullSync` would, returning a `sync:` cursor. |

`FullSync` paginates all events and `Checkpoint`s after each page, so an
interrupted backfill resumes instead of restarting. `IncrementalSync` follows
`nextPageToken` across a multi-page delta and advances to the new
`nextSyncToken`.

**Expired cursor:** Calendar answers a stale `syncToken` with **410 Gone**
(`fullSyncRequired`). The connector wraps that as `sdk.ErrCursorExpired`; an
unparseable cursor maps to the same sentinel. The hub responds by clearing the
cursor and rerunning `FullSync` — the connector never silently full-syncs
itself.

## Captured fields

| Document field      | Source |
|---------------------|--------|
| `doc_id`            | `sdk.DocID("gcal", "<calendarId>:<eventId>")` |
| `source_native_id`  | `"<calendarId>:<eventId>"` (calendar-scoped so two calendars never collide) |
| `type`              | `CALENDAR_EVENT` |
| `title`             | event `summary` (falls back to `(no title)`) |
| `body_text`         | `description` (HTML stripped to text) + `location` |
| `participants`      | `organizer` (role `organizer`) then each `attendee` (role `attendee`), with name + email |
| `metadata`          | `event_id`, `calendar_id`, `html_link`, `location`, `status`, `start`, `end`, and `response_status:<email-or-name>` per attendee |
| `ts.created`        | event `created` (RFC3339) |
| `ts.modified`       | event `updated` (RFC3339) |
| `version_etag`      | event `etag` (sha256 of the body as a fallback) — always non-empty |

All-day events store the `YYYY-MM-DD` `date` in `start`/`end`; timed events store
the RFC3339 `dateTime`.

### Deletions

An incremental event with `status == "cancelled"` is emitted as a **tombstone**:
same `doc_id`, `tombstone.deleted = true`, `deleted_at` set, no body — so the
pipeline removes the indexed document.

## ACLs

`AclInfo` is intentionally **left unset**. Per ADR-012, calendars are a
single-owner / private-by-construction source: every event a tenant's calendar
connector can read is the tenant's own, so per-tenant isolation is the entire
access-control story. (Shared sources like Drive/Confluence populate `AclInfo`;
calendars do not.)

## Webhooks

`SupportsWebhook = false`. Calendar push channels (`events.watch`) require a
publicly reachable HTTPS callback, which the dev stack and current hub do not
provide, so push is deferred and the hub polls. `HandleWebhook` returns
`sdk.ErrWebhookUnsupported`.

## Testing

Contract tests run against committed cassette fixtures via
`connectortest.RunConnectorContract` / `ReplayServer` — **no live API, ever**
(ADR-011). Fixtures under `testdata/`:

- `fullsync.json` — two-page backfill (3 events), final page carries the sync token.
- `incremental.json` — a changed event + a `cancelled` event (tombstone).
- `incremental_paged.json` — a multi-page incremental delta.
- `resume_backfill.json` — resuming a backfill from a `page:` checkpoint.
- `stale_cursor.json` — a 410 Gone proving `ErrCursorExpired` round-trips.
- `validate.json` — the credential-check round-trip (ok + 401).

Run: `go test -race ./connectors/gcal/...` (package coverage ~90%).
