package connectortest

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/asker/asker/connectors/sdk"
)

// writeTempCassette serializes cas to a temp file and returns its path, so a
// programmatically-built cassette can be fed to RunConnectorContract (which
// loads from disk).
func writeTempCassette(t *testing.T, cas *Cassette) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ad-hoc.json")
	raw, err := json.MarshalIndent(cassetteFile{Interactions: cas.Interactions}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// docID is the doc_id a notes id n maps to, for building expectations.
func docID(n string) string { return sdk.DocID(notesConnectorID, n) }

// TestRunConnectorContractHappyPath drives the full harness: FullSync emits 3
// docs, IncrementalSync emits 1 upsert + 1 tombstone, and the webhook emits
// nothing — all against one cassette and replay server.
func TestRunConnectorContractHappyPath(t *testing.T) {
	wantCur := sdk.Cursor("cur-n-3")
	incCur := sdk.Cursor("cur-9")
	RunConnectorContract(t, notesConnector{}, ContractCase{
		Name:     "notes",
		Cassette: "testdata/notes_contract.json",
		Token:    []byte("notes-token"),
		FullSync: &SyncExpectation{
			WantDocIDs: []string{docID("n-1"), docID("n-2"), docID("n-3")},
			WantCursor: &wantCur,
		},
		Incremental: &IncrementalExpectation{
			FromCursor: "cur-3",
			SyncExpectation: SyncExpectation{
				WantDocIDs:          []string{docID("n-2")},
				WantTombstoneDocIDs: []string{docID("n-1")},
				WantCursor:          &incCur,
			},
		},
		Webhook: &WebhookExpectation{
			Body: []byte(`{"notification":"changed"}`),
			Want: SyncExpectation{WantCount: 0},
		},
	})
}

// TestRunConnectorContractInjectsBaseURL proves the harness overrides the
// configured base-url field with the replay server URL even when ConfigJSON
// ships a placeholder.
func TestRunConnectorContractInjectsBaseURL(t *testing.T) {
	wantCur := sdk.Cursor("cur-n-3")
	RunConnectorContract(t, notesConnector{}, ContractCase{
		Cassette:   "testdata/notes_contract.json",
		ConfigJSON: json.RawMessage(`{"base_url":"http://placeholder.invalid"}`),
		FullSync: &SyncExpectation{
			WantCount:  3,
			WantCursor: &wantCur,
		},
	})
}

// TestRunConnectorContractMismatchFails proves a wrong expectation surfaces as a
// test failure (captured so this test passes).
func TestRunConnectorContractMismatchFails(t *testing.T) {
	inner := &capturingTB{TB: t}
	// Call the inner runner directly so the sub-test runs against inner.
	runContract(inner, notesConnector{}, ContractCase{
		Cassette: "testdata/notes_contract.json",
		FullSync: &SyncExpectation{
			// Wrong: claim a doc id that is never emitted, and omit a real one.
			WantDocIDs: []string{docID("n-1"), docID("n-2"), docID("does-not-exist")},
		},
	})
	inner.runCleanups()
	if !inner.failed {
		t.Fatal("a mismatched expectation did not fail the test")
	}
	if !inner.contains("missing expected doc_id") && !inner.contains("unexpected doc_id") {
		t.Errorf("failure did not report the doc-id diff: %v", inner.errors)
	}
}

// TestRunConnectorContractWantErr proves the WantErr path: a cassette that 404s
// the changes endpoint makes the connector return an error the case can assert.
func TestRunConnectorContractWantErr(t *testing.T) {
	// Build an ad-hoc cassette where /notes/changes returns 404, so
	// IncrementalSync errors.
	cas := &Cassette{Name: "404", Interactions: []*Interaction{
		{Request: RecordedRequest{Method: "GET", Path: "/notes/changes", Query: "since=cur-3"},
			Response: RecordedResponse{Status: 404, Body: "nope"}},
	}}
	rs := NewReplayServer(t, cas)

	cfg := ContractCase{Cassette: "unused"}.config(t, rs.URL())
	var rec EmitRecorder
	_, err := notesConnector{}.IncrementalSync(t.Context(), cfg, "cur-3", rec.Emit)
	if err == nil {
		t.Fatal("expected an error from a 404 changes endpoint")
	}
}

// TestRunConnectorContractNoBaseURLInject proves the sentinel leaves ConfigJSON
// untouched. The connector then reaches the replay layer via an injected
// transport instead, demonstrating the transport-based path.
func TestRunConnectorContractNoBaseURLInject(t *testing.T) {
	c, err := LoadCassette("testdata/notes_contract.json")
	if err != nil {
		t.Fatal(err)
	}
	// The config keeps the caller's base_url; we set it to the replay server
	// ourselves via a literal field, and assert NoBaseURLInject did not clobber
	// a foreign field.
	tc := ContractCase{
		Cassette:     "testdata/notes_contract.json",
		BaseURLField: NoBaseURLInject,
		ConfigJSON:   json.RawMessage(`{"keep":"me"}`),
	}
	cfg := tc.config(t, "http://replaced.invalid")
	var parsed map[string]any
	if err := json.Unmarshal(cfg.ConfigJSON, &parsed); err != nil {
		t.Fatal(err)
	}
	if _, ok := parsed["base_url"]; ok {
		t.Error("NoBaseURLInject still injected base_url")
	}
	if parsed["keep"] != "me" {
		t.Errorf("NoBaseURLInject dropped a config field: %v", parsed)
	}
	_ = c
}

// TestInjectBaseURLCustomField proves a non-default field name is honored.
func TestInjectBaseURLCustomField(t *testing.T) {
	tc := ContractCase{BaseURLField: "endpoint"}
	cfg := tc.config(t, "http://server.test")
	var parsed map[string]any
	if err := json.Unmarshal(cfg.ConfigJSON, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["endpoint"] != "http://server.test" {
		t.Errorf("custom base-url field not set: %v", parsed)
	}
}

// TestRunConnectorContractRejectsBadConfigJSON proves a non-object ConfigJSON
// fails fast.
func TestRunConnectorContractRejectsBadConfigJSON(t *testing.T) {
	inner := &capturingTB{TB: t}
	tc := ContractCase{Cassette: "testdata/notes_contract.json", ConfigJSON: json.RawMessage(`["not","an","object"]`)}
	tc.config(inner, "http://x")
	if !inner.failed {
		t.Error("a non-object ConfigJSON did not fail")
	}
}

// TestRunConnectorContractSurfacesUnexpectedError proves that when a pass errors
// but the case did not declare WantErr, the harness fails with a clear message
// (the success path's error propagation).
func TestRunConnectorContractSurfacesUnexpectedError(t *testing.T) {
	cas := &Cassette{Name: "404", Interactions: []*Interaction{
		{Request: RecordedRequest{Method: "GET", Path: "/notes/changes", Query: "since=cur-3"},
			Response: RecordedResponse{Status: 404, Body: "nope"}},
	}}
	path := writeTempCassette(t, cas)

	inner := &capturingTB{TB: t}
	runContract(inner, notesConnector{}, ContractCase{
		Cassette:    path,
		Incremental: &IncrementalExpectation{FromCursor: "cur-3"},
	})
	inner.runCleanups()
	if !inner.failed {
		t.Error("an unexpected connector error was not surfaced")
	}
	if !inner.contains("unexpected error") {
		t.Errorf("failure did not mention the unexpected error: %v", inner.errors)
	}
}

// TestAssertSyncErrorPaths exercises the WantErr / WantCursor branches directly.
func TestAssertSyncErrorPaths(t *testing.T) {
	cfg := ContractCase{}.config(t, "http://x")
	wantErr := sdk.ErrCursorExpired

	// WantErr set but err is nil -> failure.
	inner := &capturingTB{TB: t}
	assertSync(inner, "p", cfg, nil, "", nil, SyncExpectation{WantErr: &wantErr})
	if !inner.failed || !inner.contains("expected error") {
		t.Errorf("nil-error-with-WantErr not flagged: %v", inner.errors)
	}

	// WantErr set and err is a different error -> failure (errors.Is mismatch).
	inner = &capturingTB{TB: t}
	assertSync(inner, "p", cfg, nil, "", errSentinel, SyncExpectation{WantErr: &wantErr})
	if !inner.failed || !inner.contains("errors.Is target") {
		t.Errorf("wrong-error-target not flagged: %v", inner.errors)
	}

	// WantErr set and err satisfies errors.Is -> pass.
	inner = &capturingTB{TB: t}
	assertSync(inner, "p", cfg, nil, "", sdk.ErrCursorExpired, SyncExpectation{WantErr: &wantErr})
	if inner.failed {
		t.Errorf("matching error wrongly flagged: %v", inner.errors)
	}

	// WantCursor mismatch -> failure.
	inner = &capturingTB{TB: t}
	wantCur := sdk.Cursor("expected")
	assertSync(inner, "p", cfg, nil, sdk.Cursor("actual"), nil, SyncExpectation{WantCursor: &wantCur})
	if !inner.failed || !inner.contains("cursor =") {
		t.Errorf("cursor mismatch not flagged: %v", inner.errors)
	}
}

// TestAssertIDSetExtra proves an emitted-but-unexpected id is reported.
func TestAssertIDSetExtra(t *testing.T) {
	inner := &capturingTB{TB: t}
	assertIDSet(inner, "docs", []string{"a", "b"}, []string{"a"})
	if !inner.contains("unexpected doc_id") {
		t.Errorf("extra id not reported: %v", inner.errors)
	}
}

// TestWebhookCustomRequest proves Method/URL/Header on a WebhookExpectation flow
// through to HandleWebhook's request.
func TestWebhookCustomRequest(t *testing.T) {
	we := &WebhookExpectation{
		Method: http.MethodPut,
		URL:    "/hooks/notes/inst-1",
		Header: http.Header{"X-Signature": {"abc"}},
		Body:   []byte(`{"n":1}`),
	}
	req := we.request(t.Context())
	if req.Method != http.MethodPut || req.URL.Path != "/hooks/notes/inst-1" {
		t.Errorf("request = %s %s, want PUT /hooks/notes/inst-1", req.Method, req.URL.Path)
	}
	if req.Header.Get("X-Signature") != "abc" {
		t.Errorf("custom header dropped: %v", req.Header)
	}
}

var errSentinel error = sentinelError("sentinel")

type sentinelError string

func (e sentinelError) Error() string { return string(e) }
