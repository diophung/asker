# ADR-008: Fake Gmail service for dev and CI

## Status

Accepted (M1).

## Context

M1's flagship connector is Gmail: full sync, incremental sync via `history.list`, and push via
`watch` + Pub/Sub. The spec forbids live API calls in CI (§4: contract tests run against
recorded fixtures) — and there are no Google OAuth credentials in this project yet anyway;
obtaining them is exactly the "paid API keys for live connector testing" decision the spec
reserves for the human (§6). The M1 exit criteria, though, demand end-to-end behavior a static
fixture file cannot drive: 10K synthetic emails across 3 tenants, source edits searchable
within 30 minutes, deletes propagating as tombstones. That requires a live-ish Gmail that the
e2e stack can seed and mutate.

## Decision

Dev and CI run **`services/fake-gmail`** (HTTP :9400, compose dev/CI profile only), a local
fake implementing the subset of the **Gmail REST v1 API** the connector uses:

- `users.messages.list` / `users.messages.get` (full sync),
- `users.history.list` (incremental sync from a `historyId` cursor),
- `users.watch` plus a push shim that POSTs Pub/Sub-shaped notifications to the connector
  hub's webhook endpoint (standing in for Google Cloud Pub/Sub),
- an **admin API** (seed / edit / delete messages) so e2e tests can create corpora and mutate
  them — driving history records and push notifications exactly as the real service would.

The Gmail connector itself is written against **`google.golang.org/api/gmail/v1`** with the
client's endpoint overridden (`option.WithEndpoint`) to point at the fake in dev/CI and at
Google in prod. One connector code path; only the base URL and credentials differ.

Connector contract tests use recorded fixtures via the SDK's `connectortest` harness; the fake
serves the e2e/freshness/tombstone flows. **Live-Gmail testing is deferred** until real OAuth
credentials exist, and will be a manually-run (never CI) verification.

## Consequences

- CI is hermetic and fast: no network egress, no quotas, no secrets, deterministic corpora.
  The M1 exit criteria (10K emails / 3 tenants, <30-min edit freshness, tombstones) are
  testable on every push.
- The fake is its own small service to maintain, and it encodes our *reading* of Gmail's
  semantics — `historyId` ordering, pagination, 404-on-deleted-message, watch expiry. Where
  that reading is wrong, the connector passes CI and misbehaves against real Gmail. This risk
  is bounded by building the fake from Google's documented behavior and is only fully
  retired by the deferred live verification.
- Endpoint override keeps prod code untouched by test concerns, but means the OAuth flow
  itself (consent, token refresh against real Google) is unexercised until credentials exist;
  the hub's token-vault path is tested with the fake accepting any bearer token.
- The push shim stands in for Pub/Sub, so prod Pub/Sub subscription plumbing (auth,
  retry/redelivery quirks) is also deferred to the live verification.
- The fake's admin API doubles as the M5 synthetic-data generator's Gmail backend — seeding
  at scale is already a supported operation.
