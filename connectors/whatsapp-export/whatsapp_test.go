package whatsappexport

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

func mustTenant(t *testing.T) tenancy.Context {
	t.Helper()
	tc, err := tenancy.FromClaims(map[string]any{"tenant_id": "tenant-a", "sub": "user-a"})
	if err != nil {
		t.Fatalf("tenancy.FromClaims: %v", err)
	}
	return tc
}

func buildCfg(t *testing.T, exportURL string) sdk.Config {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"chat_name": testChat, "export_url": exportURL})
	return sdk.Config{
		Tenant:     mustTenant(t),
		InstanceID: "inst-a",
		ConfigJSON: body,
		Checkpoint: sdk.NopCheckpoint,
	}
}

func discardEmit(_ context.Context, _ *askerv1.Document) error { return nil }

const testChat = "Project Falcon"

func newTestConnector() sdk.Connector {
	return New(WithLogger(slog.New(slog.DiscardHandler)))
}

// did derives the expected doc_id for a parsed message in testChat exactly as
// the connector does (sdk.DocID over the position-independent native id, which
// folds in the message's timestamp, sender, text, and duplicate-occurrence index
// but NOT its line index).
func did(m message) string {
	return sdk.DocID(connectorID, nativeID(testChat,
		m.rawSentAt, strings.ToValidUTF8(m.sender, ""), strings.ToValidUTF8(m.text, ""), m.occurrence))
}

// The six messages of the bracket FullSync cassette, in order. Index 0 and 4
// are system lines (still emitted, no participant).
var (
	mEncrypted = "Messages and calls are end-to-end encrypted. No one outside of this chat, not even WhatsApp, can read or listen to them."
	mKickoff   = "Hey team \U0001F44B kicking off Project Falcon today!"
	mSync      = "Sounds great.\nCan we sync at 3:30?\nI have notes to share."
	mMedia     = "<Media omitted>"
	mAdded     = "Carol added Dave"
	mCafe      = "Café au lait ☕ done — shipping now"
	// incremental_append tail:
	mStandup = "Morning! Falcon standup in 5."
	mOnWay   = "On my way \U0001F680"
)

func fullSyncDocIDs() []string {
	six := parseSix()
	ids := make([]string, len(six))
	for i := range six {
		ids[i] = did(six[i])
	}
	return ids
}

