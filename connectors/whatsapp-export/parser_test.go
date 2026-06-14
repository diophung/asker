package whatsappexport

import (
	"testing"
	"time"
)

func TestParseExportBracketFormat(t *testing.T) {
	t.Parallel()
	body := "[2024-03-15, 9:42:13 AM] Alice: hello world\n" +
		"[2024-03-15, 9:43:00 AM] Bob: hi back\n"
	msgs := parseExport(body)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].sender != "Alice" || msgs[0].text != "hello world" {
		t.Errorf("msg0 = %+v, want sender Alice text 'hello world'", msgs[0])
	}
	if msgs[0].format != "bracket" {
		t.Errorf("msg0 format = %q, want bracket", msgs[0].format)
	}
	if !msgs[0].hasTime {
		t.Error("msg0 should have a parsed time")
	}
	want := time.Date(2024, 3, 15, 9, 42, 13, 0, time.UTC)
	if !msgs[0].sentAt.Equal(want) {
		t.Errorf("msg0 sentAt = %v, want %v", msgs[0].sentAt, want)
	}
	if msgs[1].lineIndex != 1 {
		t.Errorf("msg1 lineIndex = %d, want 1", msgs[1].lineIndex)
	}
}

func TestParseExportDashFormat(t *testing.T) {
	t.Parallel()
	body := "3/15/24, 09:42 - Alice: hello\n" +
		"3/15/24, 9:43 PM - Bob: evening\n"
	msgs := parseExport(body)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].sender != "Alice" || msgs[0].text != "hello" {
		t.Errorf("msg0 = %+v", msgs[0])
	}
	if msgs[0].format != "dash" {
		t.Errorf("msg0 format = %q, want dash", msgs[0].format)
	}
	if got := msgs[0].sentAt; !got.Equal(time.Date(2024, 3, 15, 9, 42, 0, 0, time.UTC)) {
		t.Errorf("msg0 24h time = %v", got)
	}
	if got := msgs[1].sentAt; !got.Equal(time.Date(2024, 3, 15, 21, 43, 0, 0, time.UTC)) {
		t.Errorf("msg1 12h time = %v, want 21:43", got)
	}
}

func TestParseExportMultiLine(t *testing.T) {
	t.Parallel()
	body := "[2024-03-15, 9:42:13 AM] Alice: line one\nline two\nline three\n" +
		"[2024-03-15, 9:43:00 AM] Bob: single\n"
	msgs := parseExport(body)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (continuation lines must not start a new message)", len(msgs))
	}
	if msgs[0].text != "line one\nline two\nline three" {
		t.Errorf("multi-line body = %q", msgs[0].text)
	}
}

func TestParseExportSystemLines(t *testing.T) {
	t.Parallel()
	body := "[2024-03-15, 9:00:00 AM] Messages and calls are end-to-end encrypted.\n" +
		"[2024-03-15, 9:01:00 AM] Alice added Bob\n" +
		"[2024-03-15, 9:42:13 AM] Alice: hello\n"
	msgs := parseExport(body)
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	if !msgs[0].system || msgs[0].sender != "" {
		t.Errorf("msg0 should be a participant-less system line, got %+v", msgs[0])
	}
	if msgs[0].text != "Messages and calls are end-to-end encrypted." {
		t.Errorf("system text = %q", msgs[0].text)
	}
	if !msgs[1].system {
		t.Errorf("'Alice added Bob' should be a system line, got %+v", msgs[1])
	}
	if msgs[2].system {
		t.Error("a normal 'Name: text' line must not be a system line")
	}
}

func TestParseExportEmojiUnicodeAndMedia(t *testing.T) {
	t.Parallel()
	body := "[2024-03-15, 9:42:13 AM] Alice: Hey \U0001F44B café ☕\n" +
		"[2024-03-15, 9:43:00 AM] Bob: <Media omitted>\n"
	msgs := parseExport(body)
	if len(msgs) != 2 {
		t.Fatalf("got %d, want 2", len(msgs))
	}
	if msgs[0].text != "Hey \U0001F44B café ☕" {
		t.Errorf("emoji/unicode body = %q", msgs[0].text)
	}
	if msgs[1].text != "<Media omitted>" {
		t.Errorf("media placeholder body = %q", msgs[1].text)
	}
}

