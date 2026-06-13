package ical

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
)

// eventFrom parses a single VEVENT from the given property lines.
func eventFrom(t *testing.T, lines ...string) *vevent {
	t.Helper()
	body := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\n" + strings.Join(lines, "\r\n") + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	events := parseEvents(body)
	if len(events) != 1 {
		t.Fatalf("parsed %d events, want 1", len(events))
	}
	return events[0]
}

func TestVersionEtag(t *testing.T) {
	t.Parallel()

	if got := versionEtag(eventFrom(t, "UID:x", "SEQUENCE:5")); got != "seq:5" {
		t.Errorf("SEQUENCE etag = %q, want seq:5", got)
	}
	if got := versionEtag(eventFrom(t, "UID:x", "LAST-MODIFIED:20260101T000000Z")); got != "mod:20260101T000000Z" {
		t.Errorf("LAST-MODIFIED etag = %q", got)
	}
	// Neither SEQUENCE nor LAST-MODIFIED: a stable sha256 of the raw block.
	got := versionEtag(eventFrom(t, "UID:x", "SUMMARY:Just a summary"))
	if !strings.HasPrefix(got, "sha256:") {
		t.Errorf("fallback etag = %q, want sha256: prefix", got)
	}
	// Same input -> same fingerprint (idempotent upserts).
	again := versionEtag(eventFrom(t, "UID:x", "SUMMARY:Just a summary"))
	if got != again {
		t.Errorf("fingerprint not stable: %q vs %q", got, again)
	}
}

func TestMailtoAddr(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"mailto:alice@example.com": "alice@example.com",
		"MAILTO:bob@example.com":   "bob@example.com",
		"Mailto:c@x.com":           "c@x.com",
		"alice@example.com":        "", // no scheme
		"":                         "",
	}
	for in, want := range tests {
		if got := mailtoAddr(in); got != want {
			t.Errorf("mailtoAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestActorNameOnlyAndEmpty(t *testing.T) {
	t.Parallel()
	// CN with no resolvable mailto still yields a name-only participant.
	ev := eventFrom(t, "UID:x", "ORGANIZER;CN=No Email:invalid:addr")
	ps := participants(ev)
	if len(ps) != 1 || ps[0].GetName() != "No Email" || ps[0].GetEmail() != "" {
		t.Errorf("name-only organizer = %+v", ps)
	}
	// An ORGANIZER with neither CN nor a mailto value is dropped.
	empty := eventFrom(t, "UID:x", "ORGANIZER:invalid:addr")
	if ps := participants(empty); len(ps) != 0 {
		t.Errorf("empty organizer produced %d participants, want 0", len(ps))
	}
}

func TestParseICalTime(t *testing.T) {
	t.Parallel()
	if ts := parseICalTime("20260603T093000Z"); ts == nil || !ts.AsTime().Equal(time.Date(2026, 6, 3, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("UTC date-time parse = %v", ts)
	}
	if ts := parseICalTime("20260610"); ts == nil || !ts.AsTime().Equal(time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("all-day date parse = %v", ts)
	}
	if ts := parseICalTime("not-a-time"); ts != nil {
		t.Errorf("unparseable time = %v, want nil", ts)
	}
	if ts := parseICalTime("  "); ts != nil {
		t.Errorf("blank time = %v, want nil", ts)
	}
}

func TestDtstampFallback(t *testing.T) {
	t.Parallel()
	if got := dtstamp(eventFrom(t, "UID:x", "DTSTAMP:20260601T000000Z")); got != "20260601T000000Z" {
		t.Errorf("dtstamp = %q", got)
	}
	// Falls back to LAST-MODIFIED, then CREATED.
	if got := dtstamp(eventFrom(t, "UID:x", "LAST-MODIFIED:20260602T000000Z")); got != "20260602T000000Z" {
		t.Errorf("dtstamp LAST-MODIFIED fallback = %q", got)
	}
	if got := dtstamp(eventFrom(t, "UID:x", "CREATED:20260603T000000Z")); got != "20260603T000000Z" {
		t.Errorf("dtstamp CREATED fallback = %q", got)
	}
	if got := dtstamp(eventFrom(t, "UID:x", "SUMMARY:none")); got != "" {
		t.Errorf("dtstamp with no stamp = %q, want empty", got)
	}
}

func TestEventTimestamps(t *testing.T) {
	t.Parallel()
	ev := eventFrom(t, "UID:x", "CREATED:20260601T090000Z", "LAST-MODIFIED:20260602T101500Z")
	ts := eventTimestamps(ev)
	if ts == nil || ts.GetCreated() == nil || ts.GetModified() == nil {
		t.Fatalf("timestamps = %v, want created+modified", ts)
	}
	if ts.GetIngested() != nil {
		t.Error("ts.ingested must be left unset by the connector")
	}
	// An event with no parseable times yields nil Timestamps.
	if got := eventTimestamps(eventFrom(t, "UID:x", "SUMMARY:none")); got != nil {
		t.Errorf("timestamps with no times = %v, want nil", got)
	}
}

// TestRedactURL proves a tokenized feed URL's query secret never leaks into an
// error message.
func TestRedactURL(t *testing.T) {
	t.Parallel()
	feed := "https://cal.example.com/private/basic.ics?token=s3cr3t-bearer"
	in := errors.New(`Get "https://cal.example.com/private/basic.ics?token=s3cr3t-bearer": dial tcp: timeout`)
	out := redactURL(in, feed)
	if strings.Contains(out.Error(), "s3cr3t-bearer") {
		t.Errorf("redactURL leaked the token: %v", out)
	}
	if !strings.Contains(out.Error(), "REDACTED") {
		t.Errorf("redactURL did not redact: %v", out)
	}
	// A URL without a query is passed through unchanged; nil stays nil.
	if got := redactURL(in, "https://cal.example.com/x.ics"); got != in {
		t.Errorf("redactURL with no query mutated the error")
	}
	if redactURL(nil, feed) != nil {
		t.Error("redactURL(nil) should be nil")
	}
}

// TestFetchNon2xx proves a non-2xx feed status surfaces as an error from FullSync.
func TestFetchNon2xx(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("nope"))
	}))
	t.Cleanup(srv.Close)

	c := newTestConnector()
	cfg := configFor(t, srv.URL)
	var rec connectortest.EmitRecorder
	if _, err := c.FullSync(context.Background(), cfg, rec.Emit); err == nil {
		t.Fatal("FullSync accepted a 404 feed")
	}
}

