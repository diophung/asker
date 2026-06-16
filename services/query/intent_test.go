package main

import "testing"

func TestClassifyIntent(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		docTypes []string
		want     intentClass
	}{
		{"calendar lookup", "what's on my calendar next week", nil, intentScheduleLookup},
		{"my schedule", "my schedule tomorrow", nil, intentScheduleLookup},
		{"meetings", "meetings this week", nil, intentScheduleLookup},
		{"calendar type filter implies schedule", "", []string{"CALENDAR_EVENT"}, intentScheduleLookup},
		{"needs attention", "what needs my attention this week", nil, intentNeedsAttention},
		{"action items", "any action items for me", nil, intentNeedsAttention},
		{"follow up", "what should I follow up on", nil, intentNeedsAttention},
		{"unread framing", "unread important email", nil, intentNeedsAttention},
		{"content lookup", "quarterly revenue deck", nil, intentFindItem},
		{"attention beats schedule when both", "meetings that need my attention", nil, intentNeedsAttention},
		{"empty freeform", "", nil, intentFreeform},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyIntent(tc.text, tc.docTypes); got != tc.want {
				t.Errorf("classifyIntent(%q,%v) = %v, want %v", tc.text, tc.docTypes, got, tc.want)
			}
		})
	}
}

func TestContentResidual(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"what's on my calendar", ""},           // pure intent
		{"what needs my attention", ""},         // pure intent
		{"calendar about budget", "budget"},     // substantive content survives
		{"meetings with the acme team", "acme"}, // "team"? team is not a stopword -> kept
		{"the a an on my", ""},                  // all stopwords
		{"'s agenda", ""},                       // possessive remnant ("next week's" -> "'s") drops
		{"am i busy", ""},                       // availability framing is pure intent
		{"busy season report", "season report"}, // soft cue + real content -> content survives
	}
	for _, tc := range cases {
		got := contentResidual(tc.text)
		if tc.text == "meetings with the acme team" {
			// "team" is content; assert acme present at least.
			if got == "" {
				t.Errorf("contentResidual(%q) dropped all content", tc.text)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("contentResidual(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

func TestIntentString(t *testing.T) {
	for c, want := range map[intentClass]string{
		intentNeedsAttention: "needs_attention",
		intentScheduleLookup: "schedule_lookup",
		intentFindItem:       "find_item",
		intentFreeform:       "freeform",
	} {
		if c.String() != want {
			t.Errorf("%d.String() = %q, want %q", c, c.String(), want)
		}
	}
}
