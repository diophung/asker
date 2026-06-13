# Building a connector

This is the "build a connector in under a day" tutorial. It walks through implementing a connector
against the **public Connector SDK** (`connectors/sdk`) — the same five-method interface the
first-party connectors use — and writing its cassette-based contract test. The worked reference is
the Gmail connector (`connectors/gmail`); this tutorial points at it rather than duplicating it.

By the end you will have a connector that the hub can register, sync, and test in CI with no live
API calls. A sibling builder is expected to implement **MS Teams** from this document using only
the public SDK, so every step here is concrete.

> **Scope.** A connector *reads one source and emits canonical Documents.* It does **not** touch
> Kafka, Vespa, blob storage, OAuth flows, token refresh, scheduling, retries, or rate limits —
> the hub owns all of that (`connectors/sdk/doc.go`, "Division of labor with the hub"). You write
> source-reading + Document-mapping, nothing else.

---

## 0. The interface you implement

`sdk.Connector` (`connectors/sdk/connector.go`):

```go
type Connector interface {
    Spec() Spec
    Validate(ctx context.Context, cfg Config) error
    FullSync(ctx context.Context, cfg Config, emit Emit) (Cursor, error)
    IncrementalSync(ctx context.Context, cfg Config, cur Cursor, emit Emit) (Cursor, error)
    HandleWebhook(ctx context.Context, cfg Config, r *http.Request, emit Emit) error
}
```

| Method | When the hub calls it | What it does |
| :--- | :--- | :--- |
| `Spec` | startup / UI | static description: ID, display name, auth type, config JSONSchema, webhook support. Must be cheap, pure, identical every call. |
| `Validate` | instance create / reconfigure | check config parses and the token reaches the source — emit nothing. Error text is shown to the user verbatim, so never leak the token. |
| `FullSync` | first sync | backfill everything; checkpoint progress; return the cursor incremental sync continues from. |
| `IncrementalSync` | every poll | emit everything changed/deleted since `cur`; return the advanced cursor. |
| `HandleWebhook` | push delivery | verify a source HTTP request and emit affected docs; return `ErrWebhookUnsupported` if there is no push path. |

The same interface runs **in-process** (the default; registered in `sdk.Registry`) and
**out-of-process** as a gRPC plugin (ADR-010). Write to the arguments you are given — never assume
shared memory with the hub — and your connector carries forward to either transport unchanged.

This tutorial uses a running example source called **memo** (a hypothetical chat/notes API) to keep
the snippets generic; substitute your source.

---

## 1. `Spec` — declare yourself

```go
const connectorID = "memo" // baked into every doc_id forever; NEVER change it.

const configSchema = `{
  "type": "object",
  "properties": {
    "base_url":   {"type": "string"},
    "workspace":  {"type": "string"}
  },
  "required": ["workspace"]
}`

func (c *Connector) Spec() sdk.Spec {
    return sdk.Spec{
        ID:              connectorID,            // ^[a-z0-9-]+$ — no ":", no underscores, lowercase
        DisplayName:     "Memo",
        AuthType:        sdk.AuthOAuth2,         // AuthNone | AuthOAuth2 | AuthToken
        ConfigSchema:    json.RawMessage(configSchema),
        SupportsWebhook: true,                   // false ⇒ hub polls only, never calls HandleWebhook
    }
}
```

Rules `connectortest.RunSpecChecks` enforces:

- **`ID` matches `^[a-z0-9-]+$`** and is **immutable**. It is the prefix in every `doc_id`
  (`sdk.DocID`), so changing it orphans every previously indexed document. The `[a-z0-9-]` rule
  guarantees `ID` can never contain the `:` DocID separator.
- **`DisplayName` is non-empty.**
- **`ConfigSchema` is valid JSON.** A connector with no config publishes `{"type":"object"}`, not
  `nil`.
- **`AuthType` is one of the three constants.** It tells the hub which credential flow to run; the
  result arrives in `Config.Token`.

### Auth types

- `sdk.AuthNone` — no credential (public iCal, direct upload). `Config.Token` is empty.
- `sdk.AuthOAuth2` — the hub runs the authorization-code flow and delivers a refreshed access token
  in `Config.Token`. (MS Teams: this one — Microsoft Graph OAuth2.)
- `sdk.AuthToken` — the user supplies a static secret (API key, PAT) in `Config.Token`.

You never see a refresh token, a client secret, or the vault — only the live, scoped token for the
instance you are syncing (ADR-010). Do not log or persist it.

---

## 2. `Config` — what the hub hands you

