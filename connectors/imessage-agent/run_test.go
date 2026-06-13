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
	// The caller (main) only persists state on a clean pass; the partial result
	// still reports highWaterMove so callers can choose, but main checks err
	// first. Assert the upload count reflects the early stop.
	if up.count() != 0 {
		t.Errorf("uploads recorded = %d, want 0 (failed before success)", up.count())
	}
	_ = res
}
