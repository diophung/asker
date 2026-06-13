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
	a := nativeID("Falcon", 3, "hello")
	b := nativeID("Falcon", 3, "hello")
	if a != b {
		t.Errorf("nativeID not stable: %q vs %q", a, b)
	}
	if nativeID("Falcon", 3, "hello") == nativeID("Falcon", 3, "world") {
		t.Error("different text produced the same native id at the same index")
	}
	if nativeID("Falcon", 3, "hello") == nativeID("Other", 3, "hello") {
		t.Error("different chat produced the same native id")
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