func TestParseExportColonInBodyNotASender(t *testing.T) {
	t.Parallel()
	// A continuation line containing " - " or ": " must not be mistaken for a
	// new header; only a line whose leading token parses as a timestamp starts a
	// message.
	body := "[2024-03-15, 9:42:13 AM] Alice: meeting at 3:30 - bring laptop\n"
	msgs := parseExport(body)
	if len(msgs) != 1 {
		t.Fatalf("got %d, want 1", len(msgs))
	}
	if msgs[0].sender != "Alice" {
		t.Errorf("sender = %q, want Alice", msgs[0].sender)
	}
	if msgs[0].text != "meeting at 3:30 - bring laptop" {
		t.Errorf("body = %q", msgs[0].text)
	}
}

func TestParseExportDashBodyWithDashSeparator(t *testing.T) {
	t.Parallel()
	// A dash-format message body that itself contains " - " must keep the full
	// body (split only on the first separator, and only when the left side is a
	// timestamp).
	body := "3/15/24, 09:42 - Alice: pros - cons - tradeoffs\n"
	msgs := parseExport(body)
	if len(msgs) != 1 {
		t.Fatalf("got %d, want 1", len(msgs))
	}
	if msgs[0].text != "pros - cons - tradeoffs" {
		t.Errorf("body = %q", msgs[0].text)
	}
}

func TestParseExportCRLF(t *testing.T) {
	t.Parallel()
	body := "[2024-03-15, 9:42:13 AM] Alice: a\r\n[2024-03-15, 9:43:00 AM] Bob: b\r\n"
	msgs := parseExport(body)
	if len(msgs) != 2 {
		t.Fatalf("CRLF: got %d, want 2", len(msgs))
	}
	if msgs[0].text != "a" || msgs[1].text != "b" {
		t.Errorf("CRLF bodies = %q, %q", msgs[0].text, msgs[1].text)
	}
}

func TestParseExportDirectionMarks(t *testing.T) {
	t.Parallel()
	// Some exports wrap the timestamp in an invisible LRM (U+200E). The header
	// must still match.
	body := "\u200e[2024-03-15, 9:42:13 AM] Alice: marked line\n"
	msgs := parseExport(body)
	if len(msgs) != 1 {
		t.Fatalf("got %d, want 1 (LRM-prefixed header must still parse)", len(msgs))
	}
	if msgs[0].sender != "Alice" || msgs[0].text != "marked line" {
		t.Errorf("msg0 = %+v", msgs[0])
	}
}

func TestParseExportNarrowNoBreakSpaceTime(t *testing.T) {
	t.Parallel()
	// iOS exports put a narrow no-break space (U+202F) before AM/PM, and some
	// put a no-break space (U+00A0) elsewhere; both must still parse.
	body := "[2024-03-15, 9:42:13\u202fAM] Alice: nnbsp time\n" +
		"[2024-03-15,\u00a09:43:00 AM] Bob: nbsp date\n"
	msgs := parseExport(body)
	if len(msgs) != 2 {
		t.Fatalf("got %d, want 2", len(msgs))
	}
	if !msgs[0].hasTime || !msgs[1].hasTime {
		t.Errorf("U+202F/U+00A0 times should still parse: %+v", msgs)
	}
	if want := time.Date(2024, 3, 15, 9, 42, 13, 0, time.UTC); !msgs[0].sentAt.Equal(want) {
		t.Errorf("U+202F time = %v, want %v", msgs[0].sentAt, want)
	}
}

func TestParseExportUnknownLocaleStillEmits(t *testing.T) {
	t.Parallel()
	// A header whose date locale we do not recognize is NOT a header at all
	// (parseTimestamp fails), so the line becomes a continuation. With no prior
	// message it is dropped. Confirm we do not panic and emit nothing.
	body := "[15.03.2024 klockan 09:42] Alice: swedish locale\n"
	msgs := parseExport(body)
	if len(msgs) != 0 {
		t.Errorf("unrecognized header parsed as %d messages: %+v", len(msgs), msgs)
	}
}

func TestParseExportEmpty(t *testing.T) {
	t.Parallel()
	if msgs := parseExport(""); len(msgs) != 0 {
		t.Errorf("empty export produced %d messages", len(msgs))
	}
}
