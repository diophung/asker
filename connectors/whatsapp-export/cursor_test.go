package whatsappexport

import (
	"strings"
	"testing"

	"github.com/asker/asker/connectors/sdk"
)

func sampleMsgs(texts ...string) []message {
	msgs := make([]message, len(texts))
	for i, txt := range texts {
		msgs[i] = message{sender: "Alice", text: txt, rawSentAt: "2024-03-15, 9:42:13 AM", lineIndex: i}
	}
	return msgs
}

func TestMakeAndParseCursorRoundTrip(t *testing.T) {
	t.Parallel()
	msgs := sampleMsgs("a", "b", "c")
	cur := makeCursor(msgs, len(msgs))
	pc, err := parseCursor(cur)
	if err != nil {
		t.Fatalf("parseCursor(%q): %v", cur, err)
	}
	if pc.count != 3 {
		t.Errorf("count = %d, want 3", pc.count)
	}
	if len(pc.prefixHash) != 64 {
		t.Errorf("prefixHash = %q (len %d), want 64 hex chars", pc.prefixHash, len(pc.prefixHash))
	}
}

func TestParseCursorEmptyIsZero(t *testing.T) {
	t.Parallel()
	pc, err := parseCursor("")
	if err != nil {
		t.Fatalf("parseCursor(\"\"): %v", err)
	}
	if pc.count != 0 || pc.prefixHash != "" {
		t.Errorf("empty cursor = %+v, want zero", pc)
	}
}

func TestParseCursorMalformed(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"notacursor", "abc:def", "3", "-1:" + strings.Repeat("0", 64), "3:short"} {
		if _, err := parseCursor(sdk.Cursor(bad)); err == nil {
			t.Errorf("parseCursor(%q) accepted a malformed cursor", bad)
		}
	}
}

func TestPrefixHashStableAndChanges(t *testing.T) {
	t.Parallel()
	base := sampleMsgs("a", "b", "c", "d")
	// The prefix hash of the first 2 messages must match the cursor built from
	// a 2-message export with the same first two messages.
	first2 := makeCursor(base, 2)
	pc, _ := parseCursor(first2)
	if got := prefixHashOf(base, 2); got != pc.prefixHash {
		t.Errorf("prefixHashOf mismatch: %q vs %q", got, pc.prefixHash)
	}
	// Editing message 0 changes the prefix hash.
	edited := sampleMsgs("EDITED", "b", "c", "d")
	if prefixHashOf(edited, 2) == prefixHashOf(base, 2) {
		t.Error("editing a prefix message did not change the prefix hash")
	}
	// Appending does NOT change the prefix hash of the original count.
	appended := sampleMsgs("a", "b", "c", "d", "e", "f")
	if prefixHashOf(appended, 4) != prefixHashOf(base, 4) {
		t.Error("appending changed the prefix hash of the original messages")
	}
}
