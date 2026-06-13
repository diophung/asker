package whatsappexport

import (
	"strings"
	"testing"
	"time"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

func TestToDocumentNormalMessage(t *testing.T) {
	t.Parallel()
	m := message{
		sender: "Alice", text: "hello world", rawSentAt: "2024-03-15, 9:42:13 AM",
		sentAt: time.Date(2024, 3, 15, 9, 42, 13, 0, time.UTC), hasTime: true,
		lineIndex: 1, format: "bracket",
	}
	doc := toDocument("tenant-a", "Falcon", m)

	if doc.GetType() != askerv1.DocType_CHAT_MESSAGE {
		t.Errorf("type = %v, want CHAT_MESSAGE", doc.GetType())
	}
	if doc.GetTenantId() != "tenant-a" {
		t.Errorf("tenant = %q", doc.GetTenantId())
	}
	if doc.GetBodyText() != "hello world" {
		t.Errorf("body = %q", doc.GetBodyText())
	}
	if doc.GetTitle() != "hello world" {
		t.Errorf("title = %q", doc.GetTitle())
	}
	if doc.GetVersionEtag() == "" {
		t.Error("version_etag empty")
	}
	if doc.GetTs().GetCreated() == nil {
		t.Error("ts.created missing")
	}
	if doc.GetTs().GetIngested() != nil {
		t.Error("ts.ingested set; the hub stamps it")
	}
	ps := doc.GetParticipants()
	if len(ps) != 1 || ps[0].GetName() != "Alice" || ps[0].GetRole() != "sender" || ps[0].GetHandle() != "Alice" {
		t.Errorf("participants = %+v, want one sender Alice", ps)
	}
	md := doc.GetMetadata()
	if md["chat_name"] != "Falcon" || md["sender"] != "Alice" || md["line_index"] != "1" ||
		md["msg_format"] != "bracket" || md["sent_at"] != "2024-03-15, 9:42:13 AM" {
		t.Errorf("metadata = %+v", md)
	}
}

func TestToDocumentSystemMessageHasNoParticipant(t *testing.T) {
	t.Parallel()
	m := message{text: "Carol added Dave", rawSentAt: "2024-03-15, 9:46:00 AM", lineIndex: 4, system: true, format: "bracket"}
	doc := toDocument("tenant-a", "Falcon", m)
	if len(doc.GetParticipants()) != 0 {
		t.Errorf("system message has participants: %+v", doc.GetParticipants())
	}
	if doc.GetMetadata()["sender"] != systemSender {
		t.Errorf("system sender metadata = %q, want %q", doc.GetMetadata()["sender"], systemSender)
	}
	if doc.GetBodyText() != "Carol added Dave" {
		t.Errorf("system body = %q", doc.GetBodyText())
	}
}

func TestToDocumentNoTimeOmitsTimestamp(t *testing.T) {
	t.Parallel()
	m := message{sender: "Alice", text: "x", hasTime: false, lineIndex: 0}
	doc := toDocument("tenant-a", "Falcon", m)
	if doc.GetTs() != nil {
		t.Errorf("ts set despite unparsed time: %+v", doc.GetTs())
	}
}

func TestTitleTruncationAndFallback(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 100)
	got := title("Falcon", long)
	if n := len([]rune(got)); n != titleMaxRunes {
		t.Errorf("long title len = %d runes, want %d", n, titleMaxRunes)
	}

	// Multi-line text collapses to a single line for the title.
	if got := title("Falcon", "line one\nline two"); got != "line one line two" {
		t.Errorf("multi-line title = %q", got)
	}

	// Empty text falls back to "<chat> message".
	if got := title("Falcon", "   "); got != "Falcon message" {
		t.Errorf("empty title = %q, want 'Falcon message'", got)
	}
	if got := title("", ""); got != "message" {
		t.Errorf("empty title with no chat = %q, want 'message'", got)
	}

	// A multi-byte rune straddling the cap is not split.
	multi := strings.Repeat("a", titleMaxRunes-1) + "é more"
	gotMulti := title("c", multi)
	if !strings.HasSuffix(gotMulti, "é") {
		t.Errorf("rune-boundary title = %q, want to end at 'é'", gotMulti)
	}
}

