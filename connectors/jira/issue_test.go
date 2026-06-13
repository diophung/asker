package jira

import (
	"testing"
	"time"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// sampleIssue is a fully-populated issue for golden mapping assertions.
func sampleIssue() *issue {
	return &issue{
		ID:  "10010",
		Key: "PROJ-42",
		Fields: issueFields{
			Summary: "Payment retries exhaust the queue",
			Description: &adfNode{
				Type: "doc",
				Content: []*adfNode{
					{Type: "paragraph", Content: []*adfNode{{Type: "text", Text: "Retries never back off."}}},
				},
			},
			Status:    &namedRef{Name: "In Progress"},
			IssueType: &namedRef{Name: "Bug"},
			Priority:  &namedRef{Name: "High"},
			Project:   &projectRef{Key: "PROJ", Name: "Payments"},
			Reporter:  &userRef{AccountID: "acc-r", DisplayName: "Rita Reporter", EmailAddress: "rita@example.com"},
			Assignee:  &userRef{AccountID: "acc-a", DisplayName: "Alan Assignee", EmailAddress: "alan@example.com"},
			Creator:   &userRef{AccountID: "acc-c", DisplayName: "Cara Creator", EmailAddress: "cara@example.com"},
			Created:   "2026-06-01T09:00:00.000+0000",
			Updated:   "2026-06-05T12:34:00.000+0000",
			Comment: &commentResult{Comments: []*comment{
				{ID: "c1", Author: &userRef{DisplayName: "Rita Reporter"}, Body: &adfNode{
					Type:    "doc",
					Content: []*adfNode{{Type: "paragraph", Content: []*adfNode{{Type: "text", Text: "Confirmed on staging."}}}},
				}},
			}},
		},
	}
}

func TestIssueDocumentGolden(t *testing.T) {
	t.Parallel()
	const base = "https://acme.atlassian.net"
	doc := issueDocument("tenant-x", base, sampleIssue())

	if doc.GetTenantId() != "tenant-x" {
		t.Errorf("tenant_id = %q, want tenant-x", doc.GetTenantId())
	}
	if doc.GetType() != askerv1.DocType_TICKET {
		t.Errorf("type = %v, want TICKET", doc.GetType())
	}
	if doc.GetConnectorId() != "jira" || doc.GetSourceNativeId() != "PROJ-42" {
		t.Errorf("identity = (%q, %q), want (jira, PROJ-42)", doc.GetConnectorId(), doc.GetSourceNativeId())
	}
	if want := docID("PROJ-42"); doc.GetDocId() != want {
		t.Errorf("doc_id = %q, want %q", doc.GetDocId(), want)
	}
	if want := "PROJ-42: Payment retries exhaust the queue"; doc.GetTitle() != want {
		t.Errorf("title = %q, want %q", doc.GetTitle(), want)
	}
	if want := "Retries never back off.\n\nRita Reporter: Confirmed on staging."; doc.GetBodyText() != want {
		t.Errorf("body = %q, want %q", doc.GetBodyText(), want)
	}
	if want := "2026-06-05T12:34:00.000+0000"; doc.GetVersionEtag() != want {
		t.Errorf("version_etag = %q, want %q", doc.GetVersionEtag(), want)
	}

	wantMeta := map[string]string{
		"issue_key":  "PROJ-42",
		"issue_id":   "10010",
		"project":    "PROJ",
		"status":     "In Progress",
		"issue_type": "Bug",
		"priority":   "High",
		"web_url":    "https://acme.atlassian.net/browse/PROJ-42",
	}
	got := doc.GetMetadata()
	if len(got) != len(wantMeta) {
		t.Errorf("metadata = %v, want %v", got, wantMeta)
	}
	for k, want := range wantMeta {
		if got[k] != want {
			t.Errorf("metadata[%q] = %q, want %q", k, got[k], want)
		}
	}

	wantTs := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	if got := doc.GetTs().GetCreated().AsTime(); !got.Equal(wantTs) {
		t.Errorf("ts.created = %v, want %v", got, wantTs)
	}
	wantMod := time.Date(2026, 6, 5, 12, 34, 0, 0, time.UTC)
	if got := doc.GetTs().GetModified().AsTime(); !got.Equal(wantMod) {
		t.Errorf("ts.modified = %v, want %v", got, wantMod)
	}
	if doc.GetTs().GetIngested() != nil {
		t.Error("ts.ingested set; the hub stamps it")
	}

	wantParts := []*askerv1.Participant{
		{Name: "Rita Reporter", Email: "rita@example.com", Handle: "acc-r", Role: "reporter"},
		{Name: "Alan Assignee", Email: "alan@example.com", Handle: "acc-a", Role: "assignee"},
		{Name: "Cara Creator", Email: "cara@example.com", Handle: "acc-c", Role: "creator"},
	}
	parts := doc.GetParticipants()
	if len(parts) != len(wantParts) {
		t.Fatalf("participants = %v, want %v", parts, wantParts)
	}
	for i, w := range wantParts {
		p := parts[i]
		if p.GetName() != w.GetName() || p.GetEmail() != w.GetEmail() || p.GetHandle() != w.GetHandle() || p.GetRole() != w.GetRole() {
			t.Errorf("participant[%d] = %v, want %v", i, p, w)
		}
	}
}

func TestIssueDocumentTitleWithoutSummary(t *testing.T) {
	t.Parallel()
	iss := &issue{ID: "1", Key: "X-1", Fields: issueFields{Updated: "2026-06-01T00:00:00.000+0000"}}
	doc := issueDocument("t", "https://x.atlassian.net", iss)
	if doc.GetTitle() != "X-1" {
		t.Errorf("title = %q, want %q", doc.GetTitle(), "X-1")
	}
}

func TestIssueParticipantsSkipsMissing(t *testing.T) {
	t.Parallel()
	iss := &issue{
		Key: "X-2",
		Fields: issueFields{
			Reporter: &userRef{DisplayName: "Only Reporter"},
			// assignee nil, creator empty
			Creator: &userRef{},
			Updated: "2026-06-01T00:00:00.000+0000",
		},
	}
	parts := issueParticipants(iss)
	if len(parts) != 1 || parts[0].GetRole() != "reporter" {
		t.Fatalf("participants = %v, want a single reporter", parts)
	}
}

func TestTombstoneDocument(t *testing.T) {
	t.Parallel()
	c := &Connector{now: func() time.Time { return time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC) }}
	iss := &issue{ID: "9", Key: "DEMO-9", Fields: issueFields{Updated: "2026-06-12T16:00:00.000+0000"}}
	doc := c.tombstoneDocument("t", iss)

	if !doc.GetTombstone().GetDeleted() {
		t.Error("tombstone.deleted = false")
	}
	if doc.GetTombstone().GetDeletedAt() == nil {
		t.Error("tombstone deleted_at unset")
	}
	if doc.GetBodyText() != "" || len(doc.GetChunks()) != 0 {
		t.Error("tombstone carries a body")
	}
	if want := docID("DEMO-9"); doc.GetDocId() != want {
		t.Errorf("doc_id = %q, want %q", doc.GetDocId(), want)
	}
	if doc.GetVersionEtag() == "" {
		t.Error("tombstone version_etag empty")
	}
}

func TestParseJiraTime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in string
		ok bool
	}{
		{"2026-06-05T12:34:00.000+0000", true},
		{"2026-06-05T12:34:00+0000", true},
		{"2026-06-05T12:34:00Z", true},
		{"2026-06-05T12:34:00.123456789Z", true},
		{"", false},
		{"not a time", false},
	}
	for _, tt := range tests {
		if _, ok := parseJiraTime(tt.in); ok != tt.ok {
			t.Errorf("parseJiraTime(%q) ok = %v, want %v", tt.in, ok, tt.ok)
		}
	}
}
