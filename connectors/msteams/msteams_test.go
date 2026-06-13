package msteams

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

// docID is the canonical doc_id for a (chatID, messageID) pair.
func docID(chatID, messageID string) string {
	return sdk.DocID(connectorID, nativeID(chatID, messageID))
}

// fullSyncCursor is the cursor FullSync returns for the fullsync.json fixture:
// a per-chat map of chat id -> primed deltaLink. encoding/json sorts object
// keys, so this value is deterministic.
const fullSyncCursor = `{"deltas":{"19:group-a":"https://graph.microsoft.com/v1.0/chats/19:group-a/messages/delta?$deltatoken=DELTA-A-1","19:oneone-b":"https://graph.microsoft.com/v1.0/chats/19:oneone-b/messages/delta?$deltatoken=DELTA-B-1"}}`

func TestSpec(t *testing.T) {
	connectortest.RunSpecChecks(t, New())

	spec := New().Spec()
	if spec.ID != connectorID {
		t.Errorf("Spec.ID = %q, want %q", spec.ID, connectorID)
	}
	if spec.AuthType != sdk.AuthOAuth2 {
		t.Errorf("Spec.AuthType = %v, want AuthOAuth2", spec.AuthType)
	}
	if spec.SupportsWebhook {
		t.Error("Spec.SupportsWebhook = true, want false (Graph change notifications deferred)")
	}
}

func TestContractFullSync(t *testing.T) {
	wantCursor := sdk.Cursor(fullSyncCursor)
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:     "fullsync",
		Cassette: "testdata/fullsync.json",
		Token:    []byte("decrypted-graph-token"),
		FullSync: &connectortest.SyncExpectation{
			WantDocIDs: []string{
				docID("19:group-a", "1001"),
				docID("19:group-a", "1002"),
				docID("19:oneone-b", "2001"),
			},
			WantCursor: &wantCursor,
		},
	})
}

func TestContractIncremental(t *testing.T) {
	wantCursor := sdk.Cursor(`{"deltas":{"19:group-a":"https://graph.microsoft.com/v1.0/chats/19:group-a/messages/delta?$deltatoken=DELTA-A-2","19:oneone-b":"https://graph.microsoft.com/v1.0/chats/19:oneone-b/messages/delta?$deltatoken=DELTA-B-2"}}`)
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:     "incremental",
		Cassette: "testdata/incremental.json",
		Token:    []byte("decrypted-graph-token"),
		Incremental: &connectortest.IncrementalExpectation{
			FromCursor: sdk.Cursor(fullSyncCursor),
			SyncExpectation: connectortest.SyncExpectation{
				WantDocIDs:          []string{docID("19:group-a", "1001")}, // edited
				WantTombstoneDocIDs: []string{docID("19:group-a", "1002")}, // deleted
				WantCursor:          &wantCursor,
			},
		},
	})
}

func TestContractStaleCursor(t *testing.T) {
	wantErr := sdk.ErrCursorExpired
	staleCursor := sdk.Cursor(`{"deltas":{"19:group-a":"https://graph.microsoft.com/v1.0/chats/19:group-a/messages/delta?$deltatoken=EXPIRED"}}`)
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:     "stale-cursor",
		Cassette: "testdata/stale_cursor.json",
		Token:    []byte("decrypted-graph-token"),
		Incremental: &connectortest.IncrementalExpectation{
			FromCursor:      staleCursor,
			SyncExpectation: connectortest.SyncExpectation{WantErr: &wantErr},
		},
	})
}

func TestContractWebhookUnsupported(t *testing.T) {
	wantErr := sdk.ErrWebhookUnsupported
	connectortest.RunConnectorContract(t, New(), connectortest.ContractCase{
		Name:     "webhook-unsupported",
		Cassette: "testdata/validate.json", // any cassette; HandleWebhook makes no HTTP call
		Token:    []byte("decrypted-graph-token"),
		Webhook: &connectortest.WebhookExpectation{
			Method: "POST",
			URL:    "/webhooks/msteams/inst-1",
			Body:   []byte(`{"value":[]}`),
			Want:   connectortest.SyncExpectation{WantErr: &wantErr},
		},
	})
}

