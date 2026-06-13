# ical — iCalendar feed connector

Indexes a public or tokenized **iCalendar (RFC 5545)** feed URL into Asker as
`CALENDAR_EVENT` documents — the kind of `.ics` feed exported by Google Calendar
("secret address in iCal format"), Outlook published calendars, Apple iCloud
shared calendars, conference schedules, and meetup pages. Spec ID `ical`;
document type `CALENDAR_EVENT`. One configured instance syncs one feed URL.

## Source & fetch

A feed is a single `text/calendar` document fetched by URL. There is **no
incremental API**: every sync GETs the whole feed and the connector diffs it
itself. The fetch uses a plain `net/http` client with a 30s timeout and a 64 MiB
body cap, honoring `ctx` cancellation.

| Purpose          | Call |
|------------------|------|
| Backfill         | `GET <feed_url>` (parse every `VEVENT`) |
| Incremental sync | `GET <feed_url>` (re-fetch, diff against the cursor) |
| Credential check | `GET <feed_url>` (must return `BEGIN:VCALENDAR`) |

## The iCalendar parser

The parser is hand-written in `parse.go` — **no third-party ics library is
vendored**. It implements the slices of RFC 5545 the connector needs:

- **Line unfolding (§3.1):** a CRLF/LF followed by a single space or tab
  continues the previous logical line; the leading whitespace char is removed
  and the lines are joined. Bare LF endings are accepted.
- **Component splitting:** the body is walked as nested `BEGIN:`/`END:` blocks;
  only top-level `VEVENT` components are returned. `VTIMEZONE`, `VTODO`,
  `VALARM`, x-components, etc. are ignored rather than failing the feed.
- **Property parameters (§3.2):** `NAME;PARAM=VALUE;PARAM=VALUE:value`, e.g.
  `DTSTART;TZID=America/Los_Angeles:20260603T093000`. Double-quoted parameter
  values may contain `:` and `;`.
- **TEXT escaping (§3.3.11):** `\\` → `\`, `\n`/`\N` → newline, `\;` → `;`,
  `\,` → `,` in TEXT-typed values (`SUMMARY`, `DESCRIPTION`, `LOCATION`, …).

It is table-tested hard in `parse_test.go` (folding, quoted params, all-day vs
timed, escaping, multiple `VEVENT`s, repeated `ATTENDEE`s).

### Recurrence (deferred)

For M2 a recurring event emits **only the master `VEVENT`**, with its `RRULE`
surfaced verbatim in `metadata["rrule"]`. Expanding an `RRULE`/`RDATE`/`EXDATE`
rule into individual occurrence documents is **deferred** to a later milestone.

## Auth

`AuthNone`. A feed is fetched by URL alone; access control lives in the (often
unguessable) URL itself, so the hub supplies no credential and `Config.Token` is
empty. The feed URL's query string can be a bearer-equivalent secret, so fetch
errors are run through a redactor that strips the query before they reach a
user-facing message or a log line.

## Config schema

```json
{
  "type": "object",
  "properties": { "feed_url": {"type": "string"} },
  "required": ["feed_url"]
}
```

- `feed_url` — the absolute `http(s)` URL of the `.ics` feed (required). In
  dev/CI the contract harness injects the replay-server URL here.

`Validate` parses the config and fetches the feed once, requiring it to contain
`BEGIN:VCALENDAR`.

## Cursor model

A feed is pulled in full every time, so the cursor is a content fingerprint of
the whole feed plus the per-event state needed to diff the next pull. It is a
base64url-wrapped JSON object tagged `ical1:` (opaque to the hub):

| Field         | Meaning |
|---------------|---------|
| `hash`        | sha256 of the previous feed body. A re-fetch that hashes the same is byte-identical: `IncrementalSync` returns the same cursor with no emissions. |
| `max_dtstamp` | the largest `DTSTAMP` across the previous feed's events (freshness marker). |
| `events`      | `UID` → `version_etag` for every event in the previous feed, so the diff can tell new/changed UIDs (upserts) from vanished ones (tombstones). |

`FullSync` emits every event and checkpoints this cursor once (a single-request
source has one resumable position). `IncrementalSync` re-fetches and:

- **unchanged feed** (`hash` matches) → no emissions, same cursor;
- **new or changed event** (UID absent from baseline, or `version_etag` differs)
  → upsert;
- **canceled event** (`STATUS:CANCELLED`) → tombstone;
- **disappeared event** (UID in baseline, absent now) → tombstone.

A cursor this connector did not produce (wrong prefix, bad base64/JSON) surfaces
as `sdk.ErrCursorExpired`, so the hub clears it and reruns `FullSync` instead of
looping on a poison cursor.

## Captured fields

| Document field     | Source |
|--------------------|--------|
| `doc_id`           | `sdk.DocID("ical", "<feed_url>:<UID>")` |
| `source_native_id` | `"<feed_url>:<UID>"` (feed-scoped so two feeds never collide) |
| `type`             | `CALENDAR_EVENT` |
| `title`            | `SUMMARY` (falls back to `(no title)`) |
| `body_text`        | `DESCRIPTION` + `LOCATION` |
| `participants`     | `ORGANIZER` (role `organizer`) then each `ATTENDEE` (role from the `ROLE` param, default `attendee`); `CN=` → name, `mailto:` → email |
| `metadata`         | `uid`, `location`, `dtstart`, `dtend`, `status`, `url`, `sequence`, `rrule` (when recurring), `all_day` (for `VALUE=DATE` events) |
| `ts.created`       | `CREATED` |
| `ts.modified`      | `LAST-MODIFIED` |
| `version_etag`     | `SEQUENCE` (`seq:<n>`) → `LAST-MODIFIED` (`mod:<ts>`) → sha256 of the raw event block (`sha256:<hex>`) — always non-empty, changes iff the event changed |

A `VEVENT` without a `UID` cannot get a stable `doc_id`, so it is skipped (rare
in real feeds).

### Deletions

A `STATUS:CANCELLED` event, or a `UID` that vanished from the feed, is emitted as
a **tombstone**: same `doc_id`, `tombstone.deleted = true`, `deleted_at` set, no
body — so the pipeline removes the indexed document.

## ACLs

`AclInfo` is intentionally left unset. A feed an instance can fetch is the
tenant's own (the URL is the access boundary), so per-tenant isolation is the
entire access-control story — consistent with the other calendar connectors.

## Webhooks

`SupportsWebhook = false`. Feeds are pull-only — there is no push channel a feed
can notify Asker through — so `HandleWebhook` returns `sdk.ErrWebhookUnsupported`
and the hub polls on its freshness schedule.

## Testing

Contract tests run against committed cassette fixtures (`.ics` bodies served by
`connectortest.ReplayServer`) — **no live API, ever** (ADR-011). Because the feed
URL is the dynamic replay-server URL, exact `doc_id` assertions are made in the
`NewReplayServer` + `EmitRecorder` tests where the URL is known;
`RunConnectorContract` pins the emission count and runs `ValidateDocument` on
every emitted document. Fixtures under `testdata/`:

- `fullsync.json` — a 3-event backfill: escaping, folded params, organizer +
  attendees with `ROLE`, an all-day `RRULE` master event.
- `incremental.json` — a changed event + a new event (upserts), a
  `STATUS:CANCELLED` event and a disappeared event (tombstones).
- `incremental_partial.json` — feed body changed but one event's etag is
  unchanged (only the changed event is re-emitted).
- `unchanged.json` — a byte-identical re-fetch (no emissions, same cursor).
- `validate.json` — a valid feed then a non-calendar body.

Run: `go test -race ./connectors/ical/...` (package coverage ~94%).
```