func TestNativeIDStableAndDistinct(t *testing.T) {
	t.Parallel()
	const (
		chat   = "Falcon"
		raw    = "2024-03-15, 9:42:13 AM"
		sender = "Alice"
	)
	a := nativeID(chat, raw, sender, "hello", 0)
	b := nativeID(chat, raw, sender, "hello", 0)
	if a != b {
		t.Errorf("nativeID not stable: %q vs %q", a, b)
	}
	if nativeID(chat, raw, sender, "hello", 0) == nativeID(chat, raw, sender, "world", 0) {
		t.Error("different text produced the same native id")
	}
	if nativeID(chat, raw, sender, "hello", 0) == nativeID("Other", raw, sender, "hello", 0) {
		t.Error("different chat produced the same native id")
	}
	if nativeID(chat, raw, sender, "hello", 0) == nativeID(chat, "2024-03-15, 9:43:00 AM", sender, "hello", 0) {
		t.Error("different timestamp produced the same native id")
	}
	if nativeID(chat, raw, sender, "hello", 0) == nativeID(chat, raw, "Bob", "hello", 0) {
		t.Error("different sender produced the same native id")
	}

	// The id is POSITION-INDEPENDENT: it takes no line index, so the same message
	// gets the same id no matter where it sits in the export. This is the whole
	// point of the fix — a middle insert/delete must not renumber later ids.

	// An exact duplicate (same chat+timestamp+sender+text) is disambiguated ONLY
	// by the occurrence index, so two identical messages still get distinct ids.
	if nativeID(chat, raw, sender, "hello", 0) == nativeID(chat, raw, sender, "hello", 1) {
		t.Error("duplicate occurrences collided; occurrence index must distinguish them")
	}
}

// docIDsByText maps each message's body text to its emitted doc_id, by running
// the full parse -> toDocument path the connector uses. It is the regression
// harness for the "doc_id must not embed line index" fix.
func docIDsByText(chat, body string) map[string]string {
	msgs := parseExport(body)
	out := make(map[string]string, len(msgs))
	for _, m := range msgs {
		out[m.text] = toDocument("tenant-a", chat, m).GetDocId()
	}
	return out
}

// TestDocIDStableAcrossMiddleInsert is the regression test for the MAJOR finding:
// the old scheme embedded lineIndex in the doc_id, so inserting a message in the
// MIDDLE of a re-exported chat shifted every later message's line index and thus
// every later doc_id — orphaning the already-indexed documents and re-emitting
// duplicates. With the position-independent id, inserting a middle message must
// leave the doc_id of every message AFTER the insertion point UNCHANGED.
func TestDocIDStableAcrossMiddleInsert(t *testing.T) {
	t.Parallel()
	const chat = "Falcon"
	before := "[2024-03-15, 9:00:00 AM] Alice: first\n" +
		"[2024-03-15, 9:01:00 AM] Bob: second\n" +
		"[2024-03-15, 9:02:00 AM] Alice: third\n"
	// Same chat, re-exported with a NEW message inserted between "first" and
	// "second" (so "second" and "third" shift from index 1,2 to index 2,3).
	after := "[2024-03-15, 9:00:00 AM] Alice: first\n" +
		"[2024-03-15, 9:00:30 AM] Bob: inserted in the middle\n" +
		"[2024-03-15, 9:01:00 AM] Bob: second\n" +
		"[2024-03-15, 9:02:00 AM] Alice: third\n"

	idsBefore := docIDsByText(chat, before)
	idsAfter := docIDsByText(chat, after)

	// The messages that existed before the insert keep their doc_id even though
	// their line index shifted. The old (lineIndex-based) scheme failed here.
	for _, text := range []string{"first", "second", "third"} {
		if idsBefore[text] != idsAfter[text] {
			t.Errorf("doc_id for %q changed after a middle insert: %q -> %q (a middle insert must not renumber later ids)",
				text, idsBefore[text], idsAfter[text])
		}
	}
	// The inserted message is genuinely new (no pre-insert id to collide with).
	if idsAfter["inserted in the middle"] == "" {
		t.Fatal("inserted message produced no doc_id")
	}
	for text, id := range idsBefore {
		if id == idsAfter["inserted in the middle"] {
			t.Errorf("inserted message reused the doc_id of existing message %q", text)
		}
	}
}

