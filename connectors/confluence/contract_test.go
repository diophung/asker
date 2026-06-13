package confluence

import (
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
)

// docID is the canonical doc_id for a Confluence page native id.
func docID(pageID string) string { return sdk.DocID(connectorID, pageID) }

// newTestConnector builds a connector with a quiet logger; tests that assert on
// cursors do not depend on the clock, so the default time source is fine.
func newTestConnector() sdk.Connector {
	return New(WithLogger(slog.New(slog.DiscardHandler)))
}

// TestContract is the M2 exit-criterion contract test: every assertion runs
// against committed cassettes via connectortest, with no live API call.
func TestContract(t *testing.T) {
	// Spec sanity.
	connectortest.RunSpecChecks(t, newTestConnector())

	cfgJSON := json.RawMessage(`{"space_key":"DEV"}`)
	token := []byte("decrypted-bearer-token")

	// 1. FullSync backfills every page across the offset-paginated set. The
	//    returned cursor is the newest version.when seen (page 102).
	backfillCursor := sdk.Cursor("2026-06-03 16:45")
	connectortest.RunConnectorContract(t, newTestConnector(), connectortest.ContractCase{
		Name:       "backfill",
		Cassette:   "testdata/backfill.json",
		ConfigJSON: cfgJSON,
		Token:      token,
		FullSync: &connectortest.SyncExpectation{
			WantDocIDs: []string{docID("100"), docID("101"), docID("102"), docID("103")},
			WantCursor: &backfillCursor,
		},
	})

	// 2. IncrementalSync from the backfill cursor: one CHANGE (page 101 edited)
	//    and one DELETE (page 103 trashed -> tombstone). Cursor advances to the
	//    newest lastModified across both windows (the edited page at 10:00).
	incCursor := sdk.Cursor("2026-06-04 10:00")
	connectortest.RunConnectorContract(t, newTestConnector(), connectortest.ContractCase{
		Name:       "incremental",
		Cassette:   "testdata/incremental.json",
		ConfigJSON: cfgJSON,
		Token:      token,
		Incremental: &connectortest.IncrementalExpectation{
			FromCursor: backfillCursor,
			SyncExpectation: connectortest.SyncExpectation{
				WantDocIDs:          []string{docID("101")},
				WantTombstoneDocIDs: []string{docID("103")},
				WantCursor:          &incCursor,
			},
		},
	})

	// 3. IncrementalSync with no changes: both windows return empty, the cursor
	//    is unchanged, and nothing is emitted.
	noChangeCursor := sdk.Cursor("2026-06-04 10:00")
	connectortest.RunConnectorContract(t, newTestConnector(), connectortest.ContractCase{
		Name:       "incremental-no-change",
		Cassette:   "testdata/incremental_empty.json",
		ConfigJSON: cfgJSON,
		Token:      token,
		Incremental: &connectortest.IncrementalExpectation{
			FromCursor: incCursor,
			SyncExpectation: connectortest.SyncExpectation{
				WantCount:  0,
				WantCursor: &noChangeCursor,
			},
		},
	})

	// 4. Webhook is unsupported: HandleWebhook returns sdk.ErrWebhookUnsupported.
	errWebhook := sdk.ErrWebhookUnsupported
	connectortest.RunConnectorContract(t, newTestConnector(), connectortest.ContractCase{
		Name:       "webhook-unsupported",
		Cassette:   "testdata/incremental_empty.json",
		ConfigJSON: cfgJSON,
		Token:      token,
		Webhook: &connectortest.WebhookExpectation{
			Method: "POST",
			URL:    "/webhooks/confluence/inst-1",
			Body:   []byte(`{"event":"page_updated"}`),
			Want:   connectortest.SyncExpectation{WantErr: &errWebhook},
		},
	})
}
