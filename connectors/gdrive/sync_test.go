package gdrive

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
	"github.com/asker/asker/platform/tenancy"
)

// testTenant builds a tenancy.Context for tests.
func testTenant(t *testing.T) tenancy.Context {
	t.Helper()
	tcx, err := tenancy.FromClaims(map[string]any{"tenant_id": "tenant-x", "sub": "user-1"})
	if err != nil {
		t.Fatalf("tenancy.FromClaims: %v", err)
	}
	return tcx
}

// configFor builds an sdk.Config pointing base_url at baseURL.
func configFor(t *testing.T, baseURL string) sdk.Config {
	t.Helper()
	return sdk.Config{
		Tenant:     testTenant(t),
		InstanceID: "inst-1",
		ConfigJSON: []byte(`{"base_url":"` + baseURL + `"}`),
		Token:      []byte("test-token"),
		Checkpoint: sdk.NopCheckpoint,
	}
}

// TestValidate exercises the credential round-trip against a fake server.
func TestValidate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newTestConnector()

	t.Run("ok with token", func(t *testing.T) {
		t.Parallel()
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer test-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"files":[],"incompleteSearch":false}`))
		}))
		defer ts.Close()
		if err := c.Validate(ctx, configFor(t, ts.URL)); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("bad token", func(t *testing.T) {
		t.Parallel()
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":401,"message":"Invalid Credentials"}}`))
		}))
		defer ts.Close()
		cfg := configFor(t, ts.URL)
		err := c.Validate(ctx, cfg)
		if err == nil {
			t.Fatal("Validate accepted an invalid token")
		}
		if strings.Contains(err.Error(), "test-token") {
			t.Errorf("Validate error leaked the token: %v", err)
		}
	})

	t.Run("no token skips round-trip", func(t *testing.T) {
		t.Parallel()
		cfg := configFor(t, "https://drive.example.com")
		cfg.Token = nil
		if err := c.Validate(ctx, cfg); err != nil {
			t.Fatalf("Validate without token: %v", err)
		}
	})

	t.Run("config error", func(t *testing.T) {
		t.Parallel()
		cfg := sdk.Config{ConfigJSON: []byte(`{"base_url":"::bad"}`), Checkpoint: sdk.NopCheckpoint}
		if err := c.Validate(ctx, cfg); err == nil {
			t.Fatal("Validate accepted a bad base_url")
		}
	})
}

// TestBackfillSkipsVanishedFile drives FullSync against a fake server where a
// listed file 404s on its permission fetch: the backfill must skip it (the
// first incremental pass reconciles the deletion) and still emit the others.
func TestBackfillSkipsVanishedFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newTestConnector()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/changes/startPageToken":
			_, _ = w.Write([]byte(`{"startPageToken":"500"}`))
		case r.URL.Path == "/files" && r.URL.Query().Get("pageToken") == "":
			_, _ = w.Write([]byte(`{"files":[` +
				`{"id":"gone","name":"vanishing","mimeType":"application/pdf","version":"1"},` +
				`{"id":"keep","name":"kept","mimeType":"application/pdf","version":"2"}` +
				`],"incompleteSearch":false}`))
		case r.URL.Path == "/files/gone/permissions":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"File not found"}}`))
		case r.URL.Path == "/files/keep/permissions":
			_, _ = w.Write([]byte(`{"permissions":[{"type":"user","role":"owner","emailAddress":"a@example.com"}]}`))
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	var rec connectortest.EmitRecorder
	cfg := configFor(t, ts.URL)
	cur, err := c.FullSync(ctx, cfg, rec.Emit)
	if err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if cur != sdk.Cursor("page:500") {
		t.Errorf("cursor = %q, want page:500", cur)
	}
	docs := rec.Docs()
	if len(docs) != 1 {
		t.Fatalf("emitted %d docs, want 1 (the vanished file is skipped)", len(docs))
	}
	if docs[0].GetSourceNativeId() != "keep" {
		t.Errorf("emitted %q, want keep", docs[0].GetSourceNativeId())
	}
	for _, d := range docs {
		connectortest.ValidateDocument(t, cfg, d)
	}
}

// TestIncrementalRefetchesChangeWithoutFile drives IncrementalSync where a
// change entry omits the embedded file, forcing a files.get refresh, and a
// second change whose file is trashed becomes a tombstone.
func TestIncrementalRefetchesChangeWithoutFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newTestConnector()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/changes":
			_, _ = w.Write([]byte(`{"changes":[` +
				`{"changeType":"file","fileId":"refresh-me","removed":false,"time":"2026-06-11T00:00:00Z"},` +
				`{"changeType":"file","fileId":"trashed-one","removed":false,"time":"2026-06-11T01:00:00Z","file":{"id":"trashed-one","name":"t","mimeType":"application/pdf","version":"9","trashed":true}},` +
				`{"changeType":"drive","fileId":"","removed":false}` +
				`],"newStartPageToken":"600"}`))
		case "/files/refresh-me":
			_, _ = w.Write([]byte(`{"id":"refresh-me","name":"Refreshed","mimeType":"application/pdf","version":"4","trashed":false,"owners":[{"emailAddress":"a@example.com"}]}`))
		case "/files/refresh-me/permissions":
			_, _ = w.Write([]byte(`{"permissions":[{"type":"user","role":"owner","emailAddress":"a@example.com"}]}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	var rec connectortest.EmitRecorder
	cfg := configFor(t, ts.URL)
	cur, err := c.IncrementalSync(ctx, cfg, sdk.Cursor("page:550"), rec.Emit)
	if err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	if cur != sdk.Cursor("page:600") {
		t.Errorf("cursor = %q, want page:600", cur)
	}
	docs := rec.Docs()
	if len(docs) != 2 {
		t.Fatalf("emitted %d docs, want 2", len(docs))
	}
	var live, tomb int
	for _, d := range docs {
		connectortest.ValidateDocument(t, cfg, d)
		if d.GetTombstone().GetDeleted() {
			tomb++
			if d.GetSourceNativeId() != "trashed-one" {
				t.Errorf("tombstone for %q, want trashed-one", d.GetSourceNativeId())
			}
		} else {
			live++
			if d.GetSourceNativeId() != "refresh-me" {
				t.Errorf("live doc for %q, want refresh-me", d.GetSourceNativeId())
			}
		}
	}
	if live != 1 || tomb != 1 {
		t.Errorf("live=%d tomb=%d, want 1 and 1", live, tomb)
	}
}

