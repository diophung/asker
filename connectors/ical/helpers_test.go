package ical

import (
	"testing"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// docTypeCalendarEvent returns the expected DocType for an event document.
func docTypeCalendarEvent() askerv1.DocType { return askerv1.DocType_CALENDAR_EVENT }

// wantDocID is the doc_id the connector produces for (feedURL, uid).
func wantDocID(feedURL, uid string) string {
	return sdk.DocID(connectorID, nativeID(feedURL, uid))
}

// indexByDocID maps emitted documents by their doc_id for lookup.
func indexByDocID(docs []*askerv1.Document) map[string]*askerv1.Document {
	out := make(map[string]*askerv1.Document, len(docs))
	for _, d := range docs {
		out[d.GetDocId()] = d
	}
	return out
}

// assertParticipant checks the i-th participant of doc has the wanted fields.
func assertParticipant(t *testing.T, doc *askerv1.Document, i int, name, email, role string) {
	t.Helper()
	ps := doc.GetParticipants()
	if i >= len(ps) {
		t.Fatalf("doc %q has %d participants, want at least %d", doc.GetDocId(), len(ps), i+1)
	}
	p := ps[i]
	if p.GetName() != name || p.GetEmail() != email || p.GetRole() != role {
		t.Errorf("participant[%d] = {name:%q email:%q role:%q}, want {name:%q email:%q role:%q}",
			i, p.GetName(), p.GetEmail(), p.GetRole(), name, email, role)
	}
}

// splitIDs partitions docs into live and tombstone doc-id slices.
func splitIDs(docs []*askerv1.Document) (live, tomb []string) {
	for _, d := range docs {
		if d.GetTombstone().GetDeleted() {
			tomb = append(tomb, d.GetDocId())
		} else {
			live = append(live, d.GetDocId())
		}
	}
	return live, tomb
}

// assertIDSet checks got is exactly the key set of want.
func assertIDSet(t *testing.T, label string, got []string, want map[string]bool) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: got %d ids %v, want %d", label, len(got), got, len(want))
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("%s: unexpected doc_id %q", label, id)
		}
	}
	seen := make(map[string]bool, len(got))
	for _, id := range got {
		seen[id] = true
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("%s: missing expected doc_id %q", label, id)
		}
	}
}
