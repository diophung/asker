package outlookcal

import (
	"encoding/json"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
)

// docID is the doc_id an Outlook event native id maps to, for expectations.
func docID(eventID string) string { return sdk.DocID(connectorID, eventID) }

// fullSyncDeltaCursor is the cursor FullSync returns: the primed deltaLink
// re-hosted as a delta cursor (the @odata.deltaLink is stored verbatim).
var fullSyncDeltaCursor = deltaCursor("https://graph.microsoft.com/v1.0/me/calendarView/delta?$deltatoken=PRIME_TOKEN_AAA")

// TestContract is the M2 exit-criterion contract test: it drives FullSync,
// IncrementalSync (change + tombstone), the stale-cursor ErrCursorExpired path,
// and a mid-backfill resume entirely against committed cassettes — no live API.
func TestContract(t *testing.T) {
	t.Parallel()

	// 0. Spec sanity.
	connectortest.RunSpecChecks(t, New())

	// 1. Backfill: FullSync primes a delta token, then pages all events.
	connectortest.RunConnectorContract(t, newTestConnector(), connectortest.ContractCase{
		Name:       "fullsync",
		Cassette:   "testdata/fullsync.json",
		ConfigJSON: json.RawMessage(`{"user_principal_name":"alice@example.com"}`),
		Token:      []byte("decrypted-graph-token"),
		FullSync: &connectortest.SyncExpectation{
			WantDocIDs: []string{
				docID("AAMkEVT1"), docID("AAMkEVT2"),
				docID("AAMkEVT3"), docID("AAMkEVT4"),
			},
			WantCursor: &fullSyncDeltaCursor,
		},
	})

	// 2. Incremental: one edited event (upsert) and one deleted event
	//    (tombstone) since the delta cursor; cursor advances to the new token.
	incCursor := deltaCursor("https://graph.microsoft.com/v1.0/me/calendarView/delta?$deltatoken=NEXT_TOKEN_BBB")
	connectortest.RunConnectorContract(t, newTestConnector(), connectortest.ContractCase{
		Name:     "incremental",
		Cassette: "testdata/incremental.json",
		Token:    []byte("decrypted-graph-token"),
		Incremental: &connectortest.IncrementalExpectation{
			FromCursor: fullSyncDeltaCursor,
			SyncExpectation: connectortest.SyncExpectation{
				WantDocIDs:          []string{docID("AAMkEVT1")},
				WantTombstoneDocIDs: []string{docID("AAMkEVT2")},
				WantCursor:          &incCursor,
			},
		},
	})

	// 3. Stale cursor: a 410 resyncRequired from the delta query round-trips as
	//    sdk.ErrCursorExpired so the hub restarts a full sync.
	wantErr := sdk.ErrCursorExpired
	connectortest.RunConnectorContract(t, newTestConnector(), connectortest.ContractCase{
		Name:     "stale-cursor",
		Cassette: "testdata/stale_cursor.json",
		Token:    []byte("decrypted-graph-token"),
		Incremental: &connectortest.IncrementalExpectation{
			FromCursor: deltaCursor("https://graph.microsoft.com/v1.0/me/calendarView/delta?$deltatoken=EXPIRED_TOKEN_ZZZ"),
			SyncExpectation: connectortest.SyncExpectation{
				WantErr: &wantErr,
			},
		},
	})

	// 4. Resume mid-backfill: a "page:<nextLink>" checkpoint cursor re-primes a
	//    delta token and finishes the backfill at the saved page.
	resumeCursor := deltaCursor("https://graph.microsoft.com/v1.0/me/calendarView/delta?$deltatoken=RESUME_TOKEN_CCC")
	connectortest.RunConnectorContract(t, newTestConnector(), connectortest.ContractCase{
		Name:     "resume-backfill",
		Cassette: "testdata/resume_backfill.json",
		Token:    []byte("decrypted-graph-token"),
		Incremental: &connectortest.IncrementalExpectation{
			FromCursor: pageCursor("https://graph.microsoft.com/v1.0/me/events?$select=id,subject,body,bodyPreview,location,start,end,isAllDay,organizer,attendees,webLink,createdDateTime,lastModifiedDateTime&$top=50&$skip=50"),
			SyncExpectation: connectortest.SyncExpectation{
				WantDocIDs: []string{docID("AAMkEVT5")},
				WantCursor: &resumeCursor,
			},
		},
	})
}
