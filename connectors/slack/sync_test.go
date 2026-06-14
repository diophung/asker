package slack

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
)

// checkpointRecorder records every checkpointed cursor so a test can assert
// FullSync checkpoints at a resumable (per-channel) boundary.
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
	return append([]sdk.Cursor(nil), r.cursors...)
}

// TestFullSyncCheckpointsPerChannel proves FullSync checkpoints the
// accumulating per-channel cursor after each channel so an interrupted
// backfill resumes instead of restarting.
func TestFullSyncCheckpointsPerChannel(t *testing.T) {
	t.Parallel()
	cas, err := connectortest.LoadCassette("testdata/fullsync.json")
	if err != nil {
		t.Fatal(err)
	}
	rs := connectortest.NewReplayServer(t, cas)
	c := newTestConnector()

	cfg := testConfig(t, rs.URL(), "")
	var cp checkpointRecorder
	cfg.Checkpoint = cp.Checkpoint

	var rec connectortest.EmitRecorder
	cur, err := c.FullSync(context.Background(), cfg, rec.Emit)
	if err != nil {
		t.Fatalf("FullSync: %v", err)
	}

	// Three channels listed => three checkpoints, the last equal to the
	// returned cursor.
	got := cp.all()
	if len(got) != 3 {
		t.Fatalf("checkpoints = %d, want 3 (one per channel)", len(got))
	}
	if got[len(got)-1] != cur {
		t.Errorf("final checkpoint %q != returned cursor %q", got[len(got)-1], cur)
	}
	// The cursor must be a resumable per-channel watermark map.
	st, err := parseCursor(cur)
	if err != nil {
		t.Fatalf("returned cursor not parseable: %v", err)
	}
	if st["C100"] != "1700000300.000300" {
		t.Errorf("C100 watermark = %q", st["C100"])
	}
}

// TestIncrementalSkipsVanishedChannel proves a channel deleted/left since the
// last list (conversations.history -> channel_not_found) is skipped rather
// than failing the whole pass.
func TestIncrementalSkipsVanishedChannel(t *testing.T) {
	t.Parallel()
	cas := &connectortest.Cassette{Name: "vanished", Interactions: []*connectortest.Interaction{
		{
			Request: connectortest.RecordedRequest{
				Method: "GET", Path: "/conversations.list",
				Query: "limit=200&types=public_channel,private_channel,mpim,im&exclude_archived=true",
			},
			Response: connectortest.RecordedResponse{
				Status: 200,
				Body:   `{"ok":true,"channels":[{"id":"C100","name":"general"}],"response_metadata":{"next_cursor":""}}`,
			},
		},
		{
			Request: connectortest.RecordedRequest{
				Method: "GET", Path: "/conversations.history",
				Query: "channel=C100&limit=200&oldest=1700000300.000300&inclusive=false",
			},
			Response: connectortest.RecordedResponse{
				Status: 200,
				Body:   `{"ok":false,"error":"channel_not_found"}`,
			},
		},
	}}
	rs := connectortest.NewReplayServer(t, cas)
	c := newTestConnector()
	cfg := testConfig(t, rs.URL(), "")

	var rec connectortest.EmitRecorder
	cur, err := c.IncrementalSync(context.Background(), cfg, sdk.Cursor(`{"C100":"1700000300.000300"}`), rec.Emit)
	if err != nil {
		t.Fatalf("IncrementalSync skipped-channel: %v", err)
	}
	if n := len(rec.Docs()); n != 0 {
		t.Errorf("emitted %d docs for a vanished channel, want 0", n)
	}
	// Cursor preserved.
	if _, perr := parseCursor(cur); perr != nil {
		t.Errorf("returned cursor not parseable: %v", perr)
	}
}

// TestIncrementalUnparseableCursorExpires proves a cursor the connector did
// not produce surfaces as sdk.ErrCursorExpired (the hub then re-full-syncs).
func TestIncrementalUnparseableCursorExpires(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	cfg := testConfig(t, "http://unused.invalid", "")
	_, err := c.IncrementalSync(context.Background(), cfg, sdk.Cursor("history:42"), emit)
	if err == nil {
		t.Fatal("a foreign cursor was accepted")
	}
	if !errors.Is(err, sdk.ErrCursorExpired) {
		t.Errorf("err = %v, want sdk.ErrCursorExpired", err)
	}
}
