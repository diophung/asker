# backfill-event-start

One-time data migration: populate the Vespa `event_start` / `event_end`
attributes on `CALENDAR_EVENT` documents that were indexed **before** those
attributes were added to the schema (v3.2, see `DECISIONS.md` D11).

## When you need it

Symptom: a temporal calendar query ("what's on my calendar next week",
"upcoming meetings") returns **nothing** or only a handful of events, while a
bare "calendar" search returns plenty. Cause: schedule lookups filter on
`event_start` (the occurrence time), and documents indexed before that field
existed have `event_start == 0`, so the window filter excludes them.

Going forward this cannot recur — the index writer
(`services/index-writer`, `eventEpoch`) sets `event_start`/`event_end` on every
calendar event it writes, and re-indexing a connector
(`POST /v1/connectors/{id}/reindex`) re-drives its docs through the pipeline.
This tool is the cheaper alternative to a full re-index: it patches the existing
documents in place with Vespa partial updates and does **not** re-embed or
re-ingest anything.

## Run

```sh
# TENANT_ID is the streaming groupname (the verified OIDC subject).
python3 tools/backfill-event-start/backfill_event_start.py <TENANT_ID>

# Non-default Vespa endpoint / cluster:
python3 tools/backfill-event-start/backfill_event_start.py <TENANT_ID> http://localhost:8082 asker
```

Stdlib only, idempotent, resumable. It prints `DONE seen=… updated=… skipped=…`;
`skipped` counts events whose `metadata_json` had no parseable `start`.

To find a tenant id, list documents in the dev stack or read it from a search
request's verified token subject.
