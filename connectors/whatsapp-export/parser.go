package whatsappexport

import (
	"strings"
	"time"
)

// message is one parsed logical WhatsApp message: a sender (empty for a system
// line), the timestamp the export recorded, and the full text (continuation
// lines joined with "\n"). lineIndex is the message's ordinal position in the
// export (0-based), kept for diagnostics/metadata only — it is deliberately NOT
// part of the source-native id (a middle insert/delete shifts every later index
// but must not renumber every later doc_id). occurrence disambiguates EXACT
// duplicate messages (same sender+timestamp+text) within one export: it is the
// 0-based count of identical messages seen before this one, so two byte-for-byte
// identical messages still get distinct, position-independent ids. format records
// which header shape matched ("bracket" or "dash") for diagnostics.
type message struct {
	sender     string
	text       string
	sentAt     time.Time
	hasTime    bool // sentAt parsed successfully
	rawSentAt  string
	lineIndex  int
	occurrence int
	system     bool
	format     string
}

// headerLayouts are the timestamp+sender header shapes WhatsApp's "Export chat"
// produces, in the two common families:
//
//   - bracket: "[2024-03-15, 9:42:13 AM] Alice: text"
//   - dash:    "3/15/24, 09:42 - Alice: text"
//
// A logical message starts at a line that matches one of these; any following
// line that does NOT start a new header is a continuation of the current
// message (multi-line messages, e.g. text with embedded newlines).
//
// System lines ("Messages and calls are end-to-end encrypted", "Alice added
// Bob", "Alice created group …") carry a timestamp header but no "Name: "
// segment, so they are detected by the absence of the sender separator and
// emitted as participant-less SYSTEM messages (see splitSenderText).

// timeLayouts are the time.Parse layouts tried for the bracketed timestamp body
// (the part between the brackets). Both 12h (AM/PM) and 24h variants appear, as
// do with/without seconds. The date portion varies by phone locale; the common
// exports use yyyy-mm-dd or m/d/yy(yy).
var bracketTimeLayouts = []string{
	"2006-01-02, 3:04:05 PM",
	"2006-01-02, 15:04:05",
	"2006-01-02, 3:04 PM",
	"2006-01-02, 15:04",
	"1/2/06, 3:04:05 PM",
	"1/2/06, 15:04:05",
	"1/2/06, 3:04 PM",
	"1/2/06, 15:04",
	"1/2/2006, 3:04:05 PM",
	"1/2/2006, 15:04:05",
	"1/2/2006, 3:04 PM",
	"1/2/2006, 15:04",
	"02/01/2006, 3:04:05 PM",
	"02/01/2006, 15:04:05",
}

// dashTimeLayouts are the layouts tried for the dash-variant timestamp (the part
// before the " - " separator).
var dashTimeLayouts = []string{
	"1/2/06, 3:04 PM",
	"1/2/06, 15:04",
	"1/2/06, 3:04:05 PM",
	"1/2/06, 15:04:05",
	"1/2/2006, 3:04 PM",
	"1/2/2006, 15:04",
	"2006-01-02, 15:04",
	"2006-01-02, 3:04 PM",
	"02/01/2006, 15:04",
}

// parseExport parses a full _chat.txt export body into ordered messages.
//
// The algorithm is line-oriented: a line that begins a new message (its leading
// token parses as a timestamp header) starts a fresh message; any other line is
// appended to the current message's text as a continuation. WhatsApp inserts an
// invisible LRM/RLM/U+200E around the timestamp in some exports, so those marks
// are stripped before matching.
//
// System lines (a timestamp header with no "Name: " sender segment) become
// system messages with an empty sender; the caller decides to emit them as a
// participant-less SYSTEM message (documented in the README — we emit, not
// skip, so a search for "end-to-end encrypted" or "added Bob" still hits).
func parseExport(body string) []message {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	// A file's trailing newline is a line terminator, not a blank continuation
	// line; trim exactly one so the last message keeps no spurious trailing "\n".
	// Internal blank lines (legitimate inside a multi-line message) are kept.
	body = strings.TrimSuffix(body, "\n")
	lines := strings.Split(body, "\n")

	var msgs []message
	var cur *message
	for _, raw := range lines {
		line := stripDirMarks(raw)
		ts, rest, format, ok := parseHeader(line)
		if !ok {
			// Continuation of the current message (or stray preamble before any
			// header, which we drop).
			if cur != nil {
				if cur.text == "" {
					cur.text = raw
				} else {
					cur.text += "\n" + raw
				}
			}
			continue
		}
		// A new message header. Flush nothing here; cur already points into msgs.
		sender, text, isSystem := splitSenderText(rest)
		msgs = append(msgs, message{
			sender:    sender,
			text:      text,
			sentAt:    ts.t,
			hasTime:   ts.ok,
			rawSentAt: ts.raw,
			lineIndex: len(msgs),
			system:    isSystem,
			format:    format,
		})
		cur = &msgs[len(msgs)-1]
	}
	assignOccurrences(msgs)
	return msgs
}

// assignOccurrences stamps each message's occurrence index: the 0-based count of
// byte-for-byte identical messages (same identity tuple as nativeID) seen earlier
// in the export. This makes a legitimate duplicate (the same person sending the
// same text at the same timestamp twice) resolve to a distinct, stable id, while
// a unique message always keeps occurrence 0 regardless of how many UNRELATED
// messages precede it — so a middle insert/delete cannot renumber it.
func assignOccurrences(msgs []message) {
	seen := make(map[string]int, len(msgs))
	for i := range msgs {
		key := identityKey(msgs[i])
		msgs[i].occurrence = seen[key]
		seen[key]++
	}
}

