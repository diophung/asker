# WhatsApp Export connector

Imports a WhatsApp chat exported with **"Export chat"** (the `_chat.txt` file) as
canonical Asker `Document`s (`DocType_CHAT_MESSAGE`). One logical message becomes
one document, in file order.

It implements the public `sdk.Connector` interface (`connectors/sdk`). The hub
owns blob storage, scheduling, retries, rate limits, and Kafka; this connector
only fetches one text file and emits Documents. `Spec.ID` is `whatsapp-export`
(immutable — it is the prefix in every `doc_id`).

## Source & auth

`AuthType: AuthNone`. There is no API and no credential. WhatsApp has no export
API: the user shares an exported `_chat.txt`, the hub stores it in blob storage,
and supplies its URL in the instance config (`export_url`). Each sync is a single
HTTP `GET export_url`. Contract tests point the URL at a replay server.

## Config schema

```json
{
  "type": "object",
  "properties": {
    "export_url": { "type": "string" },
    "chat_name":  { "type": "string" }
  }
}
```

- `export_url` — where the uploaded `_chat.txt` is fetched from (http/https).
  Required at sync time; `Validate` does **not** require it (an instance may be
  created before the file is uploaded). It must be a well-formed http(s) URL when
  present.
- `chat_name` — optional display name. Used in titles, in metadata, and as the
  doc-id scope so two chats imported into the same tenant never collide.

> The contract harness injects the replay server URL into the `base_url` field by
> default. The connector reads `export_url` first and falls back to `base_url`, so
> a contract test sets `BaseURLField: "export_url"` to inject directly into
> `export_url`. Either field works.

## Parser (the core)

`parseExport` (`parser.go`) is line-oriented. A line whose leading token parses
as a **timestamp header** starts a new message; every other line is a
**continuation** appended to the current message's text (so multi-line messages
survive). A file's single trailing newline is treated as a terminator, not a
blank continuation line.

Two header families are recognized, in 12h (AM/PM) and 24h variants, with or
without seconds:

| Family    | Example                                       |
| :-------- | :-------------------------------------------- |
| bracket   | `[2024-03-15, 9:42:13 AM] Alice: message text` |
| dash      | `3/15/24, 09:42 - Alice: message text`         |

Robustness details the parser handles (all table-tested in `parser_test.go`):

- **Multi-line messages** — continuation lines join with `\n`.
- **CRLF** line endings are normalized to `\n`.
- **Emoji / Unicode** — passed through verbatim (sanitized to valid UTF-8 only
  for the proto string requirement).
- **Attachments** — `<Media omitted>` is indexed as its literal text (M2 has no
  media extraction; the placeholder stays searchable).
- **Invisible marks** — the bidi control marks WhatsApp wraps around timestamps
  (LRM `U+200E`, RLM `U+200F`, the isolates) are stripped before matching, and a
  narrow no-break space (`U+202F`) or no-break space (`U+00A0`) inside a timestamp
  is normalized so the time still parses.
- **Colon / dash in the body** — a `: ` or ` - ` *inside* a message body does not
  start a new message; only a line whose leading token parses as a real timestamp
  is a header. The dash header splits on the **first** ` - ` only.
- **Unknown date locale** — a header whose date format is not recognized is not
  treated as a header (it becomes a continuation line); leading unrecognized text
  before the first real header is dropped.

### System lines — emitted, not skipped

A timestamped line with **no `Name: ` sender segment** is a system notification
(`"Messages and calls are end-to-end encrypted"`, `"Alice added Bob"`,
`"Alice created group …"`). **Choice: we emit these** as participant-less
`CHAT_MESSAGE` documents (no `participants`, `metadata.sender = "system"`) rather
than skipping them, so a search for "end-to-end encrypted" or "added Bob" still
hits. The body is the system phrase verbatim.

## Document mapping

| Field             | Value |
| :---------------- | :---- |
| `type`            | `CHAT_MESSAGE` |
| `connector_id`    | `whatsapp-export` |
| `source_native_id`| `<chat_name>:<line_index>:<sha256(text)[:8]>` |
| `doc_id`          | `sdk.DocID("whatsapp-export", source_native_id)` |
| `title`           | first ~60 chars of the text (newlines collapsed); `"<chat_name> message"` when the body is empty |
| `body_text`       | the message text (continuation lines joined with `\n`) |
| `participants`    | the sender, role `"sender"` (name and handle both the export's display name); none for system lines |
| `metadata`        | `chat_name`, `sender` (or `"system"`), `sent_at` (raw header timestamp), `line_index`, `msg_format` (`bracket`/`dash`) |
| `ts.created`      | the parsed message timestamp (omitted if the locale could not be parsed) |
| `version_etag`    | `sha256(sender + "\0" + raw_timestamp + "\0" + text)` — changes iff the message changes |

`ts.ingested` is never set (the hub stamps it).

### doc_id scheme — stable under re-import

The native id binds `chat_name` + `line_index` + a short content digest. WhatsApp
"Export chat" is **append-only** in normal use: a later, larger export of the
same chat reproduces every earlier message at the same line index with the same
text, so each already-seen message re-derives the **same** `doc_id`, making
re-import upserts idempotent and emitting only the new tail. The content digest
keeps two different messages at the same index (across distinct chats, or after a
prefix change) distinct. An edit/delete that shifts earlier line indices changes
the prefix and triggers a full re-import (see cursor), which converges via the
`(doc_id, version_etag)` merge.

There are **no tombstones**: an export is a point-in-time snapshot with no delete
signal, and a prefix change is handled as a full re-import rather than as
per-message deletions.

## Cursor & incremental import

A cursor is `"<count>:<sha256 of the first count messages>"`:

- **`FullSync`** fetches `export_url`, parses, emits every message in order, and
  returns the cursor pinned to the full count.
- **`IncrementalSync`** re-fetches and compares against the cursor:
  - unchanged (same count, same prefix hash) → emit nothing, return the same
    cursor (no-op);
  - grown with an unchanged prefix → emit only the new tail and advance;
  - the prefix changed, or the export shrank, or the cursor is unparseable →
    `sdk.ErrCursorExpired` so the hub clears the cursor and re-runs `FullSync`.

`SupportsWebhook` is `false`: an uploaded file has no push channel; the hub polls.

## Contract test

`whatsapp_test.go` runs `connectortest.RunSpecChecks` and
`connectortest.RunConnectorContract` against committed cassettes in `testdata/`
(no live API, ADR-011): a bracket-format FullSync, an append-only incremental, an
incremental no-op, a changed-prefix `ErrCursorExpired`, an unparseable-cursor
`ErrCursorExpired`, and a dash-format FullSync. `parser_test.go`,
`cursor_test.go`, and `document_test.go` table-test the parser, cursor logic, and
Document mapping. Coverage is ~91%.

## Hub registry wiring (integrator work)

Registering the connector in the hub is **out of this directory's scope**. An
integrator adds it to `services/connector-hub/registry.go` alongside the other
connectors, e.g.:

```go
whatsappexport.New(whatsappexport.WithLogger(logger)),
```
