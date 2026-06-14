package s3

import (
	"testing"
	"time"

	"github.com/asker/asker/connectors/sdk"
)

func TestCursorRoundTrip(t *testing.T) {
	t.Parallel()
	maxMod := time.Date(2026, 6, 11, 14, 15, 0, 0, time.UTC)
	cur := completedCursor(maxMod, []string{"c", "a", "b"})

	st, err := parseCursor(cur)
	if err != nil {
		t.Fatalf("parseCursor: %v", err)
	}
	if st.LastKey != "" {
		t.Errorf("completed cursor LastKey = %q, want empty", st.LastKey)
	}
	if !st.maxModifiedTime().Equal(maxMod) {
		t.Errorf("maxModifiedTime = %v, want %v", st.maxModifiedTime(), maxMod)
	}
	// Keys are sorted for a stable, diff-able cursor.
	want := []string{"a", "b", "c"}
	if len(st.Keys) != len(want) {
		t.Fatalf("keys = %v, want %v", st.Keys, want)
	}
	for i := range want {
		if st.Keys[i] != want[i] {
			t.Errorf("keys[%d] = %q, want %q", i, st.Keys[i], want[i])
		}
	}
	set := st.keySet()
	if _, ok := set["b"]; !ok {
		t.Error("keySet missing b")
	}
}

func TestCheckpointCursor(t *testing.T) {
	t.Parallel()
	maxMod := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	cur := checkpointCursor("docs/last.txt", maxMod, []string{"docs/a", "docs/last.txt"})
	st, err := parseCursor(cur)
	if err != nil {
		t.Fatalf("parseCursor: %v", err)
	}
	if st.LastKey != "docs/last.txt" {
		t.Errorf("LastKey = %q, want docs/last.txt", st.LastKey)
	}
	if !st.maxModifiedTime().Equal(maxMod) {
		t.Errorf("maxModifiedTime = %v, want %v", st.maxModifiedTime(), maxMod)
	}
}

func TestCompletedCursorZeroTimeOmitsModified(t *testing.T) {
	t.Parallel()
	cur := completedCursor(time.Time{}, []string{"a"})
	st, err := parseCursor(cur)
	if err != nil {
		t.Fatalf("parseCursor: %v", err)
	}
	if st.MaxModified != "" {
		t.Errorf("MaxModified = %q, want empty for zero time", st.MaxModified)
	}
	if !st.maxModifiedTime().IsZero() {
		t.Errorf("maxModifiedTime = %v, want zero", st.maxModifiedTime())
	}
}

func TestParseCursorErrors(t *testing.T) {
	t.Parallel()
	if _, err := parseCursor(sdk.Cursor("")); err == nil {
		t.Error("empty cursor: expected error")
	}
	if _, err := parseCursor(sdk.Cursor("not json")); err == nil {
		t.Error("non-json cursor: expected error")
	}
}

func TestMaxModifiedTimeInvalid(t *testing.T) {
	t.Parallel()
	st := cursorState{MaxModified: "garbage"}
	if !st.maxModifiedTime().IsZero() {
		t.Errorf("maxModifiedTime(garbage) = %v, want zero", st.maxModifiedTime())
	}
}
