package connectortest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

// ContractCase declares one connector contract scenario: a cassette of recorded
// HTTP interactions plus the documents the connector is expected to emit when
// run against it. It is the input to [RunConnectorContract], the one-call
// harness every M2 connector's contract test uses.
//
// The driver points the connector at a [ReplayServer] by injecting that
// server's URL into BaseURLField of the (parsed) ConfigJSON — the standard
// dev/CI base_url override, exactly as the e2e stack does. Connectors that
// build their own http.Client without a base_url override should instead inject
// a [ReplayTransport] in their own test and call the individual methods; this
// driver targets the base_url majority.
type ContractCase struct {
	// Name labels the scenario in test output (sub-test name).
	Name string

	// Cassette is the path to the recorded interactions JSON file (under the
	// connector's testdata/). Required.
	Cassette string

	// Tenant is the tenant id every emitted document must carry. Defaults to
	// "tenant-contract" when empty.
	Tenant string

	// ConfigJSON is the instance configuration passed to the connector. The
	// driver injects the replay server URL into BaseURLField before use, so
	// omit base_url here (or leave it as a placeholder — it is overwritten).
	ConfigJSON json.RawMessage

	// BaseURLField is the ConfigJSON object key that holds the API base URL
	// override. Empty means the default "base_url". Set it to NoBaseURLInject
	// to skip injection entirely (for a connector wired some other way).
	BaseURLField string

	// Token is the credential material placed in sdk.Config.Token (the
	// already-decrypted bearer/API key the hub would supply).
	Token []byte

	// InstanceID is the configured instance id. Defaults to "inst-contract".
	InstanceID string

	// FullSync, when set, runs c.FullSync and asserts on its emissions and
	// returned cursor.
	FullSync *SyncExpectation

	// Incremental, when set, runs c.IncrementalSync starting from FromCursor
	// and asserts on its emissions and returned cursor. It runs after FullSync
	// when both are present, against the SAME cassette and replay server, so
	// order your cassette interactions full-sync first.
	Incremental *IncrementalExpectation

	// Webhook, when set, builds an *http.Request from it and runs
	// c.HandleWebhook, asserting the emitted docs/tombstones and error.
	Webhook *WebhookExpectation
}

// SyncExpectation states what a FullSync (or any emitting pass) must produce.
type SyncExpectation struct {
	// WantDocIDs is the set of doc_ids (sdk.DocID values) the pass must emit
	// for non-tombstone documents. Order-independent. When nil, doc ids are not
	// asserted (only count and validity).
	WantDocIDs []string

	// WantTombstoneDocIDs is the set of doc_ids emitted as tombstones
	// (tombstone.deleted=true). Order-independent.
	WantTombstoneDocIDs []string

	// WantCount, when > 0, asserts the exact total number of emitted documents
	// (live + tombstone). Leave 0 to derive it from the WantDocIDs lengths.
	WantCount int

	// WantCursor, when non-nil, asserts the cursor the pass returns.
	WantCursor *sdk.Cursor

	// WantErr, when non-nil, asserts the pass returns an error satisfying
	// errors.Is(err, *WantErr) (e.g. sdk.ErrCursorExpired). When nil, the pass
	// must succeed.
	WantErr *error
}

// IncrementalExpectation is a SyncExpectation plus the cursor to resume from.
type IncrementalExpectation struct {
	SyncExpectation
	// FromCursor is the cursor handed to IncrementalSync.
	FromCursor sdk.Cursor
}

// WebhookExpectation drives a HandleWebhook assertion.
type WebhookExpectation struct {
	// Method, URL, Header, and Body build the *http.Request routed to
	// HandleWebhook (as the hub would after matching tenant+instance). Method
	// defaults to POST; URL defaults to "/webhook".
	Method string
	URL    string
	Header http.Header
	Body   []byte

	// Want states the docs/tombstones HandleWebhook must emit (Gmail-style
	// notification-only webhooks emit nothing — leave the id lists nil and
	// WantCount 0).
	Want SyncExpectation
}

