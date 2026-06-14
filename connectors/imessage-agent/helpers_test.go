package main

import (
	"context"
	"sync"
	"testing"

	"github.com/asker/asker/connectors/imessage-agent/internal/imsg"
)

// sampleRows returns a small fixed set spanning two chats, used across the main
// package's tests. g1 has rows 1 and 3; g2 has row 5.
func sampleRows() []imsg.Row {
	return []imsg.Row{
		{RowID: 1, ChatGUID: "g1", Handle: "+15551112222", Text: "hi", Service: "iMessage"},
		{RowID: 3, ChatGUID: "g1", IsFromMe: true, Text: "yo", Service: "iMessage"},
		{RowID: 5, ChatGUID: "g2", ChatName: "Team", Handle: "+15553334444", Text: "standup?", Service: "iMessage"},
	}
}

// fakeReader is an in-memory messageReader for orchestration tests.
type fakeReader struct {
	rows []imsg.Row
	err  error
	// since records the high-water mark the last ReadSince was called with.
	since int64
}

func (f *fakeReader) ReadSince(_ context.Context, since int64) ([]imsg.Row, error) {
	f.since = since
	if f.err != nil {
		return nil, f.err
	}
	var out []imsg.Row
	for _, r := range f.rows {
		if r.RowID > since {
			out = append(out, r)
		}
	}
	return out, nil
}

// fakeUploader records uploaded transcripts and can be made to fail.
type fakeUploader struct {
	mu       sync.Mutex
	uploads  []transcript
	failAt   int // 1-based index at which Upload returns an error; 0 = never
	calls    int
	docIDSeq int
}

func (f *fakeUploader) Upload(_ context.Context, t transcript) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failAt != 0 && f.calls == f.failAt {
		return "", errUploadFailed
	}
	f.uploads = append(f.uploads, t)
	f.docIDSeq++
	return "doc-" + itoa(f.docIDSeq), nil
}

func (f *fakeUploader) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.uploads)
}

// errUploadFailed is the sentinel a fakeUploader returns when configured to fail.
var errUploadFailed = errTestf("simulated upload failure")

type errTest string

func (e errTest) Error() string { return string(e) }
func errTestf(s string) error   { return errTest(s) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// must fails the test on a non-nil error.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
