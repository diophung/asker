package s3

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
)

// docID is the canonical doc_id for an object under this connector. Objects are
// identified natively by "<bucket>/<key>".
func docID(bucket, key string) string { return sdk.DocID(connectorID, nativeID(bucket, key)) }

// mustTime parses an RFC3339 timestamp for fixture expectations.
func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("mustTime(%q): %v", s, err)
	}
	return ts.UTC()
}

// TestContract is the M2 exit-criterion contract test: it drives FullSync,
// IncrementalSync, and a stale-cursor pass entirely against recorded cassettes
// (no live API), asserting emitted doc-ids, tombstones, cursors, and that every
// document satisfies connectortest.ValidateDocument. The endpoint base_url
// override is injected into the "endpoint" config field; the connector strips
// the scheme to the host:port minio-go wants.
func TestContract(t *testing.T) {
	t.Parallel()

	// 0. Spec sanity.
	connectortest.RunSpecChecks(t, New())

	const (
		token  = "AKIDTEST:secrettest"
		bucket = "my-bucket"
	)
	configJSON := json.RawMessage(`{"bucket":"my-bucket","prefix":"docs/"}`)

	// The backfill cursor is computed from the listed objects: no LastKey (the
	// pass completed), the max LastModified (notes.txt), and the sorted keyset.
	backfillCur := completedCursor(
		mustTime(t, "2026-06-11T14:15:00Z"),
		[]string{"docs/big.txt", "docs/data.json", "docs/notes.txt", "docs/report.pdf"},
	)

	// 1. Backfill: every object under the prefix is emitted as a FILE document;
	//    the returned cursor is the steady-state incremental cursor.
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:         "backfill",
		Cassette:     "testdata/backfill.json",
		BaseURLField: "endpoint",
		ConfigJSON:   configJSON,
		Token:        []byte(token),
		FullSync: &connectortest.SyncExpectation{
			WantDocIDs: []string{
				docID(bucket, "docs/big.txt"),
				docID(bucket, "docs/data.json"),
				docID(bucket, "docs/notes.txt"),
				docID(bucket, "docs/report.pdf"),
			},
			WantCursor: &backfillCur,
		},
	})

	// 2. Incremental: an object with a newer LastModified is re-emitted (upsert)
	//    and a vanished object becomes a tombstone; unchanged objects are
	//    skipped. The cursor advances to the new max LastModified + keyset.
	incCur := completedCursor(
		mustTime(t, "2026-06-12T10:00:00Z"),
		[]string{"docs/big.txt", "docs/data.json", "docs/notes.txt"},
	)
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:         "incremental",
		Cassette:     "testdata/incremental.json",
		BaseURLField: "endpoint",
		ConfigJSON:   configJSON,
		Token:        []byte(token),
		Incremental: &connectortest.IncrementalExpectation{
			FromCursor: backfillCur,
			SyncExpectation: connectortest.SyncExpectation{
				WantDocIDs:          []string{docID(bucket, "docs/notes.txt")},
				WantTombstoneDocIDs: []string{docID(bucket, "docs/report.pdf")},
				WantCursor:          &incCur,
			},
		},
	})

	// 3. An unparseable cursor surfaces as sdk.ErrCursorExpired (no HTTP call is
	//    made, so any cassette works).
	wantErr := sdk.ErrCursorExpired
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:         "stale-cursor",
		Cassette:     "testdata/stale_cursor.json",
		BaseURLField: "endpoint",
		ConfigJSON:   configJSON,
		Token:        []byte(token),
		Incremental: &connectortest.IncrementalExpectation{
			FromCursor:      sdk.Cursor("not-a-valid-cursor"),
			SyncExpectation: connectortest.SyncExpectation{WantErr: &wantErr},
		},
	})

	// 4. Webhook is unsupported: the connector reports it so the hub polls.
	wantWebhookErr := sdk.ErrWebhookUnsupported
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:         "webhook-unsupported",
		Cassette:     "testdata/stale_cursor.json", // any cassette; HandleWebhook makes no HTTP call
		BaseURLField: "endpoint",
		ConfigJSON:   configJSON,
		Token:        []byte(token),
		Webhook: &connectortest.WebhookExpectation{
			Method: "POST",
			URL:    "/webhooks/s3/inst-1",
			Body:   []byte(`{"Records":[]}`),
			Want:   connectortest.SyncExpectation{WantErr: &wantWebhookErr},
		},
	})
}
