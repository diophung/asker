package jira

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
)

// docID is the canonical doc_id for a Jira issue's immutable numeric id (NOT
// its mutable key) — see issueDocument.
func docID(id string) string { return sdk.DocID(connectorID, id) }

// TestContract is the M2 exit-criterion contract test: it drives FullSync,
// IncrementalSync (a change + a tombstone, with the inclusive JQL boundary
// issue deduped), and a stale cursor entirely against committed cassettes — no
// live API. RunConnectorContract asserts ValidateDocument on every emitted
// document plus the expected doc-id sets, counts, and cursors.
func TestContract(t *testing.T) {
	t.Parallel()

	// Spec sanity.
	connectortest.RunSpecChecks(t, New())

	// FullSync: two pages (2 + 1 issues), all live TICKET docs, cursor at the
	// newest issue.
	fullCursor := sdk.Cursor("updated:2026/06/11 14:45|key:DEMO-3")
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:       "fullsync",
		Cassette:   "testdata/fullsync.json",
		ConfigJSON: json.RawMessage(`{}`),
		Token:      []byte("decrypted-oauth-token"),
		FullSync: &connectortest.SyncExpectation{
			// doc_ids derive from the immutable numeric issue ids:
			// DEMO-1=10001, DEMO-2=10002, DEMO-3=10003.
			WantDocIDs: []string{docID("10001"), docID("10002"), docID("10003")},
			WantCursor: &fullCursor,
		},
	})

	// IncrementalSync from the DEMO-2 boundary cursor: DEMO-2 is the inclusive
	// JQL boundary and must be deduped; DEMO-3 changed; DEMO-4 is new; DEMO-5
	// reached the configured "Removed" status -> tombstone.
	incCursor := sdk.Cursor("updated:2026/06/12 16:00|key:DEMO-5")
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:       "incremental",
		Cassette:   "testdata/incremental.json",
		ConfigJSON: json.RawMessage(`{"deleted_statuses":["Removed"]}`),
		Token:      []byte("decrypted-oauth-token"),
		Incremental: &connectortest.IncrementalExpectation{
			FromCursor: sdk.Cursor("updated:2026/06/10 10:30|key:DEMO-2"),
			SyncExpectation: connectortest.SyncExpectation{
				// DEMO-3=10003 changed, DEMO-4=10004 is new, DEMO-5=10005
				// reached the "Removed" status -> tombstone (all by immutable id).
				WantDocIDs:          []string{docID("10003"), docID("10004")},
				WantTombstoneDocIDs: []string{docID("10005")},
				WantCursor:          &incCursor,
			},
		},
	})

	// A cursor bound the source rejects (HTTP 400) surfaces as
	// sdk.ErrCursorExpired so the hub restarts a full sync.
	wantErr := sdk.ErrCursorExpired
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:       "stale-cursor",
		Cassette:   "testdata/stale_cursor.json",
		ConfigJSON: json.RawMessage(`{}`),
		Token:      []byte("decrypted-oauth-token"),
		Incremental: &connectortest.IncrementalExpectation{
			FromCursor:      sdk.Cursor("updated:2099/01/01 00:00|key:DEMO-X"),
			SyncExpectation: connectortest.SyncExpectation{WantErr: &wantErr},
		},
	})
}

// TestIncrementalNoChanges: an incremental pass whose only result is the
// boundary issue (deduped) emits nothing and returns the same cursor it was
// handed.
func TestIncrementalNoChanges(t *testing.T) {
	t.Parallel()
	rs := newReplayServer(t, "testdata/incremental_nochange.json")
	c := New()
	cfg := contractConfig(t, rs.URL(), `{}`)

	from := sdk.Cursor("updated:2026/06/10 10:30|key:DEMO-2")
	var rec connectortest.EmitRecorder
	next, err := c.IncrementalSync(context.Background(), cfg, from, rec.Emit)
	if err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	if next != from {
		t.Errorf("cursor advanced from %q to %q with no changes", from, next)
	}
	if n := len(rec.Docs()); n != 0 {
		t.Errorf("emitted %d documents with no changes", n)
	}
}
