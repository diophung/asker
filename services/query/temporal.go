package main

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Query understanding — temporal scope (spec v3.2 §1.5). Natural-language time
// references ("next week", "this week", "today") carry structured intent the
// vector search cannot honor: an embedding will NOT reliably capture a date
// range. So we resolve the phrase to a concrete [from,to) window in the USER'S
// timezone relative to the current instant, and the caller applies it as a HARD
// filter (on event_start for calendar lookups, created_at otherwise — see
// scope.go), never relying on the embedding for dates.
//
// Week boundaries are Monday-based (spec: "next Mon–Sun"). All windows are
// half-open [from, to): inclusive of from, exclusive of to.

// timeWindow is a half-open [From, To) instant range.
type timeWindow struct {
	From time.Time
	To   time.Time
}

func (w timeWindow) isZero() bool { return w.From.IsZero() && w.To.IsZero() }

// temporalPhrase is one recognized natural-language time expression and the
// window it resolves to relative to a reference time in a location.
type temporalPhrase struct {
	phrase string
	window func(now time.Time, loc *time.Location) timeWindow
}

// temporalPhrases are matched longest-first (so "next week" wins over "week"
// and "this weekend" over "this week"); the first match in the query text is
// used and stripped from the residual text.
var temporalPhrases = []temporalPhrase{
	// Weekend phrases MUST precede the "this week"/"next week" entries: "this
	// weekend" contains the substring "this week", so it has to match first.
	{"this weekend", func(now time.Time, loc *time.Location) timeWindow {
		sat := startOfWeek(now, loc).AddDate(0, 0, 5) // Mon + 5 = Sat
		return timeWindow{sat, sat.AddDate(0, 0, 2)}  // Sat..Mon
	}},
	{"next weekend", func(now time.Time, loc *time.Location) timeWindow {
		sat := startOfWeek(now, loc).AddDate(0, 0, 12) // next week's Sat
		return timeWindow{sat, sat.AddDate(0, 0, 2)}
	}},
	{"next week", func(now time.Time, loc *time.Location) timeWindow {
		start := startOfWeek(now, loc).AddDate(0, 0, 7)
		return timeWindow{start, start.AddDate(0, 0, 7)}
	}},
	{"this week", func(now time.Time, loc *time.Location) timeWindow {
		start := startOfWeek(now, loc)
		return timeWindow{start, start.AddDate(0, 0, 7)}
	}},
	{"last week", func(now time.Time, loc *time.Location) timeWindow {
		start := startOfWeek(now, loc).AddDate(0, 0, -7)
		return timeWindow{start, start.AddDate(0, 0, 7)}
	}},
	{"next month", func(now time.Time, loc *time.Location) timeWindow {
		start := startOfMonth(now, loc).AddDate(0, 1, 0)
		return timeWindow{start, start.AddDate(0, 1, 0)}
	}},
	{"this month", func(now time.Time, loc *time.Location) timeWindow {
		start := startOfMonth(now, loc)
		return timeWindow{start, start.AddDate(0, 1, 0)}
	}},
	{"last month", func(now time.Time, loc *time.Location) timeWindow {
		start := startOfMonth(now, loc).AddDate(0, -1, 0)
		return timeWindow{start, start.AddDate(0, 1, 0)}
	}},
	{"tomorrow", func(now time.Time, loc *time.Location) timeWindow {
		start := startOfDay(now, loc).AddDate(0, 0, 1)
		return timeWindow{start, start.AddDate(0, 0, 1)}
	}},
	{"yesterday", func(now time.Time, loc *time.Location) timeWindow {
		start := startOfDay(now, loc).AddDate(0, 0, -1)
		return timeWindow{start, start.AddDate(0, 0, 1)}
	}},
	{"today", func(now time.Time, loc *time.Location) timeWindow {
		start := startOfDay(now, loc)
		return timeWindow{start, start.AddDate(0, 0, 1)}
	}},
	{"this year", func(now time.Time, loc *time.Location) timeWindow {
		start := startOfYear(now, loc)
		return timeWindow{start, start.AddDate(1, 0, 0)}
	}},
}

// temporalPattern is a parametric time expression ("in N days", "a week from
// now") matched by regexp. The window func receives the regexp submatch groups
// (groups[0] is the whole match) so it can read a captured count.
type temporalPattern struct {
	re     *regexp.Regexp
	window func(groups []string, now time.Time, loc *time.Location) timeWindow
}

// temporalPatterns handle relative expressions the fixed list cannot enumerate.
// They are tried after the fixed phrases (which cover the common absolute terms)
// and matched in order; the first hit wins. None of these overlap a fixed phrase
// (the fixed list has only "this/next/last week", never "a week from now" etc.).
var temporalPatterns = []temporalPattern{
	// "3 days from now" / "in 3 days" -> the single day N days out.
	{regexp.MustCompile(`\b(\d+)\s+days?\s+from\s+now\b`), nDaysOut},
	{regexp.MustCompile(`\bin\s+(\d+)\s+days?\b`), nDaysOut},
	// "a week from now" / "in a week" -> the single day 7 days out.
	{regexp.MustCompile(`\b(?:a|one)\s+weeks?\s+from\s+now\b`), weekFromNow},
	{regexp.MustCompile(`\bin\s+(?:a|one)\s+week\b`), weekFromNow},
	// "next 3 days" / "in the next 3 days" -> [today, today+N).
	{regexp.MustCompile(`\b(?:in\s+the\s+)?next\s+(\d+)\s+days?\b`), nextNDays},
	// "rest of the/this week" -> [today, end of this week).
	{regexp.MustCompile(`\brest\s+of\s+(?:the|this)\s+week\b`), restOfWeek},
}

