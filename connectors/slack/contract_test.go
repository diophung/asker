package slack

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
)

// TestContract is the M2 exit criterion: the connector runs against committed
// cassettes (no live Slack API) and every emitted document satisfies the
// canonical-Document invariants, with the expected doc-id sets, counts,
// cursors, and the stale-cursor error round-trip.
func TestContract(t *testing.T) {
	// 0. Spec sanity.
	connectortest.RunSpecChecks(t, New())

	// 1. FullSync: list two channels (paginated) plus a DM, then page each
	//    channel's history. The channel_join system message is filtered out;
	//    five real messages are emitted across C100 (3), C200 (1), D900 (1).
	backfillCursor := sdk.Cursor(`{"C100":"1700000300.000300","C200":"1700000050.000050","D900":"1700000075.000075"}`)
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:     "fullsync",
		Cassette: "testdata/fullsync.json",
		Tenant:   testTenant,
		Token:    []byte(testToken),
		FullSync: &connectortest.SyncExpectation{
			WantDocIDs: []string{
				docID("C100", "1700000300.000300"),
				docID("C100", "1700000200.000200"),
				docID("C100", "1700000100.000100"),
				docID("C200", "1700000050.000050"),
				docID("D900", "1700000075.000075"),
			},
			WantCursor: &backfillCursor,
		},
	})

	// 2. IncrementalSync from the backfill cursor: C100 yields one edited
	//    message (same doc_id, new version_etag) and one new message; the
	//    other channels are quiet. The cursor advances to C100's newest ts.
	incCursor := sdk.Cursor(`{"C100":"1700000400.000400","C200":"1700000050.000050","D900":"1700000075.000075"}`)
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:     "incremental",
		Cassette: "testdata/incremental.json",
		Tenant:   testTenant,
		Token:    []byte(testToken),
		Incremental: &connectortest.IncrementalExpectation{
			FromCursor: backfillCursor,
			SyncExpectation: connectortest.SyncExpectation{
				WantDocIDs: []string{
					docID("C100", "1700000300.000300"), // edited (upsert)
					docID("C100", "1700000400.000400"), // new
				},
				WantCursor: &incCursor,
			},
		},
	})

	// 3. A Slack "invalid_cursor" from conversations.history surfaces as
	//    sdk.ErrCursorExpired so the hub restarts a full sync.
	wantErr := sdk.ErrCursorExpired
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:     "stale-cursor",
		Cassette: "testdata/stale_cursor.json",
		Tenant:   testTenant,
		Token:    []byte(testToken),
		Incremental: &connectortest.IncrementalExpectation{
			FromCursor:      sdk.Cursor(`{"C100":"1700000300.000300"}`),
			SyncExpectation: connectortest.SyncExpectation{WantErr: &wantErr},
		},
	})

	// 4. Webhook delete: a "message_deleted" Events API callback emits a
	//    tombstone for the deleted message (no body). HandleWebhook makes no
	//    HTTP call, so any cassette serves. RunConnectorContract builds the
	//    connector with the default time.Now clock, so the request is signed
	//    at the real now to pass the freshness check.
	deleteBody := []byte(`{"type":"event_callback","team_id":"T1","event":{"type":"message","subtype":"message_deleted","channel":"C100","deleted_ts":"1700000200.000200","event_ts":"1700000600.000600","ts":"1700000600.000600"}}`)
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:       "webhook-delete",
		Cassette:   "testdata/fullsync.json",
		Tenant:     testTenant,
		Token:      []byte(testToken),
		ConfigJSON: mustJSON(t, map[string]string{"signing_secret": testSigningSecret}),
		Webhook: &connectortest.WebhookExpectation{
			Method: http.MethodPost,
			URL:    "/webhooks/slack/inst-1",
			Header: signedHeadersNow(deleteBody),
			Body:   deleteBody,
			Want: connectortest.SyncExpectation{
				WantTombstoneDocIDs: []string{docID("C100", "1700000200.000200")},
			},
		},
	})

	// 5. Webhook create: a plain "message" Events API callback emits an
	//    upsert document for the new message.
	createBody := []byte(`{"type":"event_callback","team_id":"T1","event":{"type":"message","channel":"C100","user":"U_ALICE","text":"hot take incoming","ts":"1700000700.000700","event_ts":"1700000700.000700","team":"T1"}}`)
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:       "webhook-create",
		Cassette:   "testdata/fullsync.json",
		Tenant:     testTenant,
		Token:      []byte(testToken),
		ConfigJSON: mustJSON(t, map[string]string{"signing_secret": testSigningSecret}),
		Webhook: &connectortest.WebhookExpectation{
			Method: http.MethodPost,
			URL:    "/webhooks/slack/inst-1",
			Header: signedHeadersNow(createBody),
			Body:   createBody,
			Want: connectortest.SyncExpectation{
				WantDocIDs: []string{docID("C100", "1700000700.000700")},
			},
		},
	})
}

// signedHeadersNow returns the X-Slack-Request-Timestamp and X-Slack-Signature
// headers for body, signed with testSigningSecret at the current time so a
// connector using the default time.Now clock (as RunConnectorContract builds)
// accepts the request as fresh.
func signedHeadersNow(body []byte) http.Header {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	h := http.Header{}
	h.Set(timestampHeader, ts)
	h.Set(signatureHeader, computeSignature(testSigningSecret, ts, body))
	return h
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
