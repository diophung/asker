package outlookmail

import (
	"encoding/json"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
)

// docID is the doc_id an Outlook message native id maps to, for expectations.
func docID(nativeID string) string { return sdk.DocID(connectorID, nativeID) }

// TestContract is the M2 exit-criterion contract test: it drives the connector
// against committed cassettes via a ReplayServer (no live API), asserting the
// emitted doc_ids/types/cursors on FullSync, the change+delete on
// IncrementalSync (the deleted one a tombstone), ValidateDocument on every
// emitted doc, RunSpecChecks, and the ErrCursorExpired round-trip.
func TestContract(t *testing.T) {
	t.Parallel()

	connectortest.RunSpecChecks(t, New())

	cfgJSON := json.RawMessage(`{"user_principal_name":"alice@contoso.com"}`)

	// FullSync: two list pages (3 messages total) then the delta-token init.
	backfillCursor := sdk.Cursor(deltaTokenPrefix + "DELTA_TOKEN_AFTER_BACKFILL")
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:       "fullsync",
		Cassette:   "testdata/fullsync.json",
		ConfigJSON: cfgJSON,
		Token:      []byte("decrypted-graph-token"),
		FullSync: &connectortest.SyncExpectation{
			WantDocIDs: []string{docID("AAMkADmsg1"), docID("AAMkADmsg2"), docID("AAMkADmsg3")},
			WantCursor: &backfillCursor,
		},
	})

	// Incremental: a two-page delta with one edit (upsert) and one deletion
	// (tombstone), advancing the cursor.
	incCursor := sdk.Cursor(deltaTokenPrefix + "DELTA_TOKEN_NEXT")
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:       "incremental",
		Cassette:   "testdata/incremental.json",
		ConfigJSON: cfgJSON,
		Token:      []byte("decrypted-graph-token"),
		Incremental: &connectortest.IncrementalExpectation{
			FromCursor: backfillCursor,
			SyncExpectation: connectortest.SyncExpectation{
				WantDocIDs:          []string{docID("AAMkADmsg1")},
				WantTombstoneDocIDs: []string{docID("AAMkADmsg2")},
				WantCursor:          &incCursor,
			},
		},
	})

	// Stale cursor: a 410 Gone on the delta token surfaces as ErrCursorExpired.
	wantErr := sdk.ErrCursorExpired
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:       "stale-cursor",
		Cassette:   "testdata/stale_cursor.json",
		ConfigJSON: cfgJSON,
		Token:      []byte("decrypted-graph-token"),
		Incremental: &connectortest.IncrementalExpectation{
			FromCursor:      sdk.Cursor(deltaTokenPrefix + "EXPIRED_TOKEN"),
			SyncExpectation: connectortest.SyncExpectation{WantErr: &wantErr},
		},
	})

	// Webhook: Outlook Mail has no push path this milestone.
	wantWebhookErr := sdk.ErrWebhookUnsupported
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:       "webhook-unsupported",
		Cassette:   "testdata/fullsync.json",
		ConfigJSON: cfgJSON,
		Token:      []byte("decrypted-graph-token"),
		Webhook: &connectortest.WebhookExpectation{
			Body: []byte(`{"value":[{"changeType":"created"}]}`),
			Want: connectortest.SyncExpectation{WantErr: &wantWebhookErr},
		},
	})
}