func TestSpec(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	connectortest.RunSpecChecks(t, c)

	spec := c.Spec()
	if spec.ID != "whatsapp-export" {
		t.Errorf("spec.ID = %q, want whatsapp-export", spec.ID)
	}
	if spec.AuthType != sdk.AuthNone {
		t.Errorf("spec.AuthType = %v, want AuthNone", spec.AuthType)
	}
	if spec.SupportsWebhook {
		t.Error("spec.SupportsWebhook = true, want false")
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newTestConnector()

	t.Run("empty config ok", func(t *testing.T) {
		t.Parallel()
		if err := c.Validate(ctx, sdk.Config{ConfigJSON: []byte(`{}`), Checkpoint: sdk.NopCheckpoint}); err != nil {
			t.Fatalf("Validate empty: %v", err)
		}
	})
	t.Run("good url ok", func(t *testing.T) {
		t.Parallel()
		cfg := sdk.Config{ConfigJSON: []byte(`{"export_url":"https://example.test/_chat.txt","chat_name":"Falcon"}`), Checkpoint: sdk.NopCheckpoint}
		if err := c.Validate(ctx, cfg); err != nil {
			t.Fatalf("Validate good url: %v", err)
		}
	})
	t.Run("bad url rejected", func(t *testing.T) {
		t.Parallel()
		cfg := sdk.Config{ConfigJSON: []byte(`{"export_url":"::not-a-url"}`), Checkpoint: sdk.NopCheckpoint}
		if err := c.Validate(ctx, cfg); err == nil {
			t.Fatal("Validate accepted a malformed export_url")
		}
	})
	t.Run("non-json rejected", func(t *testing.T) {
		t.Parallel()
		cfg := sdk.Config{ConfigJSON: []byte(`{`), Checkpoint: sdk.NopCheckpoint}
		if err := c.Validate(ctx, cfg); err == nil {
			t.Fatal("Validate accepted non-JSON config")
		}
	})
}

func TestContractFullSync(t *testing.T) {
	t.Parallel()
	body, _ := json.Marshal(map[string]string{"chat_name": testChat})
	wantCursor := makeCursor(parseSix(), 6)
	connectortest.RunConnectorContract(t, newTestConnector(), connectortest.ContractCase{
		Name:         "fullsync",
		Cassette:     "testdata/fullsync.json",
		BaseURLField: "export_url",
		ConfigJSON:   body,
		FullSync: &connectortest.SyncExpectation{
			WantDocIDs: fullSyncDocIDs(),
			WantCursor: &wantCursor,
		},
	})
}

func TestContractIncrementalAppend(t *testing.T) {
	t.Parallel()
	body, _ := json.Marshal(map[string]string{"chat_name": testChat})
	fromCursor := makeCursor(parseSix(), 6)
	wantCursor := makeCursor(parseEight(), 8)
	connectortest.RunConnectorContract(t, newTestConnector(), connectortest.ContractCase{
		Name:         "incremental-append",
		Cassette:     "testdata/incremental_append.json",
		BaseURLField: "export_url",
		ConfigJSON:   body,
		Incremental: &connectortest.IncrementalExpectation{
			FromCursor: fromCursor,
			SyncExpectation: connectortest.SyncExpectation{
				WantDocIDs: func() []string { eight := parseEight(); return []string{did(eight[6]), did(eight[7])} }(),
				WantCursor: &wantCursor,
			},
		},
	})
}

func TestContractIncrementalNoop(t *testing.T) {
	t.Parallel()
	body, _ := json.Marshal(map[string]string{"chat_name": testChat})
	fromCursor := makeCursor(parseSix(), 6)
	wantCursor := fromCursor // unchanged
	connectortest.RunConnectorContract(t, newTestConnector(), connectortest.ContractCase{
		Name:         "incremental-noop",
		Cassette:     "testdata/incremental_noop.json",
		BaseURLField: "export_url",
		ConfigJSON:   body,
		Incremental: &connectortest.IncrementalExpectation{
			FromCursor: fromCursor,
			SyncExpectation: connectortest.SyncExpectation{
				WantCount:  0,
				WantCursor: &wantCursor,
			},
		},
	})
}

func TestContractChangedPrefixExpires(t *testing.T) {
	t.Parallel()
	body, _ := json.Marshal(map[string]string{"chat_name": testChat})
	fromCursor := makeCursor(parseSix(), 6)
	wantErr := sdk.ErrCursorExpired
	connectortest.RunConnectorContract(t, newTestConnector(), connectortest.ContractCase{
		Name:         "changed-prefix",
		Cassette:     "testdata/changed_prefix.json",
		BaseURLField: "export_url",
		ConfigJSON:   body,
		Incremental: &connectortest.IncrementalExpectation{
			FromCursor:      fromCursor,
			SyncExpectation: connectortest.SyncExpectation{WantErr: &wantErr},
		},
	})
}

func TestContractUnparseableCursorExpires(t *testing.T) {
	t.Parallel()
	body, _ := json.Marshal(map[string]string{"chat_name": testChat})
	wantErr := sdk.ErrCursorExpired
	connectortest.RunConnectorContract(t, newTestConnector(), connectortest.ContractCase{
		Name:         "garbage-cursor",
		Cassette:     "testdata/incremental_noop.json", // no HTTP call: cursor rejected first
		BaseURLField: "export_url",
		ConfigJSON:   body,
		Incremental: &connectortest.IncrementalExpectation{
			FromCursor:      sdk.Cursor("not-a-whatsapp-cursor"),
			SyncExpectation: connectortest.SyncExpectation{WantErr: &wantErr},
		},
	})
}

func TestContractDashFormat(t *testing.T) {
	t.Parallel()
	body, _ := json.Marshal(map[string]string{"chat_name": testChat})
	// 3 messages: system (idx0), Alice (idx1), Bob multi-line (idx2). The dash
	// timestamps "3/15/24, 09:0X" are the raw header text the dash cassette
	// carries (see testdata/dash_fullsync.json); did folds them into the id.
	dash := buildMsgs([]struct {
		sender, text, raw string
		system            bool
	}{
		{"", "Messages and calls are end-to-end encrypted.", "3/15/24, 09:00", true},
		{"Alice", "hello from the dash format", "3/15/24, 09:42", false},
		{"Bob", "line one\nline two continues here", "3/15/24, 09:43", false},
	})
	connectortest.RunConnectorContract(t, newTestConnector(), connectortest.ContractCase{
		Name:         "dash-fullsync",
		Cassette:     "testdata/dash_fullsync.json",
		BaseURLField: "export_url",
		ConfigJSON:   body,
		FullSync: &connectortest.SyncExpectation{
			WantDocIDs: []string{did(dash[0]), did(dash[1]), did(dash[2])},
		},
	})
}

func TestFullSyncContentAndOrder(t *testing.T) {
	t.Parallel()
	cas, err := connectortest.LoadCassette("testdata/fullsync.json")
	if err != nil {
		t.Fatalf("LoadCassette: %v", err)
	}
	rs := connectortest.NewReplayServer(t, cas)
	cfg := buildCfg(t, rs.URL())

	var rec connectortest.EmitRecorder
	cur, err := newTestConnector().FullSync(context.Background(), cfg, rec.Emit)
	if err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	docs := rec.Docs()
	if len(docs) != 6 {
		t.Fatalf("emitted %d docs, want 6", len(docs))
	}
	// Order preserved: emission order is file order.
	if docs[1].GetBodyText() != mKickoff {
		t.Errorf("doc[1] body = %q, want kickoff", docs[1].GetBodyText())
	}
	// Multi-line body is joined with newlines.
	if docs[2].GetBodyText() != mSync {
		t.Errorf("doc[2] body = %q, want the 3-line sync message", docs[2].GetBodyText())
	}
	// System line (idx 4) is emitted with no participant.
	if len(docs[4].GetParticipants()) != 0 {
		t.Errorf("doc[4] (system) has participants: %+v", docs[4].GetParticipants())
	}
	// Every doc validates against the contract.
	for _, d := range docs {
		connectortest.ValidateDocument(t, cfg, d)
	}
	if want := makeCursor(parseSix(), 6); cur != want {
		t.Errorf("cursor = %q, want %q", cur, want)
	}
}

func TestFullSyncNoURL(t *testing.T) {
	t.Parallel()
	cfg := sdk.Config{
		Tenant:     mustTenant(t),
		ConfigJSON: []byte(`{}`),
		Checkpoint: sdk.NopCheckpoint,
	}
	if _, err := newTestConnector().FullSync(context.Background(), cfg, discardEmit); err == nil {
		t.Fatal("FullSync succeeded with no export_url")
	}
}

func TestFullSyncHTTPStatusError(t *testing.T) {
	t.Parallel()
	cas := &connectortest.Cassette{Interactions: []*connectortest.Interaction{{
		Request:  connectortest.RecordedRequest{Method: "GET", Path: "/"},
		Response: connectortest.RecordedResponse{Status: 404, Body: "nope"},
	}}}
	rs := connectortest.NewReplayServer(t, cas)
	cfg := buildCfg(t, rs.URL())
	if _, err := newTestConnector().FullSync(context.Background(), cfg, discardEmit); err == nil {
		t.Fatal("FullSync succeeded on a 404 export URL")
	}
}

func TestWebhookUnsupported(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	var rec connectortest.EmitRecorder
	err := c.HandleWebhook(context.Background(), sdk.Config{ConfigJSON: []byte(`{}`), Checkpoint: sdk.NopCheckpoint}, nil, rec.Emit)
	if !errors.Is(err, sdk.ErrWebhookUnsupported) {
		t.Errorf("HandleWebhook error = %v, want sdk.ErrWebhookUnsupported", err)
	}
	if n := len(rec.Docs()); n != 0 {
		t.Errorf("HandleWebhook emitted %d docs, want 0", n)
	}
}

// parseSix / parseEight reconstruct the message slices the cassettes encode so
// the test computes the SAME cursors the connector does (cursor = count + hash
// of per-message etags). They mirror the cassette bodies exactly.
func parseSix() []message {
	return buildMsgs([]struct {
		sender, text, raw string
		system            bool
	}{
		{"", mEncrypted, "2024-03-15, 9:00:00 AM", true},
		{"Alice", mKickoff, "2024-03-15, 9:42:13 AM", false},
		{"Bob", mSync, "2024-03-15, 9:43:01 AM", false},
		{"Bob", mMedia, "2024-03-15, 9:45:10 AM", false},
		{"", mAdded, "2024-03-15, 9:46:00 AM", true},
		{"Café Owner", mCafe, "2024-03-15, 9:50:22 AM", false},
	})
}

func parseEight() []message {
	six := parseSix()
	tail := buildMsgs([]struct {
		sender, text, raw string
		system            bool
	}{
		{"Alice", mStandup, "2024-03-16, 10:00:00 AM", false},
		{"Dave", mOnWay, "2024-03-16, 10:05:30 AM", false},
	})
	tail[0].lineIndex = 6
	tail[1].lineIndex = 7
	return append(six, tail...)
}

func buildMsgs(rows []struct {
	sender, text, raw string
	system            bool
}) []message {
	out := make([]message, len(rows))
	for i, r := range rows {
		out[i] = message{sender: r.sender, text: r.text, rawSentAt: r.raw, lineIndex: i, system: r.system}
	}
	return out
}
