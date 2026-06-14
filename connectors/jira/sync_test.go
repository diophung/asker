package jira

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// nopEmit discards documents for tests that only care about cursors/errors.
func nopEmit(context.Context, *askerv1.Document) error { return nil }

// checkpointRecorder captures the cursors a sync checkpoints, and can fail on
// demand to exercise the terminal-checkpoint-error path.
type checkpointRecorder struct {
	cursors []sdk.Cursor
	fail    error
}

func (r *checkpointRecorder) fn(_ context.Context, cur sdk.Cursor) error {
	r.cursors = append(r.cursors, cur)
	return r.fail
}

// TestFullSyncCheckpointsAfterEachPage drives the 2-page fullsync cassette and
// asserts a checkpoint fires after the first completed page carrying the
// per-issue cursor, so an interrupted backfill resumes.
func TestFullSyncCheckpointsAfterEachPage(t *testing.T) {
	t.Parallel()
	rs := newReplayServer(t, "testdata/fullsync.json")
	cfg := contractConfig(t, rs.URL(), `{}`)

	cp := &checkpointRecorder{}
	cfg.Checkpoint = cp.fn

	var rec connectortest.EmitRecorder
	cur, err := New().FullSync(context.Background(), cfg, rec.Emit)
	if err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if want := sdk.Cursor("updated:2026/06/11 14:45|key:DEMO-3"); cur != want {
		t.Errorf("final cursor = %q, want %q", cur, want)
	}
	if len(cp.cursors) != 1 {
		t.Fatalf("checkpointed %d times, want 1 (after page 1)", len(cp.cursors))
	}
	if want := sdk.Cursor("updated:2026/06/10 10:30|key:DEMO-2"); cp.cursors[0] != want {
		t.Errorf("page-1 checkpoint = %q, want %q", cp.cursors[0], want)
	}
}

// TestFullSyncCheckpointErrorIsTerminal: a Checkpoint error aborts the pass.
func TestFullSyncCheckpointErrorIsTerminal(t *testing.T) {
	t.Parallel()
	rs := newReplayServer(t, "testdata/fullsync.json")
	cfg := contractConfig(t, rs.URL(), `{}`)

	abort := errors.New("hub restart")
	cp := &checkpointRecorder{fail: abort}
	cfg.Checkpoint = cp.fn

	_, err := New().FullSync(context.Background(), cfg, nopEmit)
	if !errors.Is(err, abort) {
		t.Fatalf("FullSync error = %v, want %v", err, abort)
	}
}

// TestSyncContextCanceled: a canceled context aborts before any request.
func TestSyncContextCanceled(t *testing.T) {
	t.Parallel()
	rs := newReplayServer(t, "testdata/fullsync.json")
	cfg := contractConfig(t, rs.URL(), `{}`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New().FullSync(ctx, cfg, nopEmit); !errors.Is(err, context.Canceled) {
		t.Errorf("FullSync on canceled ctx = %v, want context.Canceled", err)
	}
}

// TestWithLogger sets a custom logger.
func TestWithLogger(t *testing.T) {
	t.Parallel()
	c := New(WithLogger(slog.Default()), WithLogger(nil)).(*Connector)
	if c.log == nil {
		t.Error("WithLogger(nil) cleared the logger")
	}
}

func TestAPIErrorMessage(t *testing.T) {
	t.Parallel()
	// The upstream detail must NOT appear in the error string: apiError is
	// surfaced to the user (via Validate/IncrementalSync) and would otherwise
	// leak field names / account ids / filter detail from the source.
	const secret = "user 'acc-secret' lacks BROWSE on project SECRET"
	e := &apiError{status: 400, messages: []string{secret, "jql: bad bound"}}
	got := e.Error()
	if got == "" {
		t.Error("apiError.Error() empty")
	}
	if strings.Contains(got, secret) || strings.Contains(got, "jql: bad bound") {
		t.Errorf("apiError.Error() leaked upstream detail: %q", got)
	}
	bare := &apiError{status: 503}
	if got := bare.Error(); got == "" {
		t.Error("status-only apiError.Error() empty")
	}
}