// TestFullSyncThenIncremental proves the cursor FullSync returns drives a
// follow-on IncrementalSync end-to-end against a combined cassette, exactly as
// the hub would chain the two passes.
func TestFullSyncThenIncremental(t *testing.T) {
	cas, err := connectortest.LoadCassette("testdata/full_then_incremental.json")
	if err != nil {
		t.Fatalf("load cassette: %v", err)
	}
	rs := connectortest.NewReplayServer(t, cas)
	cfg := buildConfig(t, rs.URL(), "tenant-a")
	ctx := context.Background()

	conn := New()
	var full connectortest.EmitRecorder
	cur, err := conn.FullSync(ctx, cfg, full.Emit)
	if err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	for _, d := range full.Docs() {
		connectortest.ValidateDocument(t, cfg, d)
	}
	if got := len(full.Docs()); got != 3 {
		t.Fatalf("FullSync emitted %d docs, want 3", got)
	}

	var inc connectortest.EmitRecorder
	cur2, err := conn.IncrementalSync(ctx, cfg, cur, inc.Emit)
	if err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	if cur2 == cur {
		t.Errorf("IncrementalSync did not advance the cursor")
	}
	docs := inc.Docs()
	for _, d := range docs {
		connectortest.ValidateDocument(t, cfg, d)
	}
	var liveCount, tombCount int
	for _, d := range docs {
		if d.GetTombstone().GetDeleted() {
			tombCount++
		} else {
			liveCount++
		}
	}
	if liveCount != 1 || tombCount != 1 {
		t.Errorf("IncrementalSync: live=%d tomb=%d, want 1 and 1", liveCount, tombCount)
	}
}

// TestMessageMapping asserts the document field mapping in detail for one
// backfilled message: title, body (HTML stripped), participants, metadata, and
// timestamps.
func TestMessageMapping(t *testing.T) {
	cas, err := connectortest.LoadCassette("testdata/fullsync.json")
	if err != nil {
		t.Fatalf("load cassette: %v", err)
	}
	rs := connectortest.NewReplayServer(t, cas)
	cfg := buildConfig(t, rs.URL(), "tenant-a")

	conn := New()
	var rec connectortest.EmitRecorder
	if _, err := conn.FullSync(context.Background(), cfg, rec.Emit); err != nil {
		t.Fatalf("FullSync: %v", err)
	}

	byID := map[string]*askerv1.Document{}
	for _, d := range rec.Docs() {
		byID[d.GetSourceNativeId()] = d
	}

	msg := byID["19:group-a:1001"]
	if msg == nil {
		t.Fatal("missing document for 19:group-a:1001")
	}
	if msg.GetType() != askerv1.DocType_CHAT_MESSAGE {
		t.Errorf("type = %v, want CHAT_MESSAGE", msg.GetType())
	}
	if got, want := msg.GetTitle(), "Kickoff at noon."; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
	if got := msg.GetBodyText(); !strings.Contains(got, "Kickoff at") || strings.Contains(got, "<") {
		t.Errorf("body_text = %q; want HTML stripped plain text", got)
	}
	if got := msg.GetVersionEtag(); got != "1700000000001" {
		t.Errorf("version_etag = %q, want message etag", got)
	}
	if msg.GetTs().GetCreated() == nil {
		t.Error("ts.created is nil; want createdDateTime")
	}
	if msg.GetTs().GetIngested() != nil {
		t.Error("ts.ingested set; the hub must stamp it")
	}

	// from.user is role "from"; chat members are role "member"; the author is
	// not duplicated.
	var fromCount, memberCount int
	for _, p := range msg.GetParticipants() {
		switch p.GetRole() {
		case "from":
			fromCount++
			if p.GetName() != "Alice Doe" || p.GetHandle() != "u-alice" {
				t.Errorf("from participant = %+v, want Alice Doe/u-alice", p)
			}
		case "member":
			memberCount++
		}
	}
	if fromCount != 1 {
		t.Errorf("from participants = %d, want 1", fromCount)
	}
	if memberCount != 1 { // 2 members, but Alice is the author and deduped
		t.Errorf("member participants = %d, want 1 (author deduped)", memberCount)
	}

	md := msg.GetMetadata()
	for k, want := range map[string]string{
		"chat_id":    "19:group-a",
		"message_id": "1001",
		"chat_type":  "group",
		"importance": "normal",
	} {
		if md[k] != want {
			t.Errorf("metadata[%q] = %q, want %q", k, md[k], want)
		}
	}
	if md["web_url"] == "" {
		t.Error("metadata[web_url] is empty")
	}

	// A group chat carries ACL (shared); the one-on-one chat does not.
	if msg.GetAcl() == nil || !msg.GetAcl().GetIsPrivate() {
		t.Error("group-chat message should carry a private Acl with allowed principals")
	}
	if len(msg.GetAcl().GetAllowedPrincipals()) != 2 {
		t.Errorf("group-chat Acl principals = %d, want 2", len(msg.GetAcl().GetAllowedPrincipals()))
	}

	oneOnOne := byID["19:oneone-b:2001"]
	if oneOnOne == nil {
		t.Fatal("missing document for 19:oneone-b:2001")
	}
	if oneOnOne.GetAcl() != nil {
		t.Error("one-on-one chat message should not carry an Acl (private by construction)")
	}
}

