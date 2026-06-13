# connectortest — connector contract & HTTP record/replay harness

`connectortest` is the test toolkit every Asker connector uses to prove it
upholds the SDK contract **without any live API call in CI** (the M2 exit
criterion, ADR-011). It has two layers:

1. **Document/Spec assertions** — `EmitRecorder`, `ValidateDocument`,
   `RunSpecChecks` (see `connectortest.go`). These check the canonical-`Document`
   invariants and `Spec` sanity.
2. **HTTP record/replay** — `Cassette`, `ReplayServer`/`ReplayTransport`,
   `Recorder`/`RecordingTransport`, and the one-call driver
   `RunConnectorContract`. This document covers that layer.

A connector talks to its source over HTTP. In a contract test the connector is
pointed at a **replay server** backed by a committed **cassette** of recorded
interactions, so the test is hermetic, fast, and deterministic.

---

## Cassette format

A cassette is one human-readable, hand-editable JSON file under the connector's
`testdata/`. It is an ordered list of request→response interactions:

```json
{
  "interactions": [
    {
      "request":  { "method": "GET", "path": "/v1/messages", "query": "page=1" },
      "response": {
        "status": 200,
        "headers": { "Content-Type": ["application/json"] },
        "body": "{\"messages\":[...],\"next_page\":\"2\"}"
      }
    }
  ]
}
```

### Request matcher

| field         | meaning                                                              |
|---------------|---------------------------------------------------------------------|
| `method`      | HTTP method, matched case-insensitively. Empty matches any method.  |
| `path`        | URL path, matched exactly (after collapsing `//` and `/./`, trimming a trailing `/`). |
| `query`       | raw query string (`"a=1&b=2"`), matched as a **set** of key/value pairs — order-independent. A recorded empty query matches only an empty incoming query. |
| `body`        | optional: pins the request body verbatim (disambiguate two POSTs).  |
| `body_sha256` | optional: pins the request body by lowercase-hex SHA-256 digest.    |

A request matches an interaction when method **and** path **and** query all
match, **and** any body pin matches. Interactions with no body pin ignore the
request body (the common case).

### Response

| field         | meaning                                                              |
|---------------|---------------------------------------------------------------------|
| `status`      | HTTP status (defaults to 200).                                      |
| `headers`     | response headers, canonical keys, multi-valued.                     |
| `body`        | UTF-8 response body, stored verbatim so it stays diff-able.         |
| `body_base64` | binary (non-UTF-8) response body, base64-encoded. Exactly one of `body`/`body_base64` is set. |

### Matching & sequencing rule (precise)

- Interactions are tried **front to back**; the first **unplayed** interaction
  whose matcher accepts the request is served, and its play cursor advances.
- Several interactions may share a matcher. They are served **in file order**,
  one per matching request. This expresses call sequences directly:
  - **pagination** — `?page=1` then `?page=2` are two interactions with
    different queries;
  - **a changing response** — the *same* request returning different bodies
    across two calls (e.g. an edit landing between polls) is two interactions
    with an identical matcher, served in order.
- Once every matching interaction for a request is consumed, a further identical
  request is a **replay miss**: the server responds `598` and the test fails in
  cleanup with a list of unmatched requests.

Pin a POST/PUT body only when you must tell two otherwise-identical requests
apart; leaving it unpinned keeps fixtures resilient to harmless reformatting.

---

## Record-once workflow (`ASKER_RECORD=1`)

You never hand-write a large cassette. Record it once against the in-repo fake
upstream (e.g. `tools/fake-gmail`) — or, carefully, a real API — then commit it.
Thereafter the same test replays with zero network calls, which is what CI runs.

```go
func TestRecordBackfill(t *testing.T) {
    // Stand up the fake upstream (or use a real base URL).
    upstream := startFakeServer(t) // your fake's httptest.Server

    // Recorder: replays in CI; with ASKER_RECORD=1 it proxies to `upstream`
    // and writes the cassette on cleanup, redacting credential headers.
    rec := connectortest.NewRecorder(t, upstream.URL, "testdata/backfill.json")

    cfg := buildConfig(rec.URL()) // point base_url at rec.URL()
    _, err := myconnector.New().FullSync(t.Context(), cfg, recorder.Emit)
    if err != nil {
        t.Fatal(err)
    }
}
```

```sh
# Record once (writes testdata/backfill.json), then commit it:
ASKER_RECORD=1 go test -run TestRecordBackfill ./connectors/myconnector/...
git add connectors/myconnector/testdata/backfill.json

# CI / everyone else replays — no ASKER_RECORD, no network:
go test ./connectors/myconnector/...
```

Recording **redacts secrets**: `Authorization`, `X-Api-Key`, cookies, and other
credential headers are replaced with `REDACTED`, and the `Set-Cookie` response
header is redacted. Default capture does not persist request headers at all.
**Always eyeball a freshly recorded cassette before committing it** — it is
plain JSON; delete anything sensitive a source echoes into a body.

