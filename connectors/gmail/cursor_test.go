package gmail

import (
	"testing"

	"github.com/asker/asker/connectors/sdk"
)

func TestCursorRoundTrip(t *testing.T) {
	t.Parallel()

	cur := incrementalCursor(42)
	if cur != "history:42" {
		t.Errorf("incrementalCursor(42) = %q, want %q", cur, "history:42")
	}
	st, err := parseCursor(cur)
	if err != nil {
		t.Fatalf("parseCursor(%q): %v", cur, err)
	}
	if st.backfill || st.historyID != 42 || st.pageToken != "" {
		t.Errorf("parseCursor(%q) = %+v, want incremental at 42", cur, st)
	}

	cur = backfillCursor(42, "page-7")
	if cur != "history:42|page:page-7" {
		t.Errorf("backfillCursor = %q, want %q", cur, "history:42|page:page-7")
	}
	st, err = parseCursor(cur)
	if err != nil {
		t.Fatalf("parseCursor(%q): %v", cur, err)
	}
	if !st.backfill || st.historyID != 42 || st.pageToken != "page-7" {
		t.Errorf("parseCursor(%q) = %+v, want backfill at 42/page-7", cur, st)
	}
}

func TestParseCursorRejectsGarbage(t *testing.T) {
	t.Parallel()
	for _, cur := range []sdk.Cursor{
		"",
		"42",
		"history:",
		"history:abc",
		"history:1|page:",
		"hist0ry:1",
		"history:-1",
	} {
		if _, err := parseCursor(cur); err == nil {
			t.Errorf("parseCursor(%q) accepted a malformed cursor", cur)
		}
	}
}