```go
type Config struct {
    Tenant     tenancy.Context // the tenant this instance syncs for — ALWAYS use this for tenant_id
    InstanceID string          // a tenant may connect the same source twice (two accounts)
    ConfigJSON []byte          // valid against Spec.ConfigSchema
    Token      []byte          // decrypted credential; empty for AuthNone
    Checkpoint Checkpoint      // never nil at runtime; persists intermediate cursors
}
```

Parse `ConfigJSON` once into a typed struct and validate it (see Gmail's `parseConfig`). Get the
tenant string **only** from `cfg.Tenant.TenantID()` — never from `ConfigJSON`, a token claim, or
anything the source returns. The hub derived `cfg.Tenant` from a verified identity (ADR-002); a
connector that puts any other value in `Document.tenant_id` breaks isolation and the contract test
fails immediately.

---

## 3. The canonical Document mapping (the core of the job)

Every method emits `*askerv1.Document` (`platform/proto/gen/go/asker/v1`). Map one source record →
one Document. **Worked example:** `connectors/gmail/message.go` (`messageDocument`,
`tombstoneDocument`).

```go
doc := &askerv1.Document{
    TenantId:       string(cfg.Tenant.TenantID()),     // == cfg.Tenant.TenantID(), never anything else
    ConnectorId:    connectorID,                        // "memo"
    SourceNativeId: rec.ID,                             // the source's own id for this record
    DocId:          sdk.DocID(connectorID, rec.ID),     // THE doc_id rule — see below
    Type:           askerv1.DocType_CHAT_MESSAGE,       // pick the DocType for your source (see below)
    Title:          rec.Title,
    BodyText:       rec.Text,                           // plain searchable text; you extract it
    Participants:   participants(rec),                  // typed people facet — see below
    VersionEtag:    rec.ETag,                           // see below
    Ts:             &askerv1.Timestamps{Created: timestamppb.New(rec.CreatedAt)},
    Acl:            acl(rec),                            // shared sources only — see §3.5
}
if err := emit(ctx, doc); err != nil {
    return "", err // an Emit error is TERMINAL for this pass — return it
}
```

The invariants below are checked by `connectortest.ValidateDocument`; a violation is a test failure.

Pick `Type` from the `askerv1.DocType` enum that fits your source:
`DocType_EMAIL`, `DocType_CHAT_MESSAGE`, `DocType_FILE`, `DocType_CALENDAR_EVENT`,
`DocType_WIKI_PAGE`, `DocType_TICKET`, `DocType_IMAGE`, `DocType_AUDIO`, `DocType_VIDEO`
(`platform/proto/gen/go/asker/v1`). For MS Teams messages use `DocType_CHAT_MESSAGE`.

### 3.1 `DocID` — the doc_id rule

```go
DocId: sdk.DocID(connectorID, sourceNativeID)
```

`sdk.DocID` is the canonical, stable document ID: `sha256(connectorID + ":" + sourceNativeID)` in
lowercase hex (`connectors/sdk/docid.go`, spec §2.4). **Always use it.** It is what makes re-syncs
and deletes converge: the same source record always produces the same `doc_id`, so upserts and
tombstones land on the same indexed document. Both `ConnectorId` and `SourceNativeId` must be set,
and `DocId` must equal `sdk.DocID(ConnectorId, SourceNativeId)`.

### 3.2 `version_etag` — idempotent upserts

`version_etag` must be **set** and must **change whenever the document's content changes**.
Downstream upserts are idempotent on `(doc_id, version_etag)`: a re-emit with the same etag is a
no-op, a re-emit with a newer etag replaces. Use the source's own version/etag/revision if it has
one; otherwise hash the content. Gmail uses the message's `historyId`, falling back to
`sha256(body)` when none is available (`connectors/gmail/message.go`).

### 3.3 `ts` — timestamps you set vs. the hub sets

- Set `ts.created` / `ts.modified` from the **source** when known.
- **Leave `ts.ingested` UNSET.** The hub stamps ingestion time when it routes the Document to Kafka.
  Setting it yourself is a contract violation.

### 3.4 `participants` — the people facet

`repeated Participant{name, email, handle, role}` powers typed search (`from:alice`). Parse the
source's people into participants with a `role` ("from"/"to"/"author"/"member"/...). Gmail parses
From/To/Cc via `net/mail` and degrades a malformed header to a single name-only participant so it
stays searchable (`connectors/gmail/message.go`, `participants`). For MS Teams, map the message
sender and channel/chat members.

### 3.5 `acl` — shared sources only (ADR-012)

For **shared** sources (Drive, Confluence, Teams channels with restricted membership) set
`Document.acl` = `AclInfo{AllowedPrincipals, IsPrivate}`: the principals the source says may see the
document, and whether it is private to the connecting account. M2 **captures** this; it is not yet
enforced at query time (ADR-012), but populate it so enforcement needs no re-sync. Single-owner /
private-by-construction sources (a 1:1 chat, a personal mailbox) leave `acl` unset.

### 3.6 Tombstones — deletions are documents too

A deletion is emitted as a Document with the **same `doc_id`**, `tombstone.deleted = true`, and
**no body** — empty `body_text`, no `chunks`. Identity fields and a `version_etag` newer than any
prior upsert (so the delete wins the idempotent merge) still required. See
`tombstoneDocument` in `connectors/gmail/message.go`:

```go
return &askerv1.Document{
    TenantId:       string(cfg.Tenant.TenantID()),
    ConnectorId:    connectorID,
    SourceNativeId: id,
    DocId:          sdk.DocID(connectorID, id),
    Type:           askerv1.DocType_CHAT_MESSAGE,
    VersionEtag:    newerEtag,                 // newer than the last live version
    Tombstone:      &askerv1.Tombstone{Deleted: true, DeletedAt: timestamppb.Now()},
}
```

---

## 4. `Validate`

Parse and sanity-check `ConfigJSON`, then — if `cfg.Token` is non-empty — make one cheap
authenticated round-trip to confirm the credential works and the source is reachable. **Emit
nothing.** If there is no token yet (an instance created before its OAuth flow completes), do
config-only validation and return nil. Error messages surface to the user verbatim, so they must
not contain the token. Pattern: `connectors/gmail/gmail.go`, `Validate` (config parse → build
client → `users.getProfile` round-trip).

---

## 5. `FullSync` — resumable backfill

Emit every visible document, **checkpoint periodically**, and return the cursor incremental sync
continues from (typically "now" at the source).

```go
func (c *Connector) FullSync(ctx context.Context, cfg Config, emit sdk.Emit) (sdk.Cursor, error) {
    client, err := c.newClient(cfg)          // base_url + bearer token from cfg
    if err != nil { return "", err }

    // Capture the source's "current position" BEFORE listing, so changes that
    // race the backfill are caught by the first incremental pass (idempotent merge).
    head, err := client.currentCursor(ctx)
    if err != nil { return "", err }

    for page := range client.listAll(ctx) {  // honor ctx — the hub cancels on shutdown/disconnect/GDPR
        for _, rec := range page.Records {
            doc := c.toDocument(cfg, rec)
            if err := emit(ctx, doc); err != nil {
                return "", err               // Emit error ⇒ terminal pass
            }
        }
        if err := cfg.Checkpoint(ctx, sdk.Cursor(page.Token)); err != nil {
            return "", err                   // Checkpoint error ⇒ terminal pass
        }
    }
    return head, nil
}
```

Notes:

- **Checkpoint at a resumable boundary** (e.g. after each completed page) so an interrupted
  backfill resumes instead of restarting. `cfg.Checkpoint` is never nil at runtime (tests use
  `sdk.NopCheckpoint`).
- **Capture the head cursor before listing.** Gmail captures the mailbox `historyId` before paging
  and encodes the in-progress page token into the checkpoint cursor, so a mid-backfill resume
  finishes exactly like an uninterrupted run; convergence is via idempotent `(doc_id,
  version_etag)` upserts (`connectors/gmail/gmail.go` package doc, "Cursor format").
- **`ctx` cancellation is mandatory** — return promptly when it fires.

---

## 6. `IncrementalSync` — the poll path

Emit everything created, changed, or deleted (as a tombstone) since `cur`, and return the advanced
cursor. Returning `cur` unchanged with no emissions means "no changes". This is the path that backs
the 30-minute freshness SLA when webhooks are unavailable.

If the source reports the cursor is no longer replayable (e.g. an expired history/delta token),
return `sdk.ErrCursorExpired` (wrapped is fine). The hub responds by restarting `FullSync` for the
instance — **do not silently full-sync yourself.** Reference: `connectors/gmail/sync.go`.

---

## 7. `HandleWebhook` — the push path (optional)

If `Spec.SupportsWebhook` is false, return `sdk.ErrWebhookUnsupported` and the hub polls instead.
If true, the hub routes a source-originated HTTP request (already matched to this tenant+instance)
here. Verify it (authenticity + that it is for this instance), then either emit the affected
documents directly, or — if the payload is notification-only — return nil and let the hub trigger an
immediate `IncrementalSync` to fetch the changes. Gmail's push is notification-only: it validates
the Pub/Sub envelope and that the notification is for this mailbox, then returns nil
(`connectors/gmail/webhook.go`). MS Teams change-notifications are similar: validate the
subscription/clientState, then incremental-sync.

---

## 8. The cassette-based contract test (ADR-011)

Every connector ships a contract test that proves the invariants in CI **with no live API call**.
The default mechanism is a **cassette**: recorded request→response pairs replayed by an
`http.RoundTripper` the SDK test harness provides, asserted with `connectortest`.

Shape of the test:

```go
func TestMemoContract(t *testing.T) {
    // 1. Spec sanity.
    conn := memo.New()
    connectortest.RunSpecChecks(t, conn)

    // 2. Build a Config whose http.Client replays the committed cassette.
    cfg := connectortest.RunConnectorContract(t, conn, connectortest.Options{
        Cassette: "testdata/fullsync.yaml", // committed recording — no network in replay mode
        Tenant:   "tenant-a",
        Config:   []byte(`{"workspace":"acme"}`),
        Token:    []byte("redacted-test-token"),
    })

    // 3. Drive the connector and validate every emitted document.
    var rec connectortest.EmitRecorder
    cur, err := conn.FullSync(context.Background(), cfg, rec.Emit)
    if err != nil { t.Fatalf("FullSync: %v", err) }
    for _, doc := range rec.Docs() {
        connectortest.ValidateDocument(t, cfg, doc)
    }
    _ = cur // assert it advances; feed it into IncrementalSync for the incremental case
}
```

> The `connectortest.RunConnectorContract` entry point and the cassette record/replay transport are
> provided by the SDK test harness (`connectors/sdk/connectortest`). Check that package's GoDoc for
> the exact `Options` fields and helper names — the snippet above is illustrative of the workflow,
> not a frozen signature. The stable, already-shipped helpers you will always use are
> **`connectortest.RunSpecChecks`**, **`connectortest.EmitRecorder`**, and
> **`connectortest.ValidateDocument`** (`connectors/sdk/connectortest/connectortest.go`).

### Recording a cassette (once)

1. Run the test with `ASKER_RECORD=1` against a controllable endpoint — a local fake, a sandbox
   tenant, or (rarely, with real credentials, by a human, never in CI) the live API. The harness
   performs the real calls and writes the cassette file.
2. **Redact secrets** from the recording: Authorization headers, tokens, cookies, and any PII that
   is not the point of the test. A cassette is committed HTTP traffic — treat it like any artifact
   that could leak secrets.
3. **Commit** the cassette next to the connector (`testdata/`).
4. CI runs in replay mode (default, `ASKER_RECORD` unset): it reads the committed cassette and never
   touches the network. A request with no matching recorded entry is a test failure, not a live
   call.

Cover, at minimum: `FullSync` (asserting `ValidateDocument` on every doc and that the returned
cursor is usable), `IncrementalSync` from that cursor (an edit appears, a delete appears as a
tombstone with no body), and `Validate` (good token passes, the no-token case is config-only).

### When to use a fake server instead

If your contract test needs to *mutate* the source mid-test (seed → edit → delete to drive
incremental + tombstone flows), a small fake HTTP server is the better fit than a static cassette —
this is exactly the Gmail approach (`tools/fake-gmail`, ADR-008, driven via the harness in
`connectors/gmail/harness_test.go`). Both satisfy the no-live-API rule; cassettes are the default
for read-only request/response connectors, fakes for mutation-driven flows.

---

## 9. Checklist before you call it done

- [ ] `Spec.ID` is final, lowercase `^[a-z0-9-]+$`, and will never change.
- [ ] `tenant_id` on every emitted Document comes only from `cfg.Tenant.TenantID()`.
- [ ] `doc_id == sdk.DocID(connector_id, source_native_id)` on every Document and every tombstone.
- [ ] `version_etag` is always set and changes with content.
- [ ] `ts.ingested` is never set by the connector.
- [ ] Deletions emit as tombstones with no body and no chunks.
- [ ] Shared-source documents populate `Acl`; single-owner sources leave it unset (ADR-012).
- [ ] All methods honor `ctx` cancellation.
- [ ] `IncrementalSync` returns `sdk.ErrCursorExpired` on a dead cursor instead of self-full-syncing.
- [ ] `HandleWebhook` returns `sdk.ErrWebhookUnsupported` when there is no push path.
- [ ] The credential (`cfg.Token`) is never logged, persisted, or put in an error message.
- [ ] A cassette (or fake-server) contract test runs green in CI with no network, asserting
      `RunSpecChecks` + `ValidateDocument` across FullSync, IncrementalSync, and a tombstone.

Reference implementation for all of the above: **`connectors/gmail`**.