// RunConnectorContract is THE one-call connector contract harness (M2 exit
// criterion: "contract tests pass for every connector against recorded
// fixtures, no live API calls in CI"). It:
//
//  1. loads tc.Cassette and starts a [ReplayServer],
//  2. builds an sdk.Config whose ConfigJSON base_url points at that server and
//     whose Tenant is a real tenancy.Context,
//  3. runs the requested FullSync / IncrementalSync / HandleWebhook passes,
//  4. asserts every emitted document satisfies [ValidateDocument] and the
//     expected (tombstone) doc-id sets, counts, cursors, and errors.
//
// Any request the connector makes that the cassette does not cover fails the
// test via the replay server's cleanup, so a contract drift surfaces as a clear
// "unmatched request" failure. Call it from a connector's _test.go:
//
//	connectortest.RunConnectorContract(t, gmail.New(), connectortest.ContractCase{
//		Cassette:   "testdata/backfill.json",
//		ConfigJSON: json.RawMessage(`{"user_email":"alice@example.com"}`),
//		Token:      []byte("fake-gmail-token:alice@example.com"),
//		FullSync:   &connectortest.SyncExpectation{WantDocIDs: []string{ /* ... */ }},
//	})
func RunConnectorContract(t *testing.T, c sdk.Connector, tc ContractCase) {
	t.Helper()
	name := tc.Name
	if name == "" {
		name = baseName(tc.Cassette)
	}
	t.Run(name, func(t *testing.T) {
		runContract(t, c, tc)
	})
}

func runContract(t testing.TB, c sdk.Connector, tc ContractCase) {
	t.Helper()
	if c == nil {
		t.Fatalf("connectortest: RunConnectorContract called with nil connector")
	}
	if tc.Cassette == "" {
		t.Fatalf("connectortest: ContractCase.Cassette is required")
	}

	cas, err := LoadCassette(tc.Cassette)
	if err != nil {
		t.Fatalf("connectortest: %v", err)
	}
	rs := NewReplayServer(t, cas)
	cfg := tc.config(t, rs.URL())
	ctx := context.Background()

	if tc.FullSync != nil {
		var rec EmitRecorder
		cur, err := c.FullSync(ctx, cfg, rec.Emit)
		assertSync(t, "FullSync", cfg, rec.Docs(), cur, err, *tc.FullSync)
	}

	if tc.Incremental != nil {
		var rec EmitRecorder
		cur, err := c.IncrementalSync(ctx, cfg, tc.Incremental.FromCursor, rec.Emit)
		assertSync(t, "IncrementalSync", cfg, rec.Docs(), cur, err, tc.Incremental.SyncExpectation)
	}

	if tc.Webhook != nil {
		var rec EmitRecorder
		req := tc.Webhook.request(ctx)
		err := c.HandleWebhook(ctx, cfg, req, rec.Emit)
		// HandleWebhook returns no cursor; pass the zero cursor and skip its check.
		exp := tc.Webhook.Want
		exp.WantCursor = nil
		assertSync(t, "HandleWebhook", cfg, rec.Docs(), "", err, exp)
	}
}

// config builds the sdk.Config for the case, injecting baseURL into the
// configured field of ConfigJSON.
func (tc ContractCase) config(t testing.TB, baseURL string) sdk.Config {
	t.Helper()
	tenant := tc.Tenant
	if tenant == "" {
		tenant = "tenant-contract"
	}
	tcx, err := tenancy.FromClaims(map[string]any{"tenant_id": tenant, "sub": "user-contract"})
	if err != nil {
		t.Fatalf("connectortest: tenancy.FromClaims(%q): %v", tenant, err)
	}

	raw := injectBaseURL(t, tc.ConfigJSON, tc.BaseURLField, baseURL)

	instanceID := tc.InstanceID
	if instanceID == "" {
		instanceID = "inst-contract"
	}
	return sdk.Config{
		Tenant:     tcx,
		InstanceID: instanceID,
		ConfigJSON: raw,
		Token:      tc.Token,
		Checkpoint: sdk.NopCheckpoint,
	}
}

// NoBaseURLInject, used as ContractCase.BaseURLField, disables base_url
// injection so the connector's ConfigJSON is passed through unchanged (for a
// connector that reaches the replay layer some other way, e.g. an injected
// ReplayTransport).
const NoBaseURLInject = "-"

