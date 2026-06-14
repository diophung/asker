package ical

import (
	"strings"
	"testing"
)

// TestUnfold exercises RFC 5545 line unfolding: a leading space or tab continues
// the previous logical line, with the one whitespace char removed. Both CRLF and
// bare LF endings are accepted.
func TestUnfold(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		in   string
		want []string
	}{
		"space continuation": {
			in:   "DESCRIPTION:Hello\r\n World\r\n",
			want: []string{"DESCRIPTION:HelloWorld", ""},
		},
		"tab continuation": {
			in:   "DESCRIPTION:Hello\r\n\tWorld",
			want: []string{"DESCRIPTION:HelloWorld"},
		},
		"multi-line fold": {
			in:   "X:abc\r\n def\r\n ghi",
			want: []string{"X:abcdefghi"},
		},
		"bare LF endings": {
			in:   "A:1\nB:2\n C\n",
			want: []string{"A:1", "B:2C", ""},
		},
		"leading continuation with no previous line is kept": {
			in:   " orphan",
			want: []string{" orphan"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := unfold(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("unfold(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("line[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestSplitLine checks the name/params/value split, including quoted parameter
// values that contain ':' and ';'.
func TestSplitLine(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		line     string
		name     string
		paramSeg string
		value    string
	}{
		"plain":              {"SUMMARY:Hello", "SUMMARY", "", "Hello"},
		"with param":         {"DTSTART;TZID=America/Los_Angeles:20260603T093000", "DTSTART", "TZID=America/Los_Angeles", "20260603T093000"},
		"value with colon":   {"URL:https://example.com/x", "URL", "", "https://example.com/x"},
		"quoted param colon": {"X;ALT=\"a:b;c\":val", "X", "ALT=\"a:b;c\"", "val"},
		"no value separator": {"BEGIN", "BEGIN", "", ""},
		"empty value":        {"DESCRIPTION:", "DESCRIPTION", "", ""},
		"multiple params":    {"ATTENDEE;CN=Bob;ROLE=CHAIR:mailto:bob@x.com", "ATTENDEE", "CN=Bob;ROLE=CHAIR", "mailto:bob@x.com"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n, ps, v := splitLine(tc.line)
			if n != tc.name || ps != tc.paramSeg || v != tc.value {
				t.Errorf("splitLine(%q) = (%q,%q,%q), want (%q,%q,%q)",
					tc.line, n, ps, v, tc.name, tc.paramSeg, tc.value)
			}
		})
	}
}

// TestParseParams checks parameter parsing, quoting, bare flags, and
// case-insensitive lookup.
func TestParseParams(t *testing.T) {
	t.Parallel()
	p := parseProperty("DTSTART;TZID=\"Europe/Paris\";VALUE=DATE:20260101")
	if v, ok := p.param("tzid"); !ok || v != "Europe/Paris" {
		t.Errorf("param tzid = (%q,%v), want Europe/Paris", v, ok)
	}
	if v, ok := p.param("VALUE"); !ok || v != "DATE" {
		t.Errorf("param VALUE = (%q,%v), want DATE", v, ok)
	}
	if _, ok := p.param("MISSING"); ok {
		t.Error("param MISSING reported present")
	}

	flag := parseProperty("X-PROP;FLAG:value")
	if v, ok := flag.param("FLAG"); !ok || v != "" {
		t.Errorf("bare flag param = (%q,%v), want empty present", v, ok)
	}
}

// TestUnescapeText checks RFC 5545 §3.3.11 TEXT unescaping.
func TestUnescapeText(t *testing.T) {
	t.Parallel()
	tests := map[string]struct{ in, want string }{
		"newline lower":  {`a\nb`, "a\nb"},
		"newline upper":  {`a\Nb`, "a\nb"},
		"escaped comma":  {`a\,b`, "a,b"},
		"escaped semi":   {`a\;b`, "a;b"},
		"escaped slash":  {`a\\b`, `a\b`},
		"combo":          {`Quick sync.\n Bring blockers\, notes\; here`, "Quick sync.\n Bring blockers, notes; here"},
		"no escapes":     {"plain text", "plain text"},
		"trailing slash": {`ends\`, `ends\`},
		"unknown escape": {`a\qb`, "aqb"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := unescapeText(tc.in); got != tc.want {
				t.Errorf("unescapeText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseEventsBasics parses a multi-event feed and checks counts, ordering,
// and that non-VEVENT components (VTIMEZONE) are ignored.
func TestParseEventsBasics(t *testing.T) {
	t.Parallel()
	feed := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"BEGIN:VTIMEZONE",
		"TZID:UTC",
		"END:VTIMEZONE",
		"BEGIN:VEVENT",
		"UID:a@example.com",
		"SUMMARY:First",
		"END:VEVENT",
		"BEGIN:VEVENT",
		"UID:b@example.com",
		"SUMMARY:Second",
		"END:VEVENT",
		"END:VCALENDAR",
	}, "\r\n")

	events := parseEvents(feed)
	if len(events) != 2 {
		t.Fatalf("parsed %d events, want 2 (VTIMEZONE must be ignored)", len(events))
	}
	if got := events[0].value("UID"); got != "a@example.com" {
		t.Errorf("event[0] UID = %q", got)
	}
	if got := events[1].value("SUMMARY"); got != "Second" {
		t.Errorf("event[1] SUMMARY = %q", got)
	}
	// raw block is captured for fingerprinting.
	if !strings.Contains(events[0].raw, "SUMMARY:First") {
		t.Errorf("event[0].raw missing SUMMARY: %q", events[0].raw)
	}
}

// TestParseEventsFolded proves folded property lines reassemble before mapping.
func TestParseEventsFolded(t *testing.T) {
	t.Parallel()
	feed := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"BEGIN:VEVENT",
		"UID:folded@example.com",
		"DESCRIPTION:This is a very long description that the feed",
		"  folded across three",
		"  physical lines.",
		"END:VEVENT",
		"END:VCALENDAR",
	}, "\r\n")

	events := parseEvents(feed)
	if len(events) != 1 {
		t.Fatalf("parsed %d events, want 1", len(events))
	}
	// Each continuation strips exactly one leading whitespace char; the fixture
	// keeps a second space so a single visible space survives between words.
	want := "This is a very long description that the feed folded across three physical lines."
	if got := events[0].value("DESCRIPTION"); got != want {
		t.Errorf("folded DESCRIPTION = %q,\n want %q", got, want)
	}
}

// TestParseEventsAllDayVsTimed checks the VALUE=DATE all-day form parses with its
// parameter intact alongside a timed event.
func TestParseEventsAllDayVsTimed(t *testing.T) {
	t.Parallel()
	feed := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"BEGIN:VEVENT",
		"UID:allday@example.com",
		"DTSTART;VALUE=DATE:20260610",
		"DTEND;VALUE=DATE:20260611",
		"END:VEVENT",
		"BEGIN:VEVENT",
		"UID:timed@example.com",
		"DTSTART;TZID=America/Los_Angeles:20260603T093000",
		"END:VEVENT",
		"END:VCALENDAR",
	}, "\r\n")

	events := parseEvents(feed)
	if len(events) != 2 {
		t.Fatalf("parsed %d events, want 2", len(events))
	}
	dt, _ := events[0].get("DTSTART")
	if v, ok := dt.param("VALUE"); !ok || v != "DATE" {
		t.Errorf("all-day DTSTART VALUE param = (%q,%v), want DATE", v, ok)
	}
	if events[0].value("DTSTART") != "20260610" {
		t.Errorf("all-day DTSTART value = %q", events[0].value("DTSTART"))
	}
	timed, _ := events[1].get("DTSTART")
	if v, _ := timed.param("TZID"); v != "America/Los_Angeles" {
		t.Errorf("timed DTSTART TZID = %q", v)
	}
}

// TestGetAll checks repeated properties (ATTENDEE) are all returned in order.
func TestGetAll(t *testing.T) {
	t.Parallel()
	feed := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"BEGIN:VEVENT",
		"UID:multi@example.com",
		"ATTENDEE;CN=A:mailto:a@x.com",
		"ATTENDEE;CN=B:mailto:b@x.com",
		"ATTENDEE;CN=C:mailto:c@x.com",
		"END:VEVENT",
		"END:VCALENDAR",
	}, "\r\n")

	events := parseEvents(feed)
	atts := events[0].getAll("ATTENDEE")
	if len(atts) != 3 {
		t.Fatalf("getAll(ATTENDEE) = %d, want 3", len(atts))
	}
	if cn, _ := atts[2].param("CN"); cn != "C" {
		t.Errorf("third attendee CN = %q, want C", cn)
	}
}