// TestDocIDStableAcrossMiddleDelete is the delete-side mirror: deleting a middle
// message shifts later line indices but must not change any surviving message's
// doc_id.
func TestDocIDStableAcrossMiddleDelete(t *testing.T) {
	t.Parallel()
	const chat = "Falcon"
	before := "[2024-03-15, 9:00:00 AM] Alice: keep one\n" +
		"[2024-03-15, 9:01:00 AM] Bob: delete me\n" +
		"[2024-03-15, 9:02:00 AM] Alice: keep two\n"
	after := "[2024-03-15, 9:00:00 AM] Alice: keep one\n" +
		"[2024-03-15, 9:02:00 AM] Alice: keep two\n"

	idsBefore := docIDsByText(chat, before)
	idsAfter := docIDsByText(chat, after)
	for _, text := range []string{"keep one", "keep two"} {
		if idsBefore[text] != idsAfter[text] {
			t.Errorf("doc_id for %q changed after a middle delete: %q -> %q",
				text, idsBefore[text], idsAfter[text])
		}
	}
}

// TestDocIDDuplicateMessagesAreDistinctAndStable checks the legitimate-duplicate
// case: the same sender sending the byte-for-byte same text at the same timestamp
// twice must yield TWO distinct, stable doc_ids (via the occurrence index), and a
// middle insert before the duplicates must not change either of their ids.
func TestDocIDDuplicateMessagesAreDistinctAndStable(t *testing.T) {
	t.Parallel()
	const chat = "Falcon"
	// Two exact-duplicate "ok" messages (same sender, timestamp, text).
	body := "[2024-03-15, 9:00:00 AM] Alice: hi\n" +
		"[2024-03-15, 9:01:00 AM] Bob: ok\n" +
		"[2024-03-15, 9:01:00 AM] Bob: ok\n"
	msgs := parseExport(body)
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	dup0 := toDocument("tenant-a", chat, msgs[1]).GetDocId()
	dup1 := toDocument("tenant-a", chat, msgs[2]).GetDocId()
	if dup0 == dup1 {
		t.Fatal("two exact-duplicate messages collapsed to one doc_id; the occurrence index must split them")
	}
	if msgs[1].occurrence != 0 || msgs[2].occurrence != 1 {
		t.Errorf("duplicate occurrences = %d,%d, want 0,1", msgs[1].occurrence, msgs[2].occurrence)
	}

	// Insert a NEW message before the duplicates; both duplicate ids must survive.
	withInsert := "[2024-03-15, 9:00:00 AM] Alice: hi\n" +
		"[2024-03-15, 9:00:30 AM] Alice: inserted\n" +
		"[2024-03-15, 9:01:00 AM] Bob: ok\n" +
		"[2024-03-15, 9:01:00 AM] Bob: ok\n"
	after := parseExport(withInsert)
	if len(after) != 4 {
		t.Fatalf("got %d messages after insert, want 4", len(after))
	}
	if got := toDocument("tenant-a", chat, after[2]).GetDocId(); got != dup0 {
		t.Errorf("first duplicate doc_id changed after a middle insert: %q -> %q", dup0, got)
	}
	if got := toDocument("tenant-a", chat, after[3]).GetDocId(); got != dup1 {
		t.Errorf("second duplicate doc_id changed after a middle insert: %q -> %q", dup1, got)
	}
}

func TestMessageEtagChangesWithContent(t *testing.T) {
	t.Parallel()
	base := message{sender: "Alice", text: "hi", rawSentAt: "t"}
	if messageEtag(base) == "" {
		t.Fatal("empty etag")
	}
	if messageEtag(base) == messageEtag(message{sender: "Alice", text: "HI", rawSentAt: "t"}) {
		t.Error("etag unchanged after text edit")
	}
	if messageEtag(base) == messageEtag(message{sender: "Bob", text: "hi", rawSentAt: "t"}) {
		t.Error("etag unchanged after sender change")
	}
}
