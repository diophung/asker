package outlookmail

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

const (
	testTenant = "tenant-a"
	testToken  = "decrypted-graph-token"
)

func newTestConnector() sdk.Connector {
	return New(WithLogger(slog.New(slog.DiscardHandler)))
}

func testTenancy(t *testing.T) tenancy.Context {
	t.Helper()
	tcx, err := tenancy.FromClaims(map[string]any{"tenant_id": testTenant, "sub": "user-test"})
	if err != nil {
		t.Fatalf("tenancy.FromClaims: %v", err)
	}
	return tcx
}

// config builds an sdk.Config pointing base_url at baseURL.
func config(t *testing.T, baseURL, token string) sdk.Config {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"base_url":            baseURL,
		"user_principal_name": "alice@contoso.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	return sdk.Config{
		Tenant:     testTenancy(t),
		InstanceID: "inst-test",
		ConfigJSON: raw,
		Token:      []byte(token),
		Checkpoint: sdk.NopCheckpoint,
	}
}

func TestSpec(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	connectortest.RunSpecChecks(t, c)

	spec := c.Spec()
	if spec.ID != "outlook-mail" {
		t.Errorf("spec.ID = %q, want outlook-mail", spec.ID)
	}
	if spec.AuthType != sdk.AuthOAuth2 {
		t.Errorf("spec.AuthType = %v, want AuthOAuth2", spec.AuthType)
	}
	if spec.SupportsWebhook {
		t.Error("spec.SupportsWebhook = true, want false (no push path this milestone)")
	}
	if !json.Valid(spec.ConfigSchema) {
		t.Error("ConfigSchema is not valid JSON")
	}
}

// TestFullSyncGolden checks the full field mapping of known messages, including
// HTML body stripping, participants with roles, metadata, timestamps, and the
// version_etag from @odata.etag.
func TestFullSyncGolden(t *testing.T) {
	t.Parallel()
	cfg := config(t, "", testToken) // base_url overridden below
	rs := newReplayServer(t, "testdata/fullsync.json")
	cfg.ConfigJSON = withBaseURL(t, cfg.ConfigJSON, rs.URL())

	var rec connectortest.EmitRecorder
	cur, err := newTestConnector().FullSync(context.Background(), cfg, rec.Emit)
	if err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if want := sdk.Cursor(deltaTokenPrefix + "DELTA_TOKEN_AFTER_BACKFILL"); cur != want {
		t.Errorf("cursor = %q, want %q", cur, want)
	}

	docs := rec.Docs()
	if len(docs) != 3 {
		t.Fatalf("emitted %d documents, want 3", len(docs))
	}
	byID := map[string]*askerv1.Document{}
	for _, d := range docs {
		connectortest.ValidateDocument(t, cfg, d)
		if d.GetType() != askerv1.DocType_EMAIL {
			t.Errorf("doc %s type = %v, want EMAIL", d.GetSourceNativeId(), d.GetType())
		}
		byID[d.GetSourceNativeId()] = d
	}

	// msg1: HTML body stripped to text, both block paragraphs preserved.
	m1 := byID["AAMkADmsg1"]
	if m1 == nil {
		t.Fatal("msg1 not emitted")
	}
	if m1.GetTitle() != "Welcome to the team" {
		t.Errorf("msg1 title = %q", m1.GetTitle())
	}
	if want := "Glad to have you aboard.\nSee you Monday."; m1.GetBodyText() != want {
		t.Errorf("msg1 body = %q, want %q", m1.GetBodyText(), want)
	}
	if strings.Contains(m1.GetBodyText(), "color:red") {
		t.Error("msg1 body leaked <style> contents")
	}
	if got, want := m1.GetVersionEtag(), `W/"CQAAABYAAACabc1"`; got != want {
		t.Errorf("msg1 version_etag = %q, want %q", got, want)
	}
	wantParts := []*askerv1.Participant{
		{Name: "Dana Reed", Email: "dana@contoso.com", Role: "sender"},
		{Name: "Alice Example", Email: "alice@contoso.com", Role: "recipient"},
		{Name: "HR", Email: "hr@contoso.com", Role: "recipient"},
	}
	gotParts := m1.GetParticipants()
	if len(gotParts) != len(wantParts) {
		t.Fatalf("msg1 participants = %v, want %v", gotParts, wantParts)
	}
	for i, w := range wantParts {
		g := gotParts[i]
		if g.GetName() != w.GetName() || g.GetEmail() != w.GetEmail() || g.GetRole() != w.GetRole() {
			t.Errorf("participant[%d] = %v, want %v", i, g, w)
		}
	}
	wantMeta := map[string]string{
		"message_id":      "AAMkADmsg1",
		"conversation_id": "conv-1",
		"web_link":        "https://outlook.office365.com/owa/?ItemID=AAMkADmsg1",
		"folder":          "AAMkInbox",
	}
	for k, w := range wantMeta {
		if m1.GetMetadata()[k] != w {
			t.Errorf("msg1 metadata[%q] = %q, want %q", k, m1.GetMetadata()[k], w)
		}
	}
	if got, want := m1.GetTs().GetCreated().AsTime(), time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("msg1 ts.created = %v, want %v", got, want)
	}
	if m1.GetTs().GetIngested() != nil {
		t.Error("msg1 ts.ingested set; the hub stamps it")
	}

	// msg3: empty subject -> placeholder title.
	m3 := byID["AAMkADmsg3"]
	if m3 == nil || m3.GetTitle() != noSubjectTitle {
		t.Errorf("msg3 title = %q, want %q", m3.GetTitle(), noSubjectTitle)
	}
}

