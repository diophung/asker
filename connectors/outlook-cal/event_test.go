package outlookcal

import (
	"encoding/json"
	"strings"
	"testing"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

const tenant = "tenant-x"

func TestEventDocumentFullMapping(t *testing.T) {
	t.Parallel()
	e := graphEvent{
		ID:        "EVT-1",
		ODataEtag: `W/"etag-1"`,
		ChangeKey: "chg-1",
		Subject:   "Design Review",
		Body: &itemBody{
			ContentType: "html",
			Content:     "<html><body><p>Discuss API.</p><div>Bring laptops.</div></body></html>",
		},
		Location:             &location{DisplayName: "Room 5"},
		Start:                &dateTimeZone{DateTime: "2026-06-15T09:00:00.0000000", TimeZone: "UTC"},
		End:                  &dateTimeZone{DateTime: "2026-06-15T10:00:00.0000000", TimeZone: "UTC"},
		IsAllDay:             false,
		WebLink:              "https://outlook.office365.com/calendar/item/EVT-1",
		CreatedDateTime:      "2026-06-01T12:00:00Z",
		LastModifiedDateTime: "2026-06-02T08:30:00Z",
		Organizer:            &recipient{EmailAddress: emailAddress{Name: "Alice", Address: "alice@example.com"}},
		Attendees: []attendee{
			{Type: "required", Status: &responseInfo{Response: "accepted"}, EmailAddress: emailAddress{Name: "Bob", Address: "bob@example.com"}},
			{Type: "optional", Status: &responseInfo{Response: "declined"}, EmailAddress: emailAddress{Name: "Carol", Address: "carol@example.com"}},
		},
	}

	doc := eventDocument(tenant, e)

	if doc.GetType() != askerv1.DocType_CALENDAR_EVENT {
		t.Errorf("type = %v, want CALENDAR_EVENT", doc.GetType())
	}
	if doc.GetTitle() != "Design Review" {
		t.Errorf("title = %q", doc.GetTitle())
	}
	if doc.GetConnectorId() != connectorID || doc.GetSourceNativeId() != "EVT-1" {
		t.Errorf("identity fields = %q/%q", doc.GetConnectorId(), doc.GetSourceNativeId())
	}
	if doc.GetVersionEtag() != `W/"etag-1"` {
		t.Errorf("version_etag = %q, want the @odata.etag", doc.GetVersionEtag())
	}
	if doc.GetAcl() != nil {
		t.Error("acl should be unset for a personal calendar (ADR-012)")
	}

	body := doc.GetBodyText()
	for _, want := range []string{"Discuss API.", "Bring laptops.", "Room 5"} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q missing %q", body, want)
		}
	}
	if strings.Contains(body, "<") {
		t.Errorf("body still contains HTML tags: %q", body)
	}

	// Participants: organizer first, then attendees with their types as roles.
	if len(doc.GetParticipants()) != 3 {
		t.Fatalf("participants = %d, want 3", len(doc.GetParticipants()))
	}
	if got := doc.GetParticipants()[0]; got.GetRole() != "organizer" || got.GetEmail() != "alice@example.com" {
		t.Errorf("organizer participant = %+v", got)
	}
	if got := doc.GetParticipants()[1]; got.GetRole() != "required" || got.GetEmail() != "bob@example.com" {
		t.Errorf("attendee[0] = %+v", got)
	}

	md := doc.GetMetadata()
	if md["event_id"] != "EVT-1" {
		t.Errorf("metadata event_id = %q", md["event_id"])
	}
	if md["web_link"] != "https://outlook.office365.com/calendar/item/EVT-1" {
		t.Errorf("metadata web_link = %q", md["web_link"])
	}
	if md["location"] != "Room 5" {
		t.Errorf("metadata location = %q", md["location"])
	}
	if md["start"] != "2026-06-15T09:00:00.0000000 UTC" {
		t.Errorf("metadata start = %q", md["start"])
	}
	if _, ok := md["is_all_day"]; ok {
		t.Errorf("is_all_day set on a timed event: %q", md["is_all_day"])
	}

	// attendee_responses is a JSON object of address->response.
	var responses map[string]string
	if err := json.Unmarshal([]byte(md["attendee_responses"]), &responses); err != nil {
		t.Fatalf("attendee_responses not JSON: %v (%q)", err, md["attendee_responses"])
	}
	if responses["bob@example.com"] != "accepted" || responses["carol@example.com"] != "declined" {
		t.Errorf("attendee_responses = %v", responses)
	}

	if doc.GetTs().GetCreated() == nil || doc.GetTs().GetModified() == nil {
		t.Error("ts.created/modified should be set")
	}
	if doc.GetTs().GetIngested() != nil {
		t.Error("ts.ingested must be unset (hub stamps it)")
	}
}

