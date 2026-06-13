package gdrive

import (
	"encoding/json"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
)

// docID is the canonical doc_id for a Drive file id under this connector.
func docID(fileID string) string { return sdk.DocID(connectorID, fileID) }

// TestContract is the M2 exit-criterion contract test: it drives FullSync,
// IncrementalSync, and a stale-cursor pass entirely against recorded cassettes
// (no live API), asserting emitted doc-ids, tombstones, cursors, and that every
// document satisfies connectortest.ValidateDocument.
func TestContract(t *testing.T) {
	t.Parallel()

	// 0. Spec sanity.
	connectortest.RunSpecChecks(t, New())

	const token = "test-access-token"

	// 1. Backfill: every visible file (the folder is skipped) is emitted; the
	//    returned cursor is the steady-state incremental cursor.
	backfillCur := sdk.Cursor("page:100")
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:       "backfill",
		Cassette:   "testdata/backfill.json",
		ConfigJSON: json.RawMessage(`{}`),
		Token:      []byte(token),
		FullSync: &connectortest.SyncExpectation{
			WantDocIDs: []string{
				docID("file-doc-1"),
				docID("file-txt-2"),
				docID("file-pdf-3"),
			},
			WantCursor: &backfillCur,
		},
	})

	// 2. Incremental: an edited file is re-emitted (upsert) and a removed file
	//    becomes a tombstone; the cursor advances to the new start page token.
	incCur := sdk.Cursor("page:205")
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:     "incremental",
		Cassette: "testdata/incremental.json",
		Token:    []byte(token),
		Incremental: &connectortest.IncrementalExpectation{
			FromCursor: backfillCur,
			SyncExpectation: connectortest.SyncExpectation{
				WantDocIDs:          []string{docID("file-doc-1")},
				WantTombstoneDocIDs: []string{docID("file-pdf-3")},
				WantCursor:          &incCur,
			},
		},
	})

	// 3. A stale changes page token surfaces as sdk.ErrCursorExpired.
	wantErr := sdk.ErrCursorExpired
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:     "stale-cursor",
		Cassette: "testdata/stale_cursor.json",
		Token:    []byte(token),
		Incremental: &connectortest.IncrementalExpectation{
			FromCursor:      sdk.Cursor("page:stale-token"),
			SyncExpectation: connectortest.SyncExpectation{WantErr: &wantErr},
		},
	})

	// 4. Webhook is unsupported: the connector reports it so the hub polls.
	wantWebhookErr := sdk.ErrWebhookUnsupported
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:     "webhook-unsupported",
		Cassette: "testdata/backfill.json", // any cassette; HandleWebhook makes no HTTP call
		Token:    []byte(token),
		Webhook: &connectortest.WebhookExpectation{
			Method: "POST",
			URL:    "/webhooks/gdrive/inst-1",
			Body:   []byte(`{"kind":"api#channel"}`),
			Want:   connectortest.SyncExpectation{WantErr: &wantWebhookErr},
		},
	})
}