// TestIncrementalChangeAndDelete verifies the edit yields an upsert with a new
// etag and the removal yields a tombstone with no body, across two delta pages.
func TestIncrementalChangeAndDelete(t *testing.T) {
	t.Parallel()
	rs := newReplayServer(t, "testdata/incremental.json")
	cfg := config(t, rs.URL(), testToken)

	var rec connectortest.EmitRecorder
	cur, err := newTestConnector().IncrementalSync(
		context.Background(), cfg, sdk.Cursor(deltaTokenPrefix+"DELTA_TOKEN_AFTER_BACKFILL"), rec.Emit)
	if err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	if want := sdk.Cursor(deltaTokenPrefix + "DELTA_TOKEN_NEXT"); cur != want {
		t.Errorf("cursor = %q, want %q", cur, want)
	}

	docs := rec.Docs()
	if len(docs) != 2 {
		t.Fatalf("emitted %d documents, want 2 (1 edit, 1 tombstone)", len(docs))
	}
	for _, d := range docs {
		connectortest.ValidateDocument(t, cfg, d)
	}

	edit, tomb := docs[0], docs[1]
	if edit.GetSourceNativeId() != "AAMkADmsg1" || edit.GetTombstone().GetDeleted() {
		t.Errorf("docs[0] = (%s, tombstone=%v), want upsert of msg1", edit.GetSourceNativeId(), edit.GetTombstone().GetDeleted())
	}
	if edit.GetTitle() != "Welcome to the team (updated)" {
		t.Errorf("edited title = %q", edit.GetTitle())
	}
	if got, want := edit.GetVersionEtag(), `W/"CQAAABYAAACabc1v2"`; got != want {
		t.Errorf("edited version_etag = %q, want %q", got, want)
	}
	if got, want := edit.GetTs().GetModified().AsTime(), time.Date(2026, 6, 5, 14, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("edited ts.modified = %v, want %v", got, want)
	}

	if tomb.GetSourceNativeId() != "AAMkADmsg2" || !tomb.GetTombstone().GetDeleted() {
		t.Fatalf("docs[1] = (%s, tombstone=%v), want tombstone of msg2", tomb.GetSourceNativeId(), tomb.GetTombstone().GetDeleted())
	}
	if tomb.GetBodyText() != "" || len(tomb.GetChunks()) != 0 {
		t.Error("tombstone carries a body")
	}
	if tomb.GetTombstone().GetDeletedAt() == nil {
		t.Error("tombstone deleted_at unset")
	}
	if tomb.GetVersionEtag() == "" {
		t.Error("tombstone version_etag empty")
	}
}

