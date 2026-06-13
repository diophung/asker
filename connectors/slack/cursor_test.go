package slack

import (
	"testing"

	"github.com/asker/asker/connectors/sdk"
)

func TestCursorRoundTrip(t *testing.T) {
	t.Parallel()

	st := cursorState{}
	st.advance("C1", "1700000100.000100")
	st.advance("C2", "1700000050.000000")
	// A stale ts must not move the watermark backward.
	st.advance("C1", "1600000000.000000")
	// A newer ts must move it forward.
	st.advance("C1", "1700000200.000200")

	cur := st.encode()
	want := sdk.Cursor(`{"C1":"1700000200.000200","C2":"1700000050.000000"}`)
	if cur != want {
		t.Fatalf("encode() = %q, want %q (sorted, deterministic)", cur, want)
	}

	back, err := parseCursor(cur)
	if err != nil {
		t.Fatalf("parseCursor(%q): %v", cur, err)
	}
	if back["C1"] != "1700000200.000200" || back["C2"] != "1700000050.000000" {
		t.Errorf("parseCursor round-trip = %v", back)
	}
}

func TestParseEmptyCursor(t *testing.T) {
	t.Parallel()
	st, err := parseCursor("")
	if err != nil {
		t.Fatalf("parseCursor(empty): %v", err)
	}
	if len(st) != 0 {
		t.Errorf("empty cursor decoded to %v, want empty map", st)
	}
	if st.encode() != "{}" {
		t.Errorf("empty state encodes to %q, want {}", st.encode())
	}
}

func TestParseCursorRejectsGarbage(t *testing.T) {
	t.Parallel()
	for _, cur := range []sdk.Cursor{
		"not json",
		"[1,2,3]",
		`{"C1":123}`, // value not a string
		`"a string"`,
	} {
		if _, err := parseCursor(cur); err == nil {
			t.Errorf("parseCursor(%q) accepted a malformed cursor", cur)
		}
	}
}