// TestFullSyncContextCanceled proves FullSync honors a canceled context.
func TestFullSyncContextCanceled(t *testing.T) {
	t.Parallel()
	rs := loadServer(t, "testdata/fullsync.json")
	c := newTestConnector()
	cfg := configFor(t, rs.URL())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var rec connectortest.EmitRecorder
	if _, err := c.FullSync(ctx, cfg, rec.Emit); err == nil {
		t.Fatal("FullSync ignored a canceled context")
	}
}

// TestFullSyncSkipsUIDless proves a VEVENT without a UID is skipped (it cannot be
// tracked across syncs).
func TestFullSyncSkipsUIDless(t *testing.T) {
	t.Parallel()
	body := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nSUMMARY:No UID here\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:has-uid@example.com\r\nSUMMARY:Keeper\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	c := newTestConnector()
	cfg := configFor(t, srv.URL)
	var rec connectortest.EmitRecorder
	if _, err := c.FullSync(context.Background(), cfg, rec.Emit); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	docs := rec.Docs()
	if len(docs) != 1 {
		t.Fatalf("emitted %d docs, want 1 (UID-less event skipped)", len(docs))
	}
	if got := docs[0].GetMetadata()["uid"]; got != "has-uid@example.com" {
		t.Errorf("kept doc uid = %q", got)
	}
}

// TestCursorRoundTripAndReject proves encode/parseCursor round-trip and that a
// foreign cursor is rejected.
func TestCursorRoundTripAndReject(t *testing.T) {
	t.Parallel()
	st := cursorState{
		Hash:       "abc123",
		MaxDTStamp: "20260601T000000Z",
		Events:     map[string]string{"a@x": "seq:1", "b@x": "mod:20260101T000000Z"},
	}
	got, err := parseCursor(st.encode())
	if err != nil {
		t.Fatalf("parseCursor(encode): %v", err)
	}
	if got.Hash != st.Hash || got.MaxDTStamp != st.MaxDTStamp || len(got.Events) != 2 {
		t.Errorf("round-trip = %+v, want %+v", got, st)
	}

	for _, bad := range []sdk.Cursor{"", "garbage", "ical1:not-base64!!", sdk.Cursor("ical1:" + "Zm9v")} {
		if _, err := parseCursor(bad); err == nil {
			t.Errorf("parseCursor(%q) accepted an invalid cursor", bad)
		}
	}
}