// TestIncrementalResumesBackfill confirms a mid-backfill checkpoint cursor
// resumes files.list at the saved page token rather than restarting.
func TestIncrementalResumesBackfill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newTestConnector()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/files" && r.URL.Query().Get("pageToken") == "RESUME":
			_, _ = w.Write([]byte(`{"files":[{"id":"resumed","name":"r","mimeType":"application/pdf","version":"1","owners":[{"emailAddress":"a@example.com"}]}],"incompleteSearch":false}`))
		case r.URL.Path == "/files/resumed/permissions":
			_, _ = w.Write([]byte(`{"permissions":[{"type":"user","role":"owner","emailAddress":"a@example.com"}]}`))
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	var rec connectortest.EmitRecorder
	cfg := configFor(t, ts.URL)
	cur, err := c.IncrementalSync(ctx, cfg, backfillCursor("900", "RESUME"), rec.Emit)
	if err != nil {
		t.Fatalf("IncrementalSync(resume): %v", err)
	}
	if cur != sdk.Cursor("page:900") {
		t.Errorf("cursor = %q, want page:900 (the captured start token)", cur)
	}
	if docs := rec.Docs(); len(docs) != 1 || docs[0].GetSourceNativeId() != "resumed" {
		t.Fatalf("docs = %v, want a single resumed file", docs)
	}
}

// TestIncrementalBadCursor confirms an unparseable cursor maps to
// ErrCursorExpired without any HTTP call.
func TestIncrementalBadCursor(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	var rec connectortest.EmitRecorder
	cfg := configFor(t, "https://drive.example.com")
	_, err := c.IncrementalSync(context.Background(), cfg, sdk.Cursor("garbage"), rec.Emit)
	if !errors.Is(err, sdk.ErrCursorExpired) {
		t.Fatalf("IncrementalSync(garbage) = %v, want ErrCursorExpired", err)
	}
}

// TestBodyExtractionDegradesToMetadata confirms a failing export still yields a
// searchable metadata-only document (body empty), and that permissions.list
// pagination is followed.
func TestBodyExtractionDegradesToMetadata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newTestConnector()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/changes/startPageToken":
			_, _ = w.Write([]byte(`{"startPageToken":"1"}`))
		case r.URL.Path == "/files" && r.URL.Query().Get("pageToken") == "":
			_, _ = w.Write([]byte(`{"files":[{"id":"gdoc","name":"Doc","mimeType":"application/vnd.google-apps.document","version":"1","owners":[{"emailAddress":"a@example.com"}]}],"incompleteSearch":false}`))
		case r.URL.Path == "/files/gdoc/permissions" && r.URL.Query().Get("pageToken") == "":
			// First permissions page points to a second page.
			_, _ = w.Write([]byte(`{"permissions":[{"type":"user","role":"owner","emailAddress":"a@example.com"}],"nextPageToken":"P2"}`))
		case r.URL.Path == "/files/gdoc/permissions" && r.URL.Query().Get("pageToken") == "P2":
			_, _ = w.Write([]byte(`{"permissions":[{"type":"anyone","role":"reader"}]}`))
		case r.URL.Path == "/files/gdoc/export":
			// Export fails: body extraction degrades to metadata-only.
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":500,"message":"export failed"}}`))
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	var rec connectortest.EmitRecorder
	cfg := configFor(t, ts.URL)
	if _, err := c.FullSync(ctx, cfg, rec.Emit); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	docs := rec.Docs()
	if len(docs) != 1 {
		t.Fatalf("emitted %d docs, want 1", len(docs))
	}
	d := docs[0]
	connectortest.ValidateDocument(t, cfg, d)
	if d.GetBodyText() != "" {
		t.Errorf("body = %q, want empty after export failure", d.GetBodyText())
	}
	// The second permissions page (anyone) makes the file public, so not private.
	if d.GetAcl().GetIsPrivate() {
		t.Error("is_private = true, want false (anyone permission from page 2)")
	}
}

// TestStaleStartTokenInFullSync confirms a failing changes.getStartPageToken
// fails FullSync (rather than emitting a partial backfill with no cursor).
func TestStaleStartTokenInFullSync(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()
	var rec connectortest.EmitRecorder
	if _, err := c.FullSync(context.Background(), configFor(t, ts.URL), rec.Emit); err == nil {
		t.Fatal("FullSync succeeded despite a failing start-token call")
	}
}
