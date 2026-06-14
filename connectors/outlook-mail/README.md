# Outlook Mail connector (`outlook-mail`)

Indexes a Microsoft 365 user's mailbox into Asker via the **Microsoft Graph v1.0** REST API.
One Graph message → one canonical `Document` of type `EMAIL`.

- **Source:** Microsoft 365 / Exchange Online mailbox, owned by the connecting account (`/me`).
- **Auth:** OAuth2 (`sdk.AuthOAuth2`). The hub runs the authorization-code flow and delivers a
  refreshed bearer access token in `sdk.Config.Token`; the connector adds it as
  `Authorization: Bearer <token>` on every request and never logs or persists it.
- **Transport:** plain `net/http` against the Graph REST API (no vendored Graph SDK in the
  module). A 30s per-request timeout guards a hung socket; the hub owns overall sync budgeting.

## Graph API subset used

| Purpose | Request |
| :--- | :--- |
| Backfill (FullSync) | `GET /me/messages?$select=<fields>&$top=50`, following `@odata.nextLink` until absent |
| Cursor init (FullSync tail) | `GET /me/messages/delta?$select=<fields>&$deltatoken=latest` → `@odata.deltaLink` |
| Incremental poll | `GET /me/messages/delta?$select=<fields>&$deltatoken=<token>`, following `@odata.nextLink` to the final `@odata.deltaLink` |
| Credential check (Validate) | `GET /me/messages?$select=id&$top=1` |

`$select` requests only the fields the mapping consumes:
`id, subject, body, bodyPreview, from, toRecipients, ccRecipients, webLink, parentFolderId,
conversationId, createdDateTime, lastModifiedDateTime`.

Absolute `@odata.nextLink` / `@odata.deltaLink` URLs returned by Graph are **rebased onto the
configured `base_url`** (host + API-version prefix rewritten) so every request hits the configured
endpoint — the replay server in CI, a fake in dev, real Graph in production.

## Configuration schema

```json
{
  "type": "object",
  "properties": {
    "base_url":            {"type": "string"},
    "user_principal_name": {"type": "string"}
  }
}
```

- `base_url` — API endpoint override. Empty → `https://graph.microsoft.com/v1.0` (real Graph);
  dev/CI point it at a replay server / fake. Must be a valid `http(s)` URL when set.
- `user_principal_name` — optional; the mailbox owner, retained for diagnostics. The token, not
  this field, selects the mailbox (`/me`).

## Cursor model

Cursors are opaque to the hub and interpreted only here. Two shapes:

- **`delta:<deltatoken>`** — the steady-state incremental cursor. The `<deltatoken>` is the
  `$deltatoken` extracted from Graph's `@odata.deltaLink`, stored **portably** (independent of
  `base_url`) so a later poll rebuilds the delta URL against whatever endpoint is configured.
  `IncrementalSync` walks `/me/messages/delta` from it and returns the next `delta:<token>`.
- **a `/me/messages?...$skip=...` list-page URL** — a mid-backfill checkpoint. `FullSync`
  checkpoints each page's (rebased) `@odata.nextLink`; if the hub replays such a checkpoint into
  `IncrementalSync`, the connector resumes the backfill there and finishes exactly like an
  uninterrupted run, then establishes the `delta:` cursor. Convergence for any change that races
  the backfill is via idempotent `(doc_id, version_etag)` upserts.

A 410 Gone on a stale `$deltatoken`, an empty cursor, or any cursor this connector did not produce
returns (wrapped) `sdk.ErrCursorExpired`; the hub responds by restarting `FullSync`.

## Fields captured

| Document field | Source |
| :--- | :--- |
| `doc_id` | `sdk.DocID("outlook-mail", message.id)` |
| `type` | `EMAIL` |
| `title` | `subject` (`(no subject)` when empty) |
| `body_text` | `body.content` — HTML stripped to text when `body.contentType == "html"`; raw text otherwise; `bodyPreview` when no body part is present |
| `participants` | `from` (role `sender`), `toRecipients` + `ccRecipients` (role `recipient`), each `name` + `email` |
| `metadata` | `message_id`, `conversation_id`, `web_link`, `folder` (= `parentFolderId`) |
| `ts.created` / `ts.modified` | `createdDateTime` / `lastModifiedDateTime` |
| `version_etag` | `@odata.etag` (then `changeKey`, then `sha256(body)`) — changes iff the message changes |
| `tombstone` | a delta `@removed` item → `Document` with `tombstone.deleted=true`, `deleted_at` set, no body |

### ACLs

A personal mailbox is **single-owner / private by construction**, so `Document.acl` is left unset
(ADR-012: shared sources such as Drive/Confluence/shared channels populate `AclInfo`; single-owner
sources do not). No re-sync is needed if ACL enforcement later treats unset ACL as
private-to-tenant.

## Webhooks

`SupportsWebhook = false`. Microsoft Graph change notifications require a public `https`
notification endpoint plus a subscription lifecycle (create, renew before the short expiry,
validate the `clientState` and the `validationToken` handshake), which is deferred to a later
milestone. The hub schedules polling only; `HandleWebhook` returns `sdk.ErrWebhookUnsupported`.

## Tests

`go test -race ./connectors/outlook-mail/...` — hermetic, no live API. Contract test
(`contract_test.go`) replays committed cassettes under `testdata/` via
`connectortest.RunConnectorContract`:

- `fullsync.json` — two `/me/messages` pages (3 messages) + the `$deltatoken=latest` init.
- `incremental.json` — a two-page delta with one edit (upsert, new etag) and one `@removed`
  deletion (tombstone).
- `stale_cursor.json` — a 410 Gone proving the `sdk.ErrCursorExpired` round-trip.
