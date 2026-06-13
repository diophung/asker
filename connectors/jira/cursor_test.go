package jira

import (
	"testing"

	"github.com/asker/asker/connectors/sdk"
)

func TestParseCursorRoundTrip(t *testing.T) {
	t.Parallel()
	cur := makeCursor("2026/06/11 14:45", "DEMO-3")
	st, err := parseCursor(cur)
	if err != nil {
		t.Fatalf("parseCursor(%q): %v", cur, err)
	}
	if st.updatedJQL != "2026/06/11 14:45" {
		t.Errorf("updatedJQL = %q, want %q", st.updatedJQL, "2026/06/11 14:45")
	}
	if st.boundaryKey != "DEMO-3" {
		t.Errorf("boundaryKey = %q, want DEMO-3", st.boundaryKey)
	}
}

func TestParseCursorEmptyIsBeginning(t *testing.T) {
	t.Parallel()
	st, err := parseCursor("")
	if err != nil {
		t.Fatalf("parseCursor(empty): %v", err)
	}
	if st.updatedJQL != "" {
		t.Errorf("empty cursor produced bound %q", st.updatedJQL)
	}
	if st.jql() != "order by updated asc" {
		t.Errorf("empty cursor jql = %q, want %q", st.jql(), "order by updated asc")
	}
}

func TestParseCursorRejectsGarbage(t *testing.T) {
	t.Parallel()
	bad := []sdk.Cursor{
		"bogus",
		"updated:2026/06/11 14:45", // missing |key:
		"updated:|key:DEMO-1",      // empty bound
		"updated:not-a-time|key:X", // malformed bound
		"key:DEMO-1",               // wrong prefix
	}
	for _, cur := range bad {
		if _, err := parseCursor(cur); err == nil {
			t.Errorf("parseCursor(%q) accepted a malformed cursor", cur)
		}
	}
}

func TestCursorForIssue(t *testing.T) {
	t.Parallel()
	iss := &issue{Key: "DEMO-7", Fields: issueFields{Updated: "2026-06-12T16:00:00.000+0000"}}
	cur, ok := cursorForIssue(iss)
	if !ok {
		t.Fatal("cursorForIssue returned ok=false for a valid issue")
	}
	if want := makeCursor("2026/06/12 16:00", "DEMO-7"); cur != want {
		t.Errorf("cursor = %q, want %q", cur, want)
	}

	bad := &issue{Key: "DEMO-8", Fields: issueFields{Updated: "garbage"}}
	if _, ok := cursorForIssue(bad); ok {
		t.Error("cursorForIssue returned ok=true for an unparseable updated time")
	}
}

func TestCursorJQL(t *testing.T) {
	t.Parallel()
	st := cursorState{updatedJQL: "2026/06/10 10:30", boundaryKey: "DEMO-2"}
	want := "updated >= '2026/06/10 10:30' order by updated asc"
	if got := st.jql(); got != want {
		t.Errorf("jql() = %q, want %q", got, want)
	}
}