For a connector that builds its own `http.Client` and has no base-url override,
use the transport twin `NewRecordingTransport(t, base, path)` and inject the
returned `http.RoundTripper` into the client.

---

## One-call contract test (`RunConnectorContract`)

`RunConnectorContract` is the harness every connector's contract test calls. It
loads a cassette, starts a `ReplayServer`, injects that server's URL into the
config's `base_url`, runs the requested passes, and asserts every emitted
document satisfies `ValidateDocument` plus the expected doc-id sets, counts,
cursors, and errors. A request the cassette does not cover fails the test as a
clear "unmatched request".

### Copy-paste skeleton for a new connector

```go
package myconnector

import (
    "encoding/json"
    "testing"

    "github.com/asker/asker/connectors/sdk"
    "github.com/asker/asker/connectors/sdk/connectortest"
)

func docID(nativeID string) string { return sdk.DocID("myconnector", nativeID) }

func TestContract(t *testing.T) {
    // 0. Spec sanity (do this once per connector).
    connectortest.RunSpecChecks(t, New())

    // 1. Backfill: FullSync emits the seeded documents.
    backfillCursor := sdk.Cursor("cursor-after-backfill")
    connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
        Name:       "backfill",
        Cassette:   "testdata/backfill.json",
        ConfigJSON: json.RawMessage(`{"user_email":"alice@example.com"}`), // base_url is injected
        Token:      []byte("decrypted-token"),
        FullSync: &connectortest.SyncExpectation{
            WantDocIDs: []string{docID("1"), docID("2"), docID("3")},
            WantCursor: &backfillCursor,
        },
    })

    // 2. Incremental: an upsert plus a tombstone since a cursor.
    incCursor := sdk.Cursor("cursor-after-incremental")
    connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
        Name:     "incremental",
        Cassette: "testdata/incremental.json",
        Token:    []byte("decrypted-token"),
        Incremental: &connectortest.IncrementalExpectation{
            FromCursor: backfillCursor,
            SyncExpectation: connectortest.SyncExpectation{
                WantDocIDs:          []string{docID("2")}, // edited
                WantTombstoneDocIDs: []string{docID("1")}, // deleted
                WantCursor:          &incCursor,
            },
        },
    })

    // 3. A stale cursor surfaces as ErrCursorExpired (optional).
    wantErr := sdk.ErrCursorExpired
    connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
        Name:     "stale-cursor",
        Cassette: "testdata/stale_cursor.json",
        Token:    []byte("decrypted-token"),
        Incremental: &connectortest.IncrementalExpectation{
            FromCursor:      "expired",
            SyncExpectation: connectortest.SyncExpectation{WantErr: &wantErr},
        },
    })

    // 4. Webhook (push connectors). Gmail-style notification-only webhooks
    //    emit nothing; set WantCount/ids only if your webhook emits docs.
    connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
        Name:     "webhook",
        Cassette: "testdata/backfill.json", // any cassette; webhook may make no HTTP call
        Token:    []byte("decrypted-token"),
        Webhook: &connectortest.WebhookExpectation{
            Method: "POST",
            URL:    "/webhooks/myconnector/inst-1",
            Header: map[string][]string{"X-Signature": {"..."}},
            Body:   []byte(`{"notification":"..."}`),
            Want:   connectortest.SyncExpectation{WantCount: 0},
        },
    })
}
```

### `ContractCase` knobs

- `Cassette` (required) — path to the fixture under `testdata/`.
- `ConfigJSON` — instance config; the driver injects the replay URL into
  `BaseURLField` (default `"base_url"`). Set `BaseURLField: connectortest.NoBaseURLInject`
  to pass `ConfigJSON` through untouched (for a connector wired via an injected
  `ReplayTransport` instead).
- `Token`, `Tenant`, `InstanceID` — defaults are filled in when empty.
- `FullSync` / `Incremental` / `Webhook` — set the passes you want to assert.
  When both `FullSync` and `Incremental` are set they run against the **same**
  cassette and server, so order your interactions full-sync-first.

### Expectation knobs (`SyncExpectation`)

- `WantDocIDs` / `WantTombstoneDocIDs` — expected `doc_id` sets
  (order-independent). Use `sdk.DocID(connectorID, nativeID)`.
- `WantCount` — exact total emission count (derived from the id lists if unset).
- `WantCursor` — the cursor the pass must return.
- `WantErr` — the pass must return an error satisfying `errors.Is(err, *WantErr)`
  (e.g. `sdk.ErrCursorExpired`); when unset the pass must succeed.

---

## Out-of-process (gRPC plugin) connectors

The same cassettes drive both in-process connectors and out-of-process ones
exercised through the plugin `Client`: the connector under test still issues
HTTP to its source, so a `ReplayServer`/`ReplayTransport` records and replays it
identically regardless of whether the connector runs in-proc or behind the
plugin transport. Point the plugin-hosted connector's `base_url` at the replay
server exactly as above.
```
