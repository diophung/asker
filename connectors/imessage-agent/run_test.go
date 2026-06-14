package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// discardLogger is a logger that drops output, for tests that don't assert logs.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRunSyncUploadsPerChat(t *testing.T) {
	reader := &fakeReader{rows: sampleRows()}
	up := &fakeUploader{}

	res, err := runSync(context.Background(), discardLogger(), reader, up, io.Discard, options{since: 0})
	if err != nil {
		t.Fatalf("runSync: %v", err)
	}
	if res.messages != 3 {
		t.Errorf("messages = %d, want 3", res.messages)
	}
	if res.chats != 2 {
		t.Errorf("chats = %d, want 2", res.chats)
	}
	if res.uploaded != 2 {
		t.Errorf("uploaded = %d, want 2", res.uploaded)
	}
	if res.newHighWater != 5 || !res.highWaterMove {
		t.Errorf("newHighWater = %d move=%v, want 5 true", res.newHighWater, res.highWaterMove)
	}
	if up.count() != 2 {
		t.Errorf("uploads recorded = %d, want 2", up.count())
	}
	// The Team chat (g2) carries its display name into the title.
	var titles []string
	for _, u := range up.uploads {
		titles = append(titles, u.title)
	}
	joined := strings.Join(titles, "|")
	if !strings.Contains(joined, "iMessage: Team") {
		t.Errorf("titles = %q, want one to be 'iMessage: Team'", joined)
	}
	if !strings.Contains(joined, "iMessage: g1") {
		t.Errorf("titles = %q, want one to be 'iMessage: g1'", joined)
	}
}

func TestRunSyncRespectsSince(t *testing.T) {
	reader := &fakeReader{rows: sampleRows()}
	up := &fakeUploader{}
	// since=3 should drop rows 1 and 3, leaving only row 5 (chat g2).
	res, err := runSync(context.Background(), discardLogger(), reader, up, io.Discard, options{since: 3})
	if err != nil {
		t.Fatalf("runSync: %v", err)
	}
	if reader.since != 3 {
		t.Errorf("reader called with since = %d, want 3", reader.since)
	}
	if res.messages != 1 || res.chats != 1 || res.uploaded != 1 {
		t.Errorf("res = %+v, want 1 message/1 chat/1 upload", res)
	}
	if res.newHighWater != 5 {
		t.Errorf("newHighWater = %d, want 5", res.newHighWater)
	}
}

func TestRunSyncNoNewMessages(t *testing.T) {
	reader := &fakeReader{rows: sampleRows()}
	up := &fakeUploader{}
	res, err := runSync(context.Background(), discardLogger(), reader, up, io.Discard, options{since: 100})
	if err != nil {
		t.Fatalf("runSync: %v", err)
	}
	if res.messages != 0 || res.uploaded != 0 {
		t.Errorf("res = %+v, want no work", res)
	}
	if res.highWaterMove {
		t.Error("high-water moved despite no new messages")
	}
	if res.newHighWater != 100 {
		t.Errorf("newHighWater = %d, want unchanged 100", res.newHighWater)
	}
}

func TestRunSyncDryRunPrintsAndUploadsNothing(t *testing.T) {
	reader := &fakeReader{rows: sampleRows()}
	up := &fakeUploader{} // must not be called
	var out bytes.Buffer

	res, err := runSync(context.Background(), discardLogger(), reader, up, &out, options{since: 0, dryRun: true})
	if err != nil {
		t.Fatalf("runSync: %v", err)
	}
	if res.uploaded != 0 {
		t.Errorf("uploaded = %d in dry-run, want 0", res.uploaded)
	}
	if up.count() != 0 {
		t.Errorf("uploader called %d times in dry-run, want 0", up.count())
	}
	// Dry-run output includes the message bodies (the only place that's allowed).
	if !strings.Contains(out.String(), "standup?") {
		t.Errorf("dry-run output missing transcript body; got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "=====") {
		t.Error("dry-run output missing the per-chat separator")
	}
}

func TestRunSyncReadError(t *testing.T) {
	reader := &fakeReader{err: errors.New("boom")}
	_, err := runSync(context.Background(), discardLogger(), reader, &fakeUploader{}, io.Discard, options{})
	if err == nil {
		t.Fatal("runSync should propagate a read error")
	}
	if !strings.Contains(err.Error(), "read messages") {
		t.Errorf("error = %v, want it to mention read messages", err)
	}
}

func TestRunSyncUploadErrorStopsAndDoesNotAdvanceState(t *testing.T) {
	reader := &fakeReader{rows: sampleRows()}
	up := &fakeUploader{failAt: 1} // first upload fails
	res, err := runSync(context.Background(), discardLogger(), reader, up, io.Discard, options{since: 0})
	if err == nil {
		t.Fatal("runSync should return the upload error")
	}
	if !errors.Is(err, errUploadFailed) {
		t.Errorf("error = %v, want it to wrap errUploadFailed", err)
	}
	// Nothing uploaded, so the high-water must not move off the starting
	// cursor: the next run retries every message.
	if up.count() != 0 {
		t.Errorf("uploads recorded = %d, want 0 (failed before success)", up.count())
	}
	if res.highWaterMove {
		t.Error("high-water moved despite zero successful uploads")
	}
	if res.newHighWater != 0 {
		t.Errorf("newHighWater = %d, want unchanged 0 after a total failure", res.newHighWater)
	}
}

// TestRunSyncPartialUploadDoesNotAdvancePastUnsent is the regression test for
// the over-advanced high-water bug: the first chat (g1, max ROWID 3) uploads,
// then the second chat (g2, ROWID 5) fails. The high-water must advance only to
// 3 — the max of the SENT messages — never to 5, or the next run would skip the
// unsent g2 message (ROWID 5) forever. The pre-fix code set newHighWater to
// maxRowID(allRows)==5 up front, which this asserts against.
func TestRunSyncPartialUploadDoesNotAdvancePastUnsent(t *testing.T) {
	reader := &fakeReader{rows: sampleRows()}
	up := &fakeUploader{failAt: 2} // g1 uploads, g2 fails
	res, err := runSync(context.Background(), discardLogger(), reader, up, io.Discard, options{since: 0})
	if err == nil {
		t.Fatal("runSync should return the upload error from the second chat")
	}
	if !errors.Is(err, errUploadFailed) {
		t.Errorf("error = %v, want it to wrap errUploadFailed", err)
	}
	if up.count() != 1 {
		t.Errorf("uploads recorded = %d, want 1 (g1 succeeded, g2 failed)", up.count())
	}
	if res.uploaded != 1 {
		t.Errorf("res.uploaded = %d, want 1", res.uploaded)
	}
	// The crux: advance only to the max ROWID of the SENT chat (g1 -> 3), never
	// past the unsent g2 message (ROWID 5).
	if res.newHighWater != 3 {
		t.Errorf("newHighWater = %d, want 3 (max ROWID among uploaded chats, not 5)", res.newHighWater)
	}
	if !res.highWaterMove {
		t.Error("highWaterMove = false, want true (g1 advanced the cursor to 3)")
	}
}
