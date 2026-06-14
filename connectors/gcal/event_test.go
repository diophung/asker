package gcal

import (
	"testing"
	"time"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// fixedNow returns a connector whose now() is deterministic, for stable
// tombstone deleted_at values.
func fixedNowConnector(t time.Time) *Connector {
	return &Connector{now: func() time.Time { return t }}
}

func TestEventDocumentMapping(t *testing.T) {
	t.Parallel()
	c := fixedNowConnector(time.Unix(0, 0))
	ev := &event{
		ID:          "evt-standup",
		Status:      "confirmed",
		Etag:        `"3401"`,
		Summary:     "Daily standup",
		Description: "Quick sync. <b>Bring</b> blockers.<br>Second line.",
		Location:    "Zoom",
		HTMLLink:    "https://cal.example/eid=evt-standup",
		Created:     "2026-06-01T09:00:00Z",
		Updated:     "2026-06-02T10:15:00.500Z",
		Start:       &eventDateTime{DateTime: "2026-06-03T09:30:00-07:00"},
		End:         &eventDateTime{DateTime: "2026-06-03T09:45:00-07:00"},
		Organizer:   &eventActor{Email: "alice@example.com", DisplayName: "Alice Anderson"},
		Attendees: []*attendee{
			{Email: "alice@example.com", DisplayName: "Alice Anderson", Organizer: true, ResponseStatus: "accepted"},
			{Email: "bob@example.com", DisplayName: "Bob Brown", ResponseStatus: "needsAction"},
		},
	}

	doc := c.eventDocument("tenant-a", "primary", ev)

	if doc.GetType() != askerv1.DocType_CALENDAR_EVENT {
		t.Errorf("type = %v, want CALENDAR_EVENT", doc.GetType())
	}
	if want := sdk.DocID("gcal", "primary:evt-standup"); doc.GetDocId() != want {
		t.Errorf("doc_id = %q, want %q", doc.GetDocId(), want)
	}
	if doc.GetSourceNativeId() != "primary:evt-standup" {
		t.Errorf("source_native_id = %q", doc.GetSourceNativeId())
	}
	if doc.GetTitle() != "Daily standup" {
		t.Errorf("title = %q", doc.GetTitle())
	}
	if doc.GetVersionEtag() != `"3401"` {
		t.Errorf("version_etag = %q, want event etag", doc.GetVersionEtag())
	}
	// Body: HTML stripped, location appended.
	wantBody := "Quick sync. Bring blockers.\nSecond line.\n\nZoom"
	if doc.GetBodyText() != wantBody {
		t.Errorf("body_text = %q, want %q", doc.GetBodyText(), wantBody)
	}

	// Participants: organizer first, then attendees.
	parts := doc.GetParticipants()
	if len(parts) != 3 {
		t.Fatalf("participants = %d, want 3", len(parts))
	}
	if parts[0].GetRole() != "organizer" || parts[0].GetEmail() != "alice@example.com" {
		t.Errorf("participant[0] = %+v, want organizer alice", parts[0])
	}
	if parts[1].GetRole() != "attendee" || parts[2].GetRole() != "attendee" {
		t.Errorf("attendee roles = %q,%q", parts[1].GetRole(), parts[2].GetRole())
	}

	// Metadata.
	md := doc.GetMetadata()
	for k, want := range map[string]string{
		"event_id":                          "evt-standup",
		"calendar_id":                       "primary",
		"html_link":                         "https://cal.example/eid=evt-standup",
		"location":                          "Zoom",
		"status":                            "confirmed",
		"start":                             "2026-06-03T09:30:00-07:00",
		"end":                               "2026-06-03T09:45:00-07:00",
		"response_status:alice@example.com": "accepted",
		"response_status:bob@example.com":   "needsAction",
	} {
		if md[k] != want {
			t.Errorf("metadata[%q] = %q, want %q", k, md[k], want)
		}
	}

	// Timestamps.
	if doc.GetTs().GetCreated() == nil || doc.GetTs().GetModified() == nil {
		t.Fatalf("ts = %+v, want created and modified", doc.GetTs())
	}
	if doc.GetTs().GetIngested() != nil {
		t.Error("ts.ingested set; the hub stamps it")
	}
	if got := doc.GetTs().GetCreated().AsTime(); !got.Equal(time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("ts.created = %v", got)
	}
}

func TestEventDocumentAllDayAndDefaults(t *testing.T) {
	t.Parallel()
	c := fixedNowConnector(time.Unix(0, 0))
	ev := &event{
		ID:        "evt-1on1",
		Status:    "confirmed",
		Etag:      `"3410"`,
		Summary:   "", // -> noTitle
		Start:     &eventDateTime{Date: "2026-06-10"},
		End:       &eventDateTime{Date: "2026-06-11"},
		Organizer: &eventActor{Email: "alice@example.com"},
	}
	doc := c.eventDocument("tenant-a", "primary", ev)
	if doc.GetTitle() != noTitle {
		t.Errorf("title = %q, want %q", doc.GetTitle(), noTitle)
	}
	if doc.GetBodyText() != "" {
		t.Errorf("body_text = %q, want empty", doc.GetBodyText())
	}
	if doc.GetMetadata()["start"] != "2026-06-10" {
		t.Errorf("all-day start = %q, want date", doc.GetMetadata()["start"])
	}
}

func TestTombstoneFromCanceled(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)
	c := fixedNowConnector(now)
	ev := &event{ID: "evt-standup", Status: statusCanceled, Etag: `"3500"`}

	doc := c.eventDocument("tenant-a", "primary", ev)
	if !doc.GetTombstone().GetDeleted() {
		t.Fatal("canceled event did not produce a tombstone")
	}
	if doc.GetBodyText() != "" || len(doc.GetChunks()) != 0 {
		t.Error("tombstone carries a body")
	}
	if doc.GetVersionEtag() == "" {
		t.Error("tombstone version_etag empty")
	}
	if want := sdk.DocID("gcal", "primary:evt-standup"); doc.GetDocId() != want {
		t.Errorf("tombstone doc_id = %q, want %q", doc.GetDocId(), want)
	}
	if !doc.GetTombstone().GetDeletedAt().AsTime().Equal(now) {
		t.Errorf("deleted_at = %v, want %v", doc.GetTombstone().GetDeletedAt().AsTime(), now)
	}
}