func TestEventDocumentAllDayAndNoSubject(t *testing.T) {
	t.Parallel()
	e := graphEvent{
		ID:        "EVT-2",
		ChangeKey: "only-change-key",
		Subject:   "   ",
		IsAllDay:  true,
		Start:     &dateTimeZone{DateTime: "2026-06-20T00:00:00.0000000", TimeZone: "UTC"},
	}
	doc := eventDocument(tenant, e)
	if doc.GetTitle() != noSubjectTitle {
		t.Errorf("title = %q, want %q", doc.GetTitle(), noSubjectTitle)
	}
	if doc.GetMetadata()["is_all_day"] != "true" {
		t.Errorf("is_all_day = %q, want true", doc.GetMetadata()["is_all_day"])
	}
	if doc.GetVersionEtag() != "only-change-key" {
		t.Errorf("version_etag = %q, want changeKey fallback", doc.GetVersionEtag())
	}
}

func TestVersionEtagBodyHashFallback(t *testing.T) {
	t.Parallel()
	e := graphEvent{ID: "EVT-3", Subject: "Hashed", Body: &itemBody{ContentType: "text", Content: "no etag here"}}
	doc := eventDocument(tenant, e)
	if doc.GetVersionEtag() == "" {
		t.Fatal("version_etag empty; a hash fallback is required")
	}
	// Changing the body must change the etag.
	e2 := e
	e2.Body = &itemBody{ContentType: "text", Content: "different"}
	if eventDocument(tenant, e2).GetVersionEtag() == doc.GetVersionEtag() {
		t.Error("version_etag did not change when body changed")
	}
}

func TestBodyTextPreviewFallback(t *testing.T) {
	t.Parallel()
	e := graphEvent{ID: "EVT-4", BodyPreview: "preview only", Body: &itemBody{ContentType: "text", Content: "   "}}
	if got := bodyText(e); got != "preview only" {
		t.Errorf("bodyText = %q, want preview fallback", got)
	}
}

func TestTombstoneDocument(t *testing.T) {
	t.Parallel()
	c := testConnector()
	e := graphEvent{ID: "EVT-DEL", ODataEtag: `W/"del-etag"`, Removed: &removedReason{Reason: "deleted"}}
	doc := c.tombstoneDocument(tenant, e)

	if !doc.GetTombstone().GetDeleted() {
		t.Error("tombstone.deleted not set")
	}
	if doc.GetTombstone().GetDeletedAt() == nil {
		t.Error("tombstone.deleted_at not set")
	}
	if doc.GetBodyText() != "" || len(doc.GetChunks()) != 0 {
		t.Error("tombstone must carry no body and no chunks")
	}
	if doc.GetVersionEtag() != `W/"del-etag"` {
		t.Errorf("tombstone version_etag = %q, want @odata.etag", doc.GetVersionEtag())
	}
	if doc.GetType() != askerv1.DocType_CALENDAR_EVENT {
		t.Errorf("tombstone type = %v", doc.GetType())
	}
}

func TestTombstoneEtagFallback(t *testing.T) {
	t.Parallel()
	c := testConnector()
	e := graphEvent{ID: "EVT-DEL2", Removed: &removedReason{Reason: "deleted"}}
	doc := c.tombstoneDocument(tenant, e)
	if doc.GetVersionEtag() != "deleted:EVT-DEL2" {
		t.Errorf("tombstone version_etag = %q, want deleted:<id> fallback", doc.GetVersionEtag())
	}
}

func TestParticipantNilWhenEmpty(t *testing.T) {
	t.Parallel()
	e := graphEvent{
		ID:        "EVT-5",
		Organizer: &recipient{EmailAddress: emailAddress{}},
		Attendees: []attendee{{Type: "required", EmailAddress: emailAddress{Name: "Named Only"}}},
	}
	ps := participants(e)
	if len(ps) != 1 {
		t.Fatalf("participants = %d, want 1 (empty organizer dropped)", len(ps))
	}
	if ps[0].GetName() != "Named Only" || ps[0].GetRole() != "required" {
		t.Errorf("participant = %+v", ps[0])
	}
}

func TestAttendeeDefaultRole(t *testing.T) {
	t.Parallel()
	e := graphEvent{ID: "EVT-6", Attendees: []attendee{{EmailAddress: emailAddress{Address: "x@example.com"}}}}
	ps := participants(e)
	if len(ps) != 1 || ps[0].GetRole() != "attendee" {
		t.Fatalf("participant role = %v, want default 'attendee'", ps)
	}
}

func TestParseGraphTime(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"2026-06-01T12:00:00Z":        true,
		"2026-06-15T09:00:00.0000000": true,
		"2026-06-15T09:00:00":         true,
		"":                            false,
		"not-a-time":                  false,
	}
	for in, wantOK := range cases {
		if _, ok := parseGraphTime(in); ok != wantOK {
			t.Errorf("parseGraphTime(%q) ok = %v, want %v", in, ok, wantOK)
		}
	}
}