// TestIncrementalStaleCursor: a 410 Gone surfaces as sdk.ErrCursorExpired.
func TestIncrementalStaleCursor(t *testing.T) {
	t.Parallel()
	rs := newReplayServer(t, "testdata/stale_cursor.json")
	cfg := config(t, rs.URL(), testToken)
	_, err := newTestConnector().IncrementalSync(
		context.Background(), cfg, sdk.Cursor(deltaTokenPrefix+"EXPIRED_TOKEN"), nopEmit)
	if !isCursorExpired(err) {
		t.Fatalf("error = %v, want sdk.ErrCursorExpired", err)
	}
}

// TestIncrementalUnreadableCursor: cursors this connector never produced (and
// the empty cursor) recover via sdk.ErrCursorExpired.
func TestIncrementalUnreadableCursor(t *testing.T) {
	t.Parallel()
	// No HTTP should be issued for an unreadable cursor, so an empty replay
	// server (no interactions) must see no requests.
	rs := newReplayServer(t, "testdata/stale_cursor.json")
	cfg := config(t, rs.URL(), testToken)
	for _, cur := range []sdk.Cursor{"", "bogus", "delta:", "https://x/y"} {
		_, err := newTestConnector().IncrementalSync(context.Background(), cfg, cur, nopEmit)
		if !isCursorExpired(err) {
			t.Errorf("cursor %q error = %v, want sdk.ErrCursorExpired", cur, err)
		}
	}
}

// TestResumeBackfillCursor: the hub replaying a mid-backfill list-page checkpoint
// into IncrementalSync resumes the backfill, then establishes the delta cursor.
func TestResumeBackfillCursor(t *testing.T) {
	t.Parallel()
	rs := newReplayServer(t, "testdata/fullsync.json")
	cfg := config(t, rs.URL(), testToken)

	// The page-1 nextLink (rebased to the replay server) is the checkpoint the
	// hub would replay; it carries $skip=50 so it serves cassette page 2.
	resume := sdk.Cursor(rs.URL() + "/me/messages?$select=" +
		"id,subject,body,bodyPreview,from,toRecipients,ccRecipients,webLink,parentFolderId,conversationId,createdDateTime,lastModifiedDateTime" +
		"&$top=50&$skip=50")

	var rec connectortest.EmitRecorder
	cur, err := newTestConnector().IncrementalSync(context.Background(), cfg, resume, rec.Emit)
	if err != nil {
		t.Fatalf("IncrementalSync (resume): %v", err)
	}
	if want := sdk.Cursor(deltaTokenPrefix + "DELTA_TOKEN_AFTER_BACKFILL"); cur != want {
		t.Errorf("resume cursor = %q, want %q", cur, want)
	}
	// Only page 2 (msg3) should be emitted by the resume.
	docs := rec.Docs()
	if len(docs) != 1 || docs[0].GetSourceNativeId() != "AAMkADmsg3" {
		t.Fatalf("resume emitted %d docs (%v), want only msg3", len(docs), idsOf(docs))
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newTestConnector()

	t.Run("ok with token", func(t *testing.T) {
		t.Parallel()
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+testToken {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":[{"id":"x"}]}`))
		}))
		t.Cleanup(ts.Close)
		if err := c.Validate(ctx, config(t, ts.URL, testToken)); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("bad token rejected", func(t *testing.T) {
		t.Parallel()
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"InvalidAuthenticationToken"}}`))
		}))
		t.Cleanup(ts.Close)
		err := c.Validate(ctx, config(t, ts.URL, "wrong"))
		if err == nil {
			t.Fatal("Validate accepted a rejected token")
		}
		if strings.Contains(err.Error(), "wrong") {
			t.Errorf("Validate error leaked the token: %v", err)
		}
	})

	t.Run("no token skips round-trip", func(t *testing.T) {
		t.Parallel()
		cfg := config(t, "https://graph.microsoft.com/v1.0", "")
		cfg.Token = nil
		if err := c.Validate(ctx, cfg); err != nil {
			t.Fatalf("Validate without token: %v", err)
		}
	})

	t.Run("bad config", func(t *testing.T) {
		t.Parallel()
		for name, raw := range map[string]string{
			"not json":     "{",
			"bad base_url": `{"base_url":"::not-a-url"}`,
			"ftp base_url": `{"base_url":"ftp://host"}`,
		} {
			cfg := sdk.Config{Tenant: testTenancy(t), ConfigJSON: []byte(raw), Checkpoint: sdk.NopCheckpoint}
			if err := c.Validate(ctx, cfg); err == nil {
				t.Errorf("Validate accepted %s config %q", name, raw)
			}
		}
	})

	t.Run("empty config is valid (base_url defaults to graph)", func(t *testing.T) {
		t.Parallel()
		cfg := sdk.Config{Tenant: testTenancy(t), ConfigJSON: nil, Checkpoint: sdk.NopCheckpoint}
		if err := c.Validate(ctx, cfg); err != nil {
			t.Fatalf("Validate empty config: %v", err)
		}
	})
}

func TestHandleWebhookUnsupported(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{}`))
	err := newTestConnector().HandleWebhook(context.Background(), config(t, "", testToken), req, nopEmit)
	if !isWebhookUnsupported(err) {
		t.Fatalf("HandleWebhook = %v, want sdk.ErrWebhookUnsupported", err)
	}
}