func TestVersionEtagFallback(t *testing.T) {
	t.Parallel()
	// No etag: a body-bearing event hashes its body; a body-less canceled
	// event still gets a non-empty, stable etag from id+updated.
	withBody := versionEtag(&event{ID: "x", Updated: "u"}, "some body")
	if withBody == "" {
		t.Fatal("etag empty for body-bearing event without etag")
	}
	noBody := versionEtag(&event{ID: "x", Updated: "u"}, "")
	if noBody == "" {
		t.Fatal("etag empty for body-less event without etag")
	}
	if withBody == noBody {
		t.Error("body and body-less etag fallbacks collided")
	}
}

func TestStripHTML(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ in, want string }{
		"plain":       {"just text", "just text"},
		"entities":    {"a &amp; b", "a & b"},
		"block break": {"<p>one</p><p>two</p>", "one\ntwo"},
		"script drop": {"keep<script>alert(1)</script>end", "keepend"},
		"inline tag":  {"a <b>bold</b> word", "a bold word"},
	}
	for name, tc := range cases {
		if got := stripHTML(tc.in); got != tc.want {
			t.Errorf("%s: stripHTML(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

func TestParseCursorRoundTrip(t *testing.T) {
	t.Parallel()
	if st, err := parseCursor(syncCursor("tok-1")); err != nil || st.syncToken != "tok-1" || st.backfill {
		t.Errorf("parseCursor(sync) = %+v, %v", st, err)
	}
	if st, err := parseCursor(pageCursor("pg-1")); err != nil || st.pageToken != "pg-1" || !st.backfill {
		t.Errorf("parseCursor(page) = %+v, %v", st, err)
	}
	for _, bad := range []sdk.Cursor{"", "garbage", "sync:", "page:"} {
		if _, err := parseCursor(bad); err == nil {
			t.Errorf("parseCursor(%q) accepted a bad cursor", bad)
		}
	}
}
