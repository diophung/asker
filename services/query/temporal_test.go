package main

import (
	"testing"
	"time"
)

func TestParseTemporalWindows(t *testing.T) {
	loc := time.UTC
	// A fixed reference: Wednesday 2026-06-10 14:00 UTC.
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, loc)

	cases := []struct {
		name      string
		text      string
		wantFrom  time.Time
		wantTo    time.Time
		wantStrip string
	}{
		{
			name:      "this week is Mon..next Mon",
			text:      "what is on this week",
			wantFrom:  time.Date(2026, 6, 8, 0, 0, 0, 0, loc),  // Mon
			wantTo:    time.Date(2026, 6, 15, 0, 0, 0, 0, loc), // next Mon
			wantStrip: "what is on",
		},
		{
			name:      "next week is the following Mon..Mon",
			text:      "calendar next week please",
			wantFrom:  time.Date(2026, 6, 15, 0, 0, 0, 0, loc),
			wantTo:    time.Date(2026, 6, 22, 0, 0, 0, 0, loc),
			wantStrip: "calendar please",
		},
		{
			name:      "today",
			text:      "meetings today",
			wantFrom:  time.Date(2026, 6, 10, 0, 0, 0, 0, loc),
			wantTo:    time.Date(2026, 6, 11, 0, 0, 0, 0, loc),
			wantStrip: "meetings",
		},
		{
			name:      "tomorrow",
			text:      "tomorrow",
			wantFrom:  time.Date(2026, 6, 11, 0, 0, 0, 0, loc),
			wantTo:    time.Date(2026, 6, 12, 0, 0, 0, 0, loc),
			wantStrip: "",
		},
		{
			name:      "this month",
			text:      "invoices this month",
			wantFrom:  time.Date(2026, 6, 1, 0, 0, 0, 0, loc),
			wantTo:    time.Date(2026, 7, 1, 0, 0, 0, 0, loc),
			wantStrip: "invoices",
		},
		{
			name:      "next month",
			text:      "next month travel",
			wantFrom:  time.Date(2026, 7, 1, 0, 0, 0, 0, loc),
			wantTo:    time.Date(2026, 8, 1, 0, 0, 0, 0, loc),
			wantStrip: "travel",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			win, stripped, ok := parseTemporal(tc.text, loc, now)
			if !ok {
				t.Fatalf("parseTemporal(%q) returned ok=false", tc.text)
			}
			if !win.From.Equal(tc.wantFrom) || !win.To.Equal(tc.wantTo) {
				t.Errorf("window = [%s,%s), want [%s,%s)", win.From, win.To, tc.wantFrom, tc.wantTo)
			}
			if stripped != tc.wantStrip {
				t.Errorf("stripped = %q, want %q", stripped, tc.wantStrip)
			}
		})
	}
}

func TestParseTemporalRelativeExpressions(t *testing.T) {
	loc := time.UTC
	// Wednesday 2026-06-10 14:00 UTC; week starts Mon 2026-06-08.
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, loc)
	day := func(d int) time.Time { return time.Date(2026, 6, d, 0, 0, 0, 0, loc) }

	cases := []struct {
		name             string
		text             string
		wantFrom, wantTo time.Time
		wantStrip        string
	}{
		{"this weekend", "what's on this weekend", day(13), day(15), "what's on"},
		{"next weekend", "plans next weekend", day(20), day(22), "plans"},
		{"in N days", "my calendar in 3 days", day(13), day(14), "my calendar"},
		{"N days from now", "meetings 2 days from now", day(12), day(13), "meetings"},
		{"a week from now", "calendar a week from now", day(17), day(18), "calendar"},
		{"in a week", "what's on in a week", day(17), day(18), "what's on"},
		{"next N days", "events next 3 days", day(10), day(13), "events"},
		{"rest of the week", "my calendar for the rest of the week", day(10), day(15), "my calendar for the"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			win, stripped, ok := parseTemporal(tc.text, loc, now)
			if !ok {
				t.Fatalf("parseTemporal(%q) returned ok=false", tc.text)
			}
			if !win.From.Equal(tc.wantFrom) || !win.To.Equal(tc.wantTo) {
				t.Errorf("window = [%s,%s), want [%s,%s)", win.From, win.To, tc.wantFrom, tc.wantTo)
			}
			if stripped != tc.wantStrip {
				t.Errorf("stripped = %q, want %q", stripped, tc.wantStrip)
			}
		})
	}
}

// "this weekend" must win over its "this week" substring.
func TestParseTemporalWeekendBeatsWeek(t *testing.T) {
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	win, _, ok := parseTemporal("this weekend", time.UTC, now)
	if !ok {
		t.Fatal("expected a match")
	}
	if !win.From.Equal(time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("this weekend resolved to %s, want 2026-06-13 (Sat)", win.From)
	}
}

func TestParseTemporalNoMatch(t *testing.T) {
	win, stripped, ok := parseTemporal("quarterly revenue report", time.UTC, time.Now())
	if ok {
		t.Errorf("unexpected temporal match: %+v", win)
	}
	if stripped != "quarterly revenue report" {
		t.Errorf("text mutated on no match: %q", stripped)
	}
}

func TestParseTemporalLongestPhraseWins(t *testing.T) {
	// "next week" must win over the substring "week"/"this week".
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	win, _, ok := parseTemporal("agenda next week", time.UTC, now)
	if !ok {
		t.Fatal("expected a match")
	}
	if !win.From.Equal(time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("next week resolved to %s, want 2026-06-15", win.From)
	}
}

func TestParseTemporalHonorsTimezone(t *testing.T) {
	// 00:30 UTC on the 11th is still the 10th (evening) in New York, so "today"
	// in NY is the 10th, not the 11th.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	now := time.Date(2026, 6, 11, 0, 30, 0, 0, time.UTC)
	win, _, ok := parseTemporal("today", ny, now)
	if !ok {
		t.Fatal("expected a match")
	}
	// Local midnight of June 10 in NY.
	wantFrom := time.Date(2026, 6, 10, 0, 0, 0, 0, ny)
	if !win.From.Equal(wantFrom) {
		t.Errorf("today in NY = %s, want %s", win.From, wantFrom)
	}
}