// injectBaseURL rewrites the base-url field of a JSON config object to baseURL.
// An empty field name means the default "base_url"; the NoBaseURLInject
// sentinel returns the config unchanged. A nil/blank config becomes "{}".
func injectBaseURL(t testing.TB, cfg json.RawMessage, field, baseURL string) json.RawMessage {
	t.Helper()
	obj := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(cfg)) > 0 {
		if err := json.Unmarshal(cfg, &obj); err != nil {
			t.Fatalf("connectortest: ContractCase.ConfigJSON is not a JSON object: %v", err)
		}
	}
	if field == NoBaseURLInject {
		out, err := json.Marshal(obj)
		if err != nil {
			t.Fatalf("connectortest: marshal config: %v", err)
		}
		return out
	}
	if field == "" {
		field = "base_url"
	}
	urlRaw, err := json.Marshal(baseURL)
	if err != nil {
		t.Fatalf("connectortest: marshal base_url: %v", err)
	}
	obj[field] = urlRaw
	out, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("connectortest: marshal config: %v", err)
	}
	return out
}

// request builds the *http.Request HandleWebhook receives.
func (w *WebhookExpectation) request(ctx context.Context) *http.Request {
	method := w.Method
	if method == "" {
		method = http.MethodPost
	}
	target := w.URL
	if target == "" {
		target = "/webhook"
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(w.Body)).WithContext(ctx)
	for k, vs := range w.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return req
}

// assertSync checks one pass's emissions, cursor, and error against exp.
func assertSync(t testing.TB, pass string, cfg sdk.Config, docs []*askerv1.Document, cur sdk.Cursor, err error, exp SyncExpectation) {
	t.Helper()

	if exp.WantErr != nil {
		if err == nil {
			t.Errorf("%s: expected error %v, got nil", pass, *exp.WantErr)
			return
		}
		if !errors.Is(err, *exp.WantErr) {
			t.Errorf("%s: error = %v, want errors.Is target %v", pass, err, *exp.WantErr)
		}
		return
	}
	if err != nil {
		t.Errorf("%s: unexpected error: %v", pass, err)
		return
	}

	for _, doc := range docs {
		ValidateDocument(t, cfg, doc)
	}

	gotLive, gotTomb := splitDocIDs(docs)
	if exp.WantDocIDs != nil {
		assertIDSet(t, pass+" live docs", gotLive, exp.WantDocIDs)
	}
	if exp.WantTombstoneDocIDs != nil {
		assertIDSet(t, pass+" tombstones", gotTomb, exp.WantTombstoneDocIDs)
	}

	wantCount := exp.WantCount
	if wantCount == 0 && (exp.WantDocIDs != nil || exp.WantTombstoneDocIDs != nil) {
		wantCount = len(exp.WantDocIDs) + len(exp.WantTombstoneDocIDs)
	}
	if wantCount > 0 && len(docs) != wantCount {
		t.Errorf("%s: emitted %d documents, want %d", pass, len(docs), wantCount)
	}

	if exp.WantCursor != nil && cur != *exp.WantCursor {
		t.Errorf("%s: cursor = %q, want %q", pass, cur, *exp.WantCursor)
	}
}

// splitDocIDs partitions emitted docs into live and tombstone doc-id slices.
func splitDocIDs(docs []*askerv1.Document) (live, tomb []string) {
	for _, d := range docs {
		if d.GetTombstone().GetDeleted() {
			tomb = append(tomb, d.GetDocId())
		} else {
			live = append(live, d.GetDocId())
		}
	}
	return live, tomb
}

// assertIDSet compares two doc-id slices as sets, reporting missing and extra
// ids with a stable, readable diff.
func assertIDSet(t testing.TB, label string, got, want []string) {
	t.Helper()
	gotSet := make(map[string]int, len(got))
	for _, id := range got {
		gotSet[id]++
	}
	wantSet := make(map[string]int, len(want))
	for _, id := range want {
		wantSet[id]++
	}
	var missing, extra []string
	for id, n := range wantSet {
		if gotSet[id] < n {
			missing = append(missing, id)
		}
	}
	for id, n := range gotSet {
		if wantSet[id] < n {
			extra = append(extra, id)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("%s: missing expected doc_id(s): %v", label, missing)
	}
	if len(extra) > 0 {
		t.Errorf("%s: emitted unexpected doc_id(s): %v", label, extra)
	}
}
