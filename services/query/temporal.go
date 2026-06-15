package main

import (
	"strings"
	"time"
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

// parseTemporal finds the first recognized time phrase in text and returns its
// resolved window (in loc, relative to now), the text with that phrase removed,
// and ok=true. With no recognized phrase it returns the text unchanged and
// ok=false. Matching is case-insensitive and phrase-longest-first.
func parseTemporal(text string, loc *time.Location, now time.Time) (timeWindow, string, bool) {
	if loc == nil {
		loc = time.UTC
	}
	lower := strings.ToLower(text)
	for _, tp := range temporalPhrases {
		idx := strings.Index(lower, tp.phrase)
		if idx < 0 {
			continue
		}
		win := tp.window(now.In(loc), loc)
		stripped := text[:idx] + text[idx+len(tp.phrase):]
		return win, normalizeSpaces(stripped), true
	}
	return timeWindow{}, text, false
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