// identityKey is the position-independent identity of a message: the same
// content/identity tuple nativeID hashes (sender, raw timestamp, text), with
// sender and text sanitized to valid UTF-8 exactly as toDocument sanitizes them
// before hashing. Two messages share a key iff they are exact duplicates that
// the occurrence index must disambiguate — and iff they would otherwise produce
// the same nativeID hash, so the occurrence count tracks real id collisions.
func identityKey(m message) string {
	return strings.ToValidUTF8(m.sender, "") + "\x00" +
		m.rawSentAt + "\x00" +
		strings.ToValidUTF8(m.text, "")
}

// parsedTime carries a parsed timestamp plus the raw header text and whether the
// parse succeeded (an unrecognized date locale still yields a message, just
// without ts.created).
type parsedTime struct {
	t   time.Time
	ok  bool
	raw string
}

// parseHeader recognizes the two header families and returns the parsed
// timestamp, the remainder of the line ("Name: text" or a system phrase), the
// matched format name, and whether a header was found at all.
func parseHeader(line string) (parsedTime, string, string, bool) {
	if ts, rest, ok := parseBracketHeader(line); ok {
		return ts, rest, "bracket", true
	}
	if ts, rest, ok := parseDashHeader(line); ok {
		return ts, rest, "dash", true
	}
	return parsedTime{}, "", "", false
}

// parseBracketHeader matches "[<timestamp>] <rest>".
func parseBracketHeader(line string) (parsedTime, string, bool) {
	if len(line) == 0 || line[0] != '[' {
		return parsedTime{}, "", false
	}
	end := strings.IndexByte(line, ']')
	if end < 0 {
		return parsedTime{}, "", false
	}
	inner := strings.TrimSpace(line[1:end])
	rest := strings.TrimSpace(line[end+1:])
	t, ok := parseTimestamp(inner, bracketTimeLayouts)
	if !ok {
		return parsedTime{}, "", false
	}
	return parsedTime{t: t, ok: true, raw: inner}, rest, true
}

// parseDashHeader matches "<timestamp> - <rest>". The dash separator is " - "
// (space-hyphen-space); the timestamp is everything before the FIRST such
// separator, and it must itself parse as a date+time so a message body that
// merely contains " - " is not mistaken for a header.
func parseDashHeader(line string) (parsedTime, string, bool) {
	sep := strings.Index(line, " - ")
	if sep < 0 {
		return parsedTime{}, "", false
	}
	stamp := strings.TrimSpace(line[:sep])
	rest := strings.TrimSpace(line[sep+len(" - "):])
	t, ok := parseTimestamp(stamp, dashTimeLayouts)
	if !ok {
		return parsedTime{}, "", false
	}
	return parsedTime{t: t, ok: true, raw: stamp}, rest, true
}

// parseTimestamp tries each layout against the (whitespace-normalized) stamp.
// WhatsApp sometimes uses a narrow no-break space (U+202F) or a no-break space
// (U+00A0) before AM/PM; any Unicode space is normalized to a plain ASCII space
// (and runs are collapsed) before matching.
func parseTimestamp(stamp string, layouts []string) (time.Time, bool) {
	stamp = strings.Join(strings.Fields(stamp), " ")
	for _, layout := range layouts {
		if t, err := time.Parse(layout, stamp); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// splitSenderText splits a header remainder into sender and message text. A
// normal message is "Name: text"; a system line ("Messages and calls are
// end-to-end encrypted", "Alice added Bob") has no "Name: " segment and yields
// an empty sender with isSystem=true.
//
// The sender is the text before the FIRST ": " (colon-space). A colon inside
// the message body (e.g. "see 3:30") is not a false split because a real sender
// never contains a newline and the body's colon is preceded/followed by content
// rather than starting the line — but to stay robust we only treat the segment
// before the first ": " as a sender when it is non-empty and does not itself
// look like a sentence (heuristic: short, no trailing period). To avoid being
// too clever we keep it simple: split on the first ": " and treat the left side
// as the sender; if there is no ": " at all it is a system line.
func splitSenderText(rest string) (sender, text string, isSystem bool) {
	idx := strings.Index(rest, ": ")
	if idx < 0 {
		// No sender segment: a system notification.
		return "", strings.TrimSpace(rest), true
	}
	sender = strings.TrimSpace(rest[:idx])
	text = rest[idx+len(": "):]
	if sender == "" {
		return "", strings.TrimSpace(rest), true
	}
	return sender, text, false
}

// stripDirMarks removes the Unicode bidirectional control marks (LRM U+200E,
// RLM U+200F, and the isolates) WhatsApp wraps around timestamps and "<Media
// omitted>" placeholders in some exports, so header matching is not defeated by
// an invisible leading mark.
func stripDirMarks(s string) string {
	const marks = "\u200e\u200f\u2066\u2067\u2068\u2069"
	if !strings.ContainsAny(s, marks) {
		return s
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '\u200e', '\u200f', '\u2066', '\u2067', '\u2068', '\u2069':
			return -1
		default:
			return r
		}
	}, s)
}
