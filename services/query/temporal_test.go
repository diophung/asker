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
