package gcal

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
	"github.com/asker/asker/platform/tenancy"
)

// checkpointRecorder records every checkpointed cursor.
type checkpointRecorder struct {
	mu      sync.Mutex
	cursors []sdk.Cursor
}

func (r *checkpointRecorder) Checkpoint(_ context.Context, cur sdk.Cursor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cursors = append(r.cursors, cur)
	return nil
}

func (r *checkpointRecorder) all() []sdk.Cursor {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]sdk.Cursor, len(r.cursors))
	copy(out, r.cursors)
	return out
}

// configFor builds an sdk.Config pointing at baseURL with the given checkpoint.
func configFor(t *testing.T, baseURL string, cp sdk.Checkpoint) sdk.Config {
	t.Helper()
	tc, err := tenancy.FromClaims(map[string]any{"tenant_id": "tenant-a", "sub": "user-a"})
	if err != nil {
		t.Fatalf("tenancy.FromClaims: %v", err)
	}
	raw, err := json.Marshal(map[string]string{"base_url": baseURL})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if cp == nil {
		cp = sdk.NopCheckpoint
	}
	return sdk.Config{
		Tenant:     tc,
		InstanceID: "inst-1",
		ConfigJSON: raw,
		Token:      []byte(testToken),
		Checkpoint: cp,
	}
}

func loadServer(t *testing.T, cassette string) *connectortest.ReplayServer {
	t.Helper()
	cas, err := connectortest.LoadCassette(cassette)
	if err != nil {
		t.Fatalf("LoadCassette(%s): %v", cassette, err)
	}
	return connectortest.NewReplayServer(t, cas)
}

// TestFullSyncCheckpoints proves the backfill checkpoints a resumable page
// cursor after the first page and the final sync cursor at the end.
func TestFullSyncCheckpoints(t *testing.T) {
	t.Parallel()
	rs := loadServer(t, "testdata/fullsync.json")
	c := New(WithLogger(slog.New(slog.DiscardHandler)))

	var cp checkpointRecorder
	var rec connectortest.EmitRecorder
	cfg := configFor(t, rs.URL(), cp.Checkpoint)

	cur, err := c.FullSync(context.Background(), cfg, rec.Emit)
	if err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if want := syncCursor("SYNCTOKEN-AFTER-BACKFILL"); cur != want {
		t.Errorf("cursor = %q, want %q", cur, want)
	}

	got := cp.all()
	// Page 1 has nextPageToken -> page cursor checkpoint; final page -> sync cursor.
	wantCheckpoints := []sdk.Cursor{
		pageCursor("CkgK...page2"),
		syncCursor("SYNCTOKEN-AFTER-BACKFILL"),
	}
	if len(got) != len(wantCheckpoints) {
		t.Fatalf("checkpoints = %v, want %v", got, wantCheckpoints)
	}
	for i := range wantCheckpoints {
		if got[i] != wantCheckpoints[i] {
			t.Errorf("checkpoint[%d] = %q, want %q", i, got[i], wantCheckpoints[i])
		}
	}
}

// TestIncrementalResumesBackfill proves that replaying a mid-backfill "page:"
// checkpoint into IncrementalSync resumes the backfill at that page.
func TestIncrementalResumesBackfill(t *testing.T) {
	t.Parallel()
	rs := loadServer(t, "testdata/resume_backfill.json")
	c := New(WithLogger(slog.New(slog.DiscardHandler)))

	var rec connectortest.EmitRecorder
	cfg := configFor(t, rs.URL(), nil)

	cur, err := c.IncrementalSync(context.Background(), cfg, pageCursor("CkgK...page2"), rec.Emit)
	if err != nil {
		t.Fatalf("IncrementalSync(resume): %v", err)
	}
	if want := syncCursor("SYNCTOKEN-AFTER-BACKFILL"); cur != want {
		t.Errorf("cursor = %q, want %q", cur, want)
	}
	docs := rec.Docs()
	if len(docs) != 1 {
		t.Fatalf("emitted %d docs, want 1 (the resumed final page)", len(docs))
	}
	for _, d := range docs {
		connectortest.ValidateDocument(t, cfg, d)
	}
	if want := sdk.DocID(connectorID, nativeID(defaultCalendarID, "evt-1on1")); docs[0].GetDocId() != want {
		t.Errorf("resumed doc_id = %q, want %q", docs[0].GetDocId(), want)
	}
}

// TestIncrementalPaginates proves replaySync follows nextPageToken across a
// multi-page delta before returning the final sync cursor.
func TestIncrementalPaginates(t *testing.T) {
	t.Parallel()
	rs := loadServer(t, "testdata/incremental_paged.json")
	c := New(WithLogger(slog.New(slog.DiscardHandler)))

	var rec connectortest.EmitRecorder
	cfg := configFor(t, rs.URL(), nil)

	cur, err := c.IncrementalSync(context.Background(), cfg, syncCursor("SYNCTOKEN-AFTER-BACKFILL"), rec.Emit)
	if err != nil {
		t.Fatalf("IncrementalSync(paged): %v", err)
	}
	if want := syncCursor("SYNCTOKEN-PAGED"); cur != want {
		t.Errorf("cursor = %q, want %q", cur, want)
	}
	docs := rec.Docs()
	if len(docs) != 2 {
		t.Fatalf("emitted %d docs across pages, want 2", len(docs))
	}
}
