# imessage-agent — local macOS iMessage indexer

A **local CLI** that indexes your iMessages into Asker. There is no iMessage
cloud API, so this is not a hub-hosted connector: it runs on **your own Mac**,
reads the Messages SQLite database (`~/Library/Messages/chat.db`), renders each
chat to a text transcript, and uploads it to Asker through the gateway's
authenticated `POST /v1/upload` endpoint, where it becomes a searchable `FILE`
document in your tenant.

```
chat.db ──sqlite3 -json──▶ imsg.Row ──render──▶ transcript ──POST /v1/upload──▶ Asker (FILE doc)
```

## Privacy posture

- **Everything runs locally.** The agent talks only to the Asker gateway URL you
  pass it; nothing else leaves your machine.
- **Read-only.** `chat.db` is opened read-only (`mode=ro&immutable=1`), so the
  agent never writes to your Messages database.
- **Your tenant only.** The agent never chooses a tenant. The gateway derives
  `tenant_id` from your verified OIDC token (ADR-002), so uploads land only in
  your own per-tenant corpus.
- **No body leakage.** Message text is printed **only** under `--dry-run` and is
  **never** logged. Logs carry chat labels, counts, and the returned `doc_id` —
  never message bodies or the bearer token.

## macOS Full Disk Access (required)

macOS guards `~/Library/Messages/chat.db` behind **Full Disk Access**. Grant it
to the program that runs the agent (your terminal app, e.g. Terminal or iTerm):

1. **System Settings → Privacy & Security → Full Disk Access**.
2. Enable your terminal application (add it with **+** if absent).
3. **Quit and reopen** the terminal so the new permission takes effect.

Without this, `sqlite3` cannot open the database and the agent exits non-zero
with a message pointing here.

## Requirements

- macOS with the system **`sqlite3`** binary on `PATH` (ships with macOS). The
  Asker build has no SQLite driver, so the agent shells out to `sqlite3 -json`
  and parses the JSON rows.
- An Asker gateway URL and a valid **OIDC bearer token** (unless `--dry-run`).

## Usage

```sh
# Build
go build -o imessage-agent ./connectors/imessage-agent

# Dry run: render transcripts to stdout, upload nothing (the only mode that
# prints message bodies).
./imessage-agent --dry-run

# Upload new messages to your Asker tenant.
./imessage-agent --gateway-url http://127.0.0.1:8080 --token "$OIDC_TOKEN"

# Re-read everything (ignore the saved high-water mark).
./imessage-agent --gateway-url http://127.0.0.1:8080 --token "$OIDC_TOKEN" --since 0
```

### Flags

| Flag            | Default | Meaning |
|-----------------|---------|---------|
| `--db`          | `~/Library/Messages/chat.db` | Path to the Messages database. |
| `--gateway-url` | _(none)_ | Asker gateway base URL. Required unless `--dry-run`. |
| `--token`       | _(none)_ | OIDC bearer token for the gateway. Required unless `--dry-run`. Never logged. |
| `--state`       | `<user-config>/asker/imessage-agent/state.json` | Incremental high-water state file. |
| `--since`       | `-1` | Override the high-water `ROWID`. `-1` = use the state file; `0` = re-read all messages; any N = read messages with `ROWID > N`. |
| `--dry-run`     | `false` | Print transcripts to stdout and upload nothing. Skips the gateway/token requirement and never advances state. |

## Incremental sync

The agent uses `message.ROWID` (a monotonic primary key) as an incremental
high-water mark. Each successful run persists the largest `ROWID` it uploaded to
the state file; the next run queries only `ROWID > last`. The high-water mark is
advanced **only on a clean pass** — if an upload fails partway, state is left
untouched so the next run retries the unsent chats. `--dry-run` never advances
state.

## The Apple epoch detail

`chat.db`'s `message.date` is **nanoseconds since the Apple/Cocoa epoch
`2001-01-01T00:00:00Z`** (Mac OS X 10.6+). Older databases stored whole seconds
since the same epoch. `imsg.NormalizeDate` accepts either encoding and
`imsg.AppleTimeToTime` converts to a UTC `time.Time`. Forgetting this offset (vs.
the Unix epoch) is the classic iMessage-parsing bug — timestamps land ~31 years
off.

## Document mapping (the contract-tested core)

`internal/imsg` is a pure, side-effect-free package (no DB, no network, no FS)
that maps one `chat.db` row to a canonical Asker `Document` and renders
transcripts. It is unit-tested to **100%** with hand-authored fixture rows and
**no `chat.db` present**.

`imsg.RowToDocument` produces a `CHAT_MESSAGE` document:

| Field | Source |
|-------|--------|
| `doc_id` | `sdk.DocID("imessage", chatGUID + ":" + messageROWID)` |
| `connector_id` / `source_native_id` | `"imessage"` / `chatGUID + ":" + ROWID` |
| `type` | `CHAT_MESSAGE` |
| `title` | `<chat name or GUID>: <message snippet>` |
| `body_text` | the message text |
| `participants` | sender handle (role `from`, `"me"` for outbound) + counterparty (role `to`); `@`-handles map to `email`, others to `handle` |
| `metadata` | `chat`, `chat_guid`, `handle`, `service`, `is_from_me`, `rowid` |
| `version_etag` | `sha256(text + ":" + date)` — changes on edit/re-send, stable otherwise |
| `ts.created` | the converted Apple date (`ts.ingested` left for the hub) |
| `acl` | unset — a personal-device chat is private by construction (ADR-012) |

## M2 scope and the bulk-document follow-up

**Today (M2), the agent uploads chat transcripts as `FILE` documents** via the
existing `/v1/upload` multipart endpoint (one transcript file per chat, with a
`title`). That endpoint is the only document-ingestion API the gateway exposes,
and it produces `FILE` docs.

The richer per-message `CHAT_MESSAGE` mapping in `internal/imsg`
(`RowToDocument`, typed participants, per-message `version_etag` and timestamps)
is **deliberately built and contract-tested now** but not yet wired to an
ingestion path. **Follow-up:** a future bulk-document ingestion API (accepting
canonical `Document` batches) will let the agent emit one `CHAT_MESSAGE` per
message instead of a transcript `FILE`, at which point `RowToDocument` is the
mapping it uses unchanged. This is integrator/platform work, not connector work.

## Testing

- `internal/imsg` — table-driven unit tests for the row→Document mapping
  (golden), Apple-time conversion (including the seconds/nanoseconds encodings
  and zero/negative edge cases), transcript rendering, `is_from_me` handling,
  and empty/null text. **100% coverage, no `chat.db` needed.**
- main package — `parseRows`, the state file (round-trip, atomic overwrite,
  missing/corrupt), the `runSync` orchestration (with fake reader/uploader),
  the gateway uploader (against `httptest`), and flag validation.
- **Live-db integration** (`integration_test.go`) — seeds a minimal `chat.db`
  with the real Messages schema subset via `sqlite3` and exercises the
  sqlite3-shelling reader and the full `realMain` happy path against an
  `httptest` gateway. These tests **skip** automatically when `sqlite3` is
  absent.

A run against your **real** `chat.db` is necessarily manual (it requires Full
Disk Access and your own messages): use `--dry-run` to inspect what would be
uploaded before sending anything.
