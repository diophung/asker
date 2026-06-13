# ADR-011: Connector contract tests run against recorded fixtures (cassettes)

## Status

Accepted (M2).

## Context

The spec forbids live API calls in CI: "connector contract tests against the SDK with recorded
fixtures (no live API calls in CI)" (§4). M2 adds breadth — Outlook, Google Drive, S3, calendars,
Slack, Confluence, Jira, and more — and each connector must have a contract test proving it upholds
the canonical-Document invariants (`connectortest.ValidateDocument`, `RunSpecChecks`) and the
sync semantics (full → cursor → incremental → tombstone). We cannot get there with a hand-built
fake service per source: the M1 Gmail fake (`tools/fake-gmail`, ADR-008) is a few hundred lines
encoding our reading of one API's semantics, and writing one of those for every M2 source does not
scale and would not even reproduce the real API's response quirks.

The two options for "recorded fixtures" are: (a) a stub *server* per connector that the connector
talks to over HTTP (the Gmail approach); or (b) *cassettes* — recorded real HTTP request/response
pairs replayed deterministically by an SDK-provided round-tripper, with no server at all.

## Decision

1. **Cassettes are the M2 default.** A connector's contract test runs the connector against an
   `http.Client` whose transport replays a committed cassette: a file of recorded
   request→response pairs. No network, no server, no credentials at test time. The replay
   transport is provided by the SDK test harness (`connectortest`); a connector author points
   their `http.Client` at it, seeds the `sdk.Config`, and runs `FullSync` / `IncrementalSync` /
   `HandleWebhook`, asserting emitted Documents with `connectortest.ValidateDocument`.

2. **Record-once workflow.** Cassettes are recorded by running the test with `ASKER_RECORD=1`
   against a controllable endpoint — a local fake, a sandbox tenant, or (rarely, by a human with
   credentials) the real API. The harness in record mode performs the live calls and writes the
   cassette. The author then **redacts secrets** (Authorization headers, tokens, cookies, any PII
   that is not the point of the test) and **commits the cassette** alongside the connector. CI
   runs in replay mode (the default, `ASKER_RECORD` unset): it reads the committed cassette and
   never touches the network.

3. **Matching is on method + URL (+ body where it disambiguates).** Replay matches each outgoing
   request to a recorded entry so pagination, incremental cursors, and 404-on-deleted flows are
   reproduced deterministically. A request with no matching entry is a test failure, not a live
   call — replay never falls through to the network.

4. **The fake-server approach (Gmail, ADR-008) remains acceptable.** Both are valid ways to
   satisfy the no-live-API rule. The fake server stays where a connector needs *mutation* during
   the test (seed → edit → delete, driving history records and push notifications) or where the
   e2e/freshness/tombstone stack needs a live-ish backend — things a static cassette cannot do.
   For the breadth connectors, whose contract tests are read-only request/response checks,
   cassettes are simpler and are the default.

## Consequences

- **Deterministic, offline, secret-free CI.** Every connector's contract test runs on every push
  with no egress, no quotas, and no shared test credentials checked into the repo — which is the
  spec's §4 requirement and keeps the "paid API keys for live testing" decision (§6) a human,
  out-of-CI concern.
- **Cassettes drift from reality.** A committed cassette is a snapshot; when a source API changes,
  the connector passes CI and misbehaves against the live API until someone re-records. This is the
  same risk ADR-008 names for the Gmail fake, and is retired the same way: a periodic,
  manually-run live verification (never in CI) by someone holding credentials.
- **Redaction is load-bearing and must be reviewed.** A cassette is committed HTTP traffic; an
  un-redacted token or real user's mailbox content would leak into git history. Record mode must
  redact by default and contract-test review must check it — a cassette is reviewed like any other
  committed artifact carrying potential secrets.
- **One harness, every connector.** Because replay is an `http.RoundTripper` and the assertions are
  the existing `connectortest` helpers, a new connector's contract test is boilerplate over the
  same harness — which is exactly what the "build a connector in under a day" exit criterion
  (ADR/tutorial in `docs/connectors/`) relies on.
- **Dependency, out of scope here:** the cassette record/replay transport and the
  `connectortest.RunConnectorContract` entry point are SDK-harness work owned by the
  `connectors/sdk/connectortest` builder; this ADR records the *decision and workflow* those
  helpers implement. The tutorial in `docs/connectors/building-a-connector.md` documents the
  author-facing API surface they expose.