func TestValidate(t *testing.T) {
	t.Run("good token round-trips", func(t *testing.T) {
		cas, err := connectortest.LoadCassette("testdata/validate.json")
		if err != nil {
			t.Fatalf("load cassette: %v", err)
		}
		rs := connectortest.NewReplayServer(t, cas)
		cfg := buildConfig(t, rs.URL(), "tenant-a")
		cfg.Token = []byte("decrypted-graph-token")
		if err := New().Validate(context.Background(), cfg); err != nil {
			t.Errorf("Validate: %v", err)
		}
	})

	t.Run("no token is config-only", func(t *testing.T) {
		// No replay server: a token-less Validate must make no HTTP call.
		cfg := buildConfig(t, "http://127.0.0.1:0", "tenant-a")
		cfg.Token = nil
		if err := New().Validate(context.Background(), cfg); err != nil {
			t.Errorf("Validate (no token): %v", err)
		}
	})

	t.Run("bad base_url is rejected", func(t *testing.T) {
		cfg := buildConfig(t, "://nope", "tenant-a")
		cfg.Token = []byte("t")
		err := New().Validate(context.Background(), cfg)
		if err == nil {
			t.Fatal("Validate accepted a malformed base_url")
		}
		if strings.Contains(err.Error(), "decrypted") {
			t.Error("error message leaks token material")
		}
	})
}

func TestDecodeCursorExpired(t *testing.T) {
	_, err := decodeCursor(sdk.Cursor("not-json"))
	if !errors.Is(err, sdk.ErrCursorExpired) {
		t.Errorf("decodeCursor(bad) error = %v, want ErrCursorExpired", err)
	}
}

func TestStripHTML(t *testing.T) {
	got := stripHTML("<div><p>Hi <b>there</b></p><script>alert(1)</script>&amp; bye</div>")
	if strings.Contains(got, "<") || strings.Contains(got, "alert") {
		t.Errorf("stripHTML left markup/script: %q", got)
	}
	if !strings.Contains(got, "Hi there") || !strings.Contains(got, "& bye") {
		t.Errorf("stripHTML lost text: %q", got)
	}
}

// buildConfig builds an sdk.Config pointed at baseURL for the given tenant.
func buildConfig(t *testing.T, baseURL, tenant string) sdk.Config {
	t.Helper()
	tcx, err := tenancy.FromClaims(map[string]any{"tenant_id": tenant, "sub": "user-test"})
	if err != nil {
		t.Fatalf("tenancy.FromClaims(%q): %v", tenant, err)
	}
	raw, err := json.Marshal(map[string]string{"base_url": baseURL})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return sdk.Config{
		Tenant:     tcx,
		InstanceID: "inst-test",
		ConfigJSON: raw,
		Token:      []byte("decrypted-graph-token"),
		Checkpoint: sdk.NopCheckpoint,
	}
}
