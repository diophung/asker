# Microsoft Teams connector (`msteams`)

Indexes a user's Microsoft Teams **chat messages** into Asker via the Microsoft
Graph v1.0 REST API. Built from `docs/connectors/building-a-connector.md` using
only the public Connector SDK (`connectors/sdk`) and the cassette harness
(`connectors/sdk/connectortest`).

| | |
|---|---|
| Spec ID | `msteams` (immutable — baked into every `doc_id`) |
| Auth | OAuth2 (`sdk.AuthOAuth2`); the hub delivers a live access token in `Config.Token` |
| DocType | `CHAT_MESSAGE` |
| Webhook | **unsupported** — Graph change notifications are deferred (as for the other Graph connectors); the hub polls |

## What it reads

- **FullSync** — `GET /me/chats?$expand=members` to list the user's chats
  (following `@odata.nextLink` pages), then `GET /chats/{id}/messages` per chat
  (also paginated). Each live message is emitted as one Document; the connector
  checkpoints after every completed chat. After backfilling a chat it primes
  that chat's `/messages/delta` link so the first incremental poll resumes
  exactly where the backfill stopped.
- **IncrementalSync** — `GET /chats/{id}/messages/delta` per chat from the
  stored `@odata.deltaLink`. A changed message is re-emitted (idempotent upsert
  on `(doc_id, version_etag)`); a message with `deletedDateTime` or an
  `@removed` annotation is emitted as a tombstone. A `410 Gone` on any chat's
  delta link surfaces as `sdk.ErrCursorExpired`, and the hub restarts a full
  sync.

## Document mapping

| Document field | Source |
|---|---|
| `doc_id` | `sdk.DocID("msteams", "<chatId>:<messageId>")` |
| `source_native_id` | `"<chatId>:<messageId>"` (a Teams message id is only unique within its chat) |
| `type` | `CHAT_MESSAGE` |
| `title` | first ~60 chars / first line of the body text, or `"<topic> message"` when empty |
| `body_text` | `body.content`, HTML stripped to plain text when `contentType=html` |
| `participants` | `from.user` (role `from`) + chat `members` (role `member`), author de-duplicated |
| `metadata` | `chat_id`, `message_id`, `web_url`, `chat_type`, `importance` |
| `version_etag` | message `etag`, else `lastModifiedDateTime`, else a content hash |
| `ts.created` / `ts.modified` | `createdDateTime` / `lastModifiedDateTime` (`ts.ingested` is left for the hub) |
| `acl` | group/meeting chats: members are `AllowedPrincipals`, `IsPrivate=true`. One-on-one chats leave `acl` unset (private by construction). ADR-012: captured, not yet enforced. |

## Cursor format

A single opaque `sdk.Cursor` carries a per-chat delta map:

```json
{"deltas":{"<chatId>":"<deltaLink>", ...}}
```

`FullSync` returns the primed map; `IncrementalSync` advances each chat's
`deltaLink`. An undecodable cursor is treated as expired.

## Configuration

```json
{ "base_url": "https://graph.microsoft.com/v1.0" }
```

`base_url` is an optional endpoint override (dev/CI point it at a replay server
or fake). With no config the connector talks to real Graph. The bearer token
identifies the signed-in user, so no other config is required.

## Tests

Contract-tested offline with hand-authored, Graph-shaped cassettes under
`testdata/` — no live API call. `go test ./connectors/msteams/...` covers:

- `TestSpec` / `RunSpecChecks` — Spec invariants.
- `TestContractFullSync` — backfill emits the expected doc-id set and cursor
  (`RunConnectorContract`, `ValidateDocument` on every doc).
- `TestContractIncremental` — one edit (upsert) + one deletion (tombstone).
- `TestContractStaleCursor` — `410 Gone` ⇒ `sdk.ErrCursorExpired`.
- `TestContractWebhookUnsupported` — `HandleWebhook` ⇒ `sdk.ErrWebhookUnsupported`.
- `TestFullSyncThenIncremental` — the cursor `FullSync` returns drives the
  follow-on `IncrementalSync` end-to-end (cursor chaining).
- `TestMessageMapping` and `mapping_test.go` — field-level mapping, ACL,
  participants, title/etag/timestamp fallbacks, OData-link rebasing.

Coverage is ~86% of statements.

## Recording cassettes

The committed cassettes are hand-authored. To re-record against a controllable
Graph endpoint, follow the record-once workflow in
`connectors/sdk/connectortest/README.md` (`ASKER_RECORD=1`) and redact the
`Authorization` header before committing.

## Integrator notes

`New(opts ...sdk.Option-shaped Option)` returns an `sdk.Connector` with a
`WithLogger(*slog.Logger)` option, matching the wave-1 connectors so the hub
registry can wire it uniformly. **Registering `msteams.New()` in the hub
connector registry is integrator work** (outside this connector's owned
directory).
