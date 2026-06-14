# Slack connector

Indexes a Slack workspace's channel and DM messages as canonical Asker
`Document`s (`DocType_CHAT_MESSAGE`). Backfill and incremental sync read the
Slack **Web API**; new and edited messages and deletions are also delivered in
near-real time over the Slack **Events API** (`HandleWebhook`).

It implements the public `sdk.Connector` interface (`connectors/sdk`) and is
the reference for an Events-API-plus-backfill source. The hub owns OAuth, token
refresh/vault, scheduling, retries, rate limits, and Kafka; this connector only
reads Slack and emits Documents.

## Source & API subset

Base URL: `https://slack.com/api` (overridable via config `base_url`; contract
tests point it at a replay server). Every request carries
`Authorization: Bearer <token>`, where the token is the decrypted bot/user
OAuth token (`xoxb-…`/`xoxp-…`) the hub supplies in `sdk.Config.Token`.

| Method | Web API call | Use |
| :--- | :--- | :--- |
| `Validate` | `GET auth.test` | one cheap authed round-trip to confirm the token works |
| `FullSync` | `GET conversations.list` then per channel `GET conversations.history` | backfill every visible channel/DM, paginated |
| `IncrementalSync` | `GET conversations.list` then per channel `GET conversations.history?oldest=<ts>&inclusive=false` | poll each channel for messages newer than its watermark |
| `HandleWebhook` | Events API callback (no outbound call) | push: create/edit upserts, delete tombstones |

Pagination follows `response_metadata.next_cursor` on both list and history
calls. Every response is the standard Web API envelope (`{"ok":…,"error":…}`);
an `"ok":false` body is surfaced as an error carrying the Slack reason.

System/system-subtype messages (`channel_join`, `channel_leave`,
`channel_topic`, …) are filtered out; plain messages, bot messages, thread
broadcasts, `me_message`, and `file_share` are indexed.

## Config schema

```json
{
  "type": "object",
  "properties": {
    "base_url":       {"type": "string"},
    "signing_secret": {"type": "string"}
  }
}
```

- `base_url` — API endpoint override. Empty ⇒ real Slack. Dev/CI point it at
  the contract replay server.
- `signing_secret` — the Slack app **signing secret**, used to verify Events
  API callbacks (`X-Slack-Signature` HMAC). Required for `HandleWebhook`; the
  hub injects it for webhook-enabled instances.

`AuthType` is `AuthOAuth2`. `SupportsWebhook` is `true`.

## Cursor model

Slack has no global change feed, so each conversation is paginated and read
independently. The cursor is a deterministic JSON object mapping **channel id →
the latest message `ts` emitted for that channel** (Slack `ts` is a
`"seconds.micros"` string, lexicographically chronological):

```json
{"C100":"1700000300.000300","C200":"1700000050.000050"}
```

- `FullSync` builds this map from the newest `ts` seen per channel and
  checkpoints the accumulating map after **each channel** (resumable backfill).
- `IncrementalSync` lists channels (picking up any created since the last sync),
  then calls `conversations.history` per channel with `oldest=<watermark>` and
  `inclusive=false` to fetch only newer messages, advancing each channel's
  watermark.
- A cursor that is not this JSON-object shape (e.g. a foreign/corrupt cursor),
  or a Slack `invalid_cursor` from `conversations.history`, is returned wrapped
  as `sdk.ErrCursorExpired`, so the hub restarts a full sync instead of looping.

## Webhook (Events API)

`HandleWebhook` verifies the request came from Slack — the `X-Slack-Signature`
HMAC-SHA256 over `v0:<X-Slack-Request-Timestamp>:<raw-body>` keyed by the
signing secret, with a 5-minute freshness window against replay — then:

- `url_verification` — acknowledged with no emission. The **hub/gateway** must
  echo the `challenge` in the HTTP response; `HandleWebhook` can only emit
  documents, not write a response body, so the challenge handshake is the hub's
  job (documented here, not handled in this connector).
- `event_callback` with inner `message`:
  - plain message / `bot_message` / `file_share` / `thread_broadcast` /
    `me_message` → upsert Document;
  - `message_changed` → upsert the same `doc_id` with a new `version_etag`
    (from `edited.ts`);
  - `message_deleted` → tombstone for the deleted `ts`.
- other event/subtype kinds are ignored.

## Captured fields

Per message Document:

| Field | Source |
| :--- | :--- |
| `doc_id` | `sdk.DocID("slack", "<channel>:<ts>")` |
| `source_native_id` | `<channel>:<ts>` |
| `type` | `CHAT_MESSAGE` |
| `title` | first ≤80 runes of text (single line), or `<#channel> message` |
| `body_text` | the message text (raw; `<@U…>`/`<#C…>` mentions left as-is) |
| `participants` | the author as `{handle: <user/bot id>, role: "from"}` (no `users.info` lookup) |
| `metadata` | `channel`, `channel_name`, `ts`, `thread_ts`, `team`, `user`, `permalink` |
| `ts.created` | message `ts`; `ts.modified` set from `edited.ts` on edits |
| `version_etag` | `edited.ts` when edited, else `ts` (changes on every edit) |
| `acl` | see below |

### ACL capture (ADR-012)

Channels are shared sources, so their Documents carry `AclInfo`:

- public/private channels → `allowed_principals` = channel member ids,
  `is_private` = the channel's privacy flag;
- 1:1 DMs (`is_im`) are private-by-construction and **leave `acl` unset**.

ACLs are captured but not enforced at query time in M2 (ADR-012); per-tenant
isolation remains the enforced boundary.

## Tests

`go test ./connectors/slack/...` runs entirely against committed cassettes under
`testdata/` (`fullsync.json`, `incremental.json`, `stale_cursor.json`,
`validate.json`) and signed in-memory webhook requests — **no live Slack API**
(ADR-011). `TestContract` asserts FullSync doc-id sets and cursor, the
incremental change + cursor advance, the `ErrCursorExpired` round-trip, and the
webhook create/delete emissions; `ValidateDocument` and `RunSpecChecks` run over
everything emitted.

## Mentions & name resolution (not done)

`<@U…>`/`<#C…>` mention tokens in `body_text` are left raw and user ids are not
resolved to display names (no `users.info` round-trip per author). Resolving
mentions/names is a future enrichment; the ids are searchable as participant
handles and in metadata.