// TestStripHTML exercises the HTML-to-text fallback edge cases directly.
func TestStripHTML(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ in, want string }{
		"entities":         {"<p>Tom &amp; Jerry</p>", "Tom & Jerry"},
		"script dropped":   {"<div>keep</div><script>alert(1)</script><div>this</div>", "keep\nthis"},
		"unterminated tag": {"hello <b world", "hello"},
		"plain":            {"just text", "just text"},
	}
	for name, tc := range cases {
		if got := stripHTML(tc.in); got != tc.want {
			t.Errorf("%s: stripHTML(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

// TestBodyFallbackToPreview: when no body part is present, bodyPreview is used.
func TestBodyFallbackToPreview(t *testing.T) {
	t.Parallel()
	m := &graphMessage{ID: "x", BodyPreview: "preview text"}
	if got := messageBody(m); got != "preview text" {
		t.Errorf("body = %q, want preview text", got)
	}
	// version_etag falls back to sha256 of the body when no source etag.
	doc := messageDocument("t", m)
	if len(doc.GetVersionEtag()) != 64 {
		t.Errorf("expected sha256 hex etag fallback, got %q", doc.GetVersionEtag())
	}
}

// --- helpers ---

func nopEmit(context.Context, *askerv1.Document) error { return nil }

func isCursorExpired(err error) bool { return errors.Is(err, sdk.ErrCursorExpired) }

func isWebhookUnsupported(err error) bool { return errors.Is(err, sdk.ErrWebhookUnsupported) }

func idsOf(docs []*askerv1.Document) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.GetSourceNativeId()
	}
	return out
}

func newReplayServer(t *testing.T, cassette string) *connectortest.ReplayServer {
	t.Helper()
	cas, err := connectortest.LoadCassette(cassette)
	if err != nil {
		t.Fatalf("LoadCassette: %v", err)
	}
	return connectortest.NewReplayServer(t, cas)
}

func withBaseURL(t *testing.T, raw json.RawMessage, baseURL string) json.RawMessage {
	t.Helper()
	obj := map[string]any{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	obj["base_url"] = baseURL
	out, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