func nDaysOut(groups []string, now time.Time, loc *time.Location) timeWindow {
	n, _ := strconv.Atoi(groups[1])
	start := startOfDay(now, loc).AddDate(0, 0, n)
	return timeWindow{start, start.AddDate(0, 0, 1)}
}

func weekFromNow(_ []string, now time.Time, loc *time.Location) timeWindow {
	start := startOfDay(now, loc).AddDate(0, 0, 7)
	return timeWindow{start, start.AddDate(0, 0, 1)}
}

func nextNDays(groups []string, now time.Time, loc *time.Location) timeWindow {
	n, _ := strconv.Atoi(groups[1])
	if n < 1 {
		n = 1
	}
	start := startOfDay(now, loc)
	return timeWindow{start, start.AddDate(0, 0, n)}
}

func restOfWeek(_ []string, now time.Time, loc *time.Location) timeWindow {
	return timeWindow{startOfDay(now, loc), startOfWeek(now, loc).AddDate(0, 0, 7)}
}

// parseTemporal finds the first recognized time phrase in text and returns its
// resolved window (in loc, relative to now), the text with that phrase removed,
// and ok=true. With no recognized phrase it returns the text unchanged and
// ok=false. Matching is case-insensitive: fixed phrases first (longest-first),
// then the parametric patterns ("in N days", "a week from now").
func parseTemporal(text string, loc *time.Location, now time.Time) (timeWindow, string, bool) {
	if loc == nil {
		loc = time.UTC
	}
	nowL := now.In(loc)
	// The supported time expressions are ASCII. Fold only ASCII letters so
	// matching offsets still address the original UTF-8 text (Unicode lowercase
	// mappings can change byte lengths, e.g. the capital dotted I).
	lower := strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, maskQuotedText(text))
	for _, tp := range temporalPhrases {
		idx := temporalPhraseIndex(lower, tp.phrase)
		if idx < 0 {
			continue
		}
		win := tp.window(nowL, loc)
		stripped := text[:idx] + text[idx+len(tp.phrase):]
		return win, normalizeSpaces(stripped), true
	}
	for _, tp := range temporalPatterns {
		for _, span := range tp.re.FindAllStringIndex(lower, -1) {
			if !temporalWordBoundaries(lower, span[0], span[1]) {
				continue
			}
			groups := tp.re.FindStringSubmatch(lower[span[0]:span[1]])
			win := tp.window(groups, nowL, loc)
			stripped := text[:span[0]] + text[span[1]:]
			return win, normalizeSpaces(stripped), true
		}
	}
	return timeWindow{}, text, false
}

// A temporal phrase must occupy complete words. A title such as Tomorrowland
// or an identifier such as today_report must remain ordinary search content.
func temporalPhraseIndex(text, phrase string) int {
	for offset := 0; offset < len(text); {
		idx := strings.Index(text[offset:], phrase)
		if idx < 0 {
			break
		}
		idx += offset
		if temporalWordBoundaries(text, idx, idx+len(phrase)) {
			return idx
		}
		offset = idx + len(phrase)
	}
	return -1
}

func temporalWordBoundaries(text string, start, end int) bool {
	isWord := func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) || r == '_'
	}
	if start > 0 {
		r, _ := utf8.DecodeLastRuneInString(text[:start])
		if isWord(r) {
			return false
		}
	}
	if end < len(text) {
		r, _ := utf8.DecodeRuneInString(text[end:])
		if isWord(r) {
			return false
		}
	}
	return true
}

// Preserve titles and exact phrases such as "last week report" as content.
// Byte positions stay unchanged so the selected unquoted span can be removed.
func maskQuotedText(text string) string {
	out := []byte(text)
	quoted, escaped := false, false
	for i, b := range out {
		if b == '"' && !escaped {
			quoted = !quoted
			out[i] = ' '
		} else if quoted {
			out[i] = ' '
		}
		escaped = b == '\\' && !escaped
	}
	return string(out)
}

// startOfDay returns local midnight of now's calendar day in loc.
func startOfDay(now time.Time, loc *time.Location) time.Time {
	t := now.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// startOfWeek returns local midnight of the Monday of now's week in loc.
func startOfWeek(now time.Time, loc *time.Location) time.Time {
	d := startOfDay(now, loc)
	// time.Weekday: Sunday=0..Saturday=6; shift so Monday is the week start.
	offset := (int(d.Weekday()) + 6) % 7
	return d.AddDate(0, 0, -offset)
}

// startOfMonth returns local midnight of the first day of now's month in loc.
func startOfMonth(now time.Time, loc *time.Location) time.Time {
	t := now.In(loc)
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
}

// startOfYear returns local midnight of Jan 1 of now's year in loc.
func startOfYear(now time.Time, loc *time.Location) time.Time {
	t := now.In(loc)
	return time.Date(t.Year(), 1, 1, 0, 0, 0, 0, loc)
}

// normalizeSpaces collapses runs of whitespace to single spaces and trims.
func normalizeSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
