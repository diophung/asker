package main

import (
	"testing"
	"time"

	"github.com/asker/asker/platform/personalization"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// TestScopeQueryNLP locks in the v3.2 natural-language calendar fixes surfaced
// by the NLP-robustness probe: relative/colloquial phrasings must resolve to a
// time-scoped CALENDAR_EVENT lookup that lists the window (empty residual text),
// and a window-less schedule lookup must default to UPCOMING occurrences (today
// onward) instead of returning the entire history oldest-first.
func TestScopeQueryNLP(t *testing.T) {
	// Wednesday 2026-06-10 14:00 UTC; week starts Mon 2026-06-08, today=06-10.
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	profile := personalization.DefaultProfile() // empty timezone => UTC
	day := func(d int) time.Time { return time.Date(2026, 6, d, 0, 0, 0, 0, time.UTC) }

	cases := []struct {
		name          string
		query         string
		wantIntent    intentClass
		wantText      string // residual after scoping ("" => list the window)
		wantEventFrom time.Time
		wantEventTo   time.Time // zero => unbounded (upcoming default)
	}{
		{"explicit next week", "what's on my calendar next week", intentScheduleLookup, "", day(15), day(22)},
		{"agenda possessive", "next week's agenda", intentScheduleLookup, "", day(15), day(22)},
		{"show possessive", "show me next week's calendar", intentScheduleLookup, "", day(15), day(22)},
		{"busy availability", "am I busy next week", intentScheduleLookup, "", day(15), day(22)},
		{"coming up", "what's coming up next week", intentScheduleLookup, "", day(15), day(22)},
		{"happening", "what's happening this week", intentScheduleLookup, "", day(8), day(15)},
		{"in N days", "my calendar in 3 days", intentScheduleLookup, "", day(13), day(14)},
		{"a week from now", "what's on my calendar a week from now", intentScheduleLookup, "", day(17), day(18)},
		{"rest of the week", "my calendar for the rest of the week", intentScheduleLookup, "", day(10), day(15)},
		{"bare temporal promotes", "next week", intentScheduleLookup, "", day(15), day(22)},
		// Window-less schedule lookups: default to upcoming (today onward), no upper bound.
		{"upcoming meetings", "upcoming meetings", intentScheduleLookup, "", day(10), time.Time{}},
		{"bare my calendar", "my calendar", intentScheduleLookup, "", day(10), time.Time{}},
		{"bare my schedule", "my schedule", intentScheduleLookup, "", day(10), time.Time{}},
		{"soft coming up no window", "what's coming up", intentScheduleLookup, "", day(10), time.Time{}},
		// A real content query that merely contains a soft cue must NOT be hijacked.
		{"content with soft cue", "busy season sales report", intentFindItem, "busy season sales report", time.Time{}, time.Time{}},
		{"content lookup", "quarterly revenue deck", intentFindItem, "quarterly revenue deck", time.Time{}, time.Time{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := parsedQuery{Text: tc.query}
			got := scopeQuery(plan, profile, now)
			if got.Intent != tc.wantIntent {
				t.Errorf("intent = %v, want %v", got.Intent, tc.wantIntent)
			}
			if got.Text != tc.wantText {
				t.Errorf("residual text = %q, want %q", got.Text, tc.wantText)
			}
			if !got.EventFrom.Equal(tc.wantEventFrom) {
				t.Errorf("event_from = %s, want %s", got.EventFrom, tc.wantEventFrom)
			}
			if !got.EventTo.Equal(tc.wantEventTo) {
				t.Errorf("event_to = %s, want %s", got.EventTo, tc.wantEventTo)
			}
			if tc.wantIntent == intentScheduleLookup {
				if len(got.DocTypes) != 1 || got.DocTypes[0] != askerv1.DocType_CALENDAR_EVENT {
					t.Errorf("doc types = %v, want [CALENDAR_EVENT]", got.DocTypes)
				}
			}
		})
	}
}
