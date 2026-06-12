package sdk

import (
	"regexp"
	"testing"
)

// TestDocIDGolden pins the doc_id rule: lowercase hex SHA-256 of
// connectorID + ":" + sourceNativeID. These values are part of the on-disk
// contract — if this test breaks, every already-indexed document orphans.
func TestDocIDGolden(t *testing.T) {
	tests := []struct {
		connectorID    string
		sourceNativeID string
		want           string
	}{
		{"gmail", "msg-123", "71c4bdcbc466415aab074999b9fc233bfe48a727f880cecd9700093ef74874ed"},
		{"upload", "report.pdf", "8c35769d670f9edcefb883d3ec55b2870fef9dc65e55c8bd405605facaf97d5b"},
		{"", "", "e7ac0786668e0ff0f02b62bd04f45ff636fd82db63b1104601c975dc005f3a67"}, // sha256(":")
	}
	for _, tt := range tests {
		if got := DocID(tt.connectorID, tt.sourceNativeID); got != tt.want {
			t.Errorf("DocID(%q, %q) = %q, want %q", tt.connectorID, tt.sourceNativeID, got, tt.want)
		}
	}
}

func TestDocIDShape(t *testing.T) {
	hexPattern := regexp.MustCompile(`^[0-9a-f]{64}$`)
	id := DocID("gmail", "msg-123")
	if !hexPattern.MatchString(id) {
		t.Errorf("DocID output %q is not 64 lowercase hex chars", id)
	}
	if again := DocID("gmail", "msg-123"); again != id {
		t.Errorf("DocID is not deterministic: %q != %q", again, id)
	}
}

// TestDocIDDistinct is a collision-resistance sanity check: distinct
// (connector, source) inputs must produce distinct IDs. Note that the rule's
// cross-connector uniqueness relies on connector IDs never containing ":"
// (enforced by the [a-z0-9-]+ spec ID rule), so colon-bearing connector IDs
// are deliberately absent here.
func TestDocIDDistinct(t *testing.T) {
	inputs := []struct{ connectorID, sourceNativeID string }{
		{"gmail", "msg-123"},
		{"gmail", "msg-124"},
		{"slack", "msg-123"},
		{"gmail", "MSG-123"},
		{"upload", ""},
		{"", "upload"},
		{"upload", "a/b"},
		{"upload", "a/c"},
	}
	seen := make(map[string]int, len(inputs))
	for i, in := range inputs {
		id := DocID(in.connectorID, in.sourceNativeID)
		if j, dup := seen[id]; dup {
			t.Errorf("collision: inputs %d %+v and %d %+v both map to %s", i, in, j, inputs[j], id)
		}
		seen[id] = i
	}
}
