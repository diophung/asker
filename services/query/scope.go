package main

import (
	"strings"
	"time"

	"github.com/asker/asker/platform/personalization"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// scopeQuery is the v3 query-understanding step layered on top of understand():
// it classifies intent and resolves natural-language temporal scope, then maps
// the window onto the hard filter for that intent. It applies regardless of
// preference ranking, with explicit source/date controls taking precedence.
//
// Intent → handling:
//   - schedule_lookup ("what's on my calendar next week"): scope to the calendar
//     source and filter EVENT_START (occurrence time) by the window; a pure-
//     intent query lists the window rather than keyword-matching it.
//   - needs_attention ("what needs my attention this week"): stay multi-source;
//     the window feeds the attention scorer (it is NOT a hard exclusion, so an
//     old-but-unanswered item still surfaces).
//   - find_item / freeform: an explicit window scopes received-time (created_at),
//     unless the user already pinned before:/after:.
func scopeQuery(plan parsedQuery, profile personalization.Profile, now time.Time) parsedQuery {
	plan.Intent = classifyIntent(plan.Text, docTypeNames(plan.DocTypes))
	// Source tabs and explicit type filters win over inferred calendar framing.
	if len(plan.DocTypes) > 0 && !onlyCalendarTypes(docTypeNames(plan.DocTypes)) && plan.Intent == intentScheduleLookup {
		plan.Intent = intentFindItem
	}

	win, stripped, hasWin := parseTemporal(plan.Text, profile.Location(), now)
	if hasWin {
		plan.Text = stripped
		plan.WinFrom, plan.WinTo = win.From, win.To
	}

	// Promote a bare temporal query ("next week", "tomorrow") or a soft
	// availability query ("what's coming up", "am I busy") with no other content
	// to a schedule lookup: it means "show my calendar", not a keyword search for
	// the leftover words (which finds nothing). The empty-residual guard means a
	// real content query that merely contains a soft cue ("busy season report")
	// is never reclassified.
	if (plan.Intent == intentFindItem || plan.Intent == intentFreeform) &&
		(len(plan.DocTypes) == 0 || onlyCalendarTypes(docTypeNames(plan.DocTypes))) &&
		contentResidual(strings.ToLower(plan.Text)) == "" &&
		(hasWin || hasScheduleSignal(plan.Text)) {
		plan.Intent = intentScheduleLookup
	}
	// Explicit calendar dates select event occurrence even when attention
	// framing chooses a different ranking intent.
	explicitDates := !plan.From.IsZero() || !plan.To.IsZero()
	calendarScoped := onlyCalendarTypes(docTypeNames(plan.DocTypes)) && (explicitDates || hasWin)
	if calendarScoped {
		if explicitDates {
			plan.EventFrom, plan.EventTo = plan.From, plan.To
			if !plan.EventTo.IsZero() && !plan.ToExclusive {
				plan.EventTo = plan.EventTo.Add(time.Second)
			}
		} else {
			plan.EventFrom, plan.EventTo = win.From, win.To
		}
		plan.From, plan.To = time.Time{}, time.Time{}
		plan.WinFrom, plan.WinTo = plan.EventFrom, plan.EventTo
	}

	switch plan.Intent {
	case intentScheduleLookup:
		if len(plan.DocTypes) == 0 {
			plan.DocTypes = []askerv1.DocType{askerv1.DocType_CALENDAR_EVENT}
		}
		if !calendarScoped && (!plan.From.IsZero() || !plan.To.IsZero()) {
			// Calendar source date controls describe occurrence, not authoring.
			// Explicit inclusive bounds win over inferred relative dates.
			plan.EventFrom, plan.EventTo = plan.From, plan.To
			if !plan.EventTo.IsZero() && !plan.ToExclusive {
				plan.EventTo = plan.EventTo.Add(time.Second)
			}
			plan.From, plan.To = time.Time{}, time.Time{}
			if !plan.EventFrom.IsZero() && !plan.EventTo.IsZero() {
				plan.WinFrom, plan.WinTo = plan.EventFrom, plan.EventTo
			}
		} else if !calendarScoped && hasWin {
			plan.EventFrom, plan.EventTo = win.From, win.To
		} else if !calendarScoped {
			// No explicit window: "my calendar" / "upcoming meetings" mean what is
			// ahead, not the entire history. Bound occurrence time to today onward
			// so the list is upcoming-first (sortByEventStart is ascending) and
			// never surfaces decade-old events.
			plan.EventFrom = startOfDay(now, profile.Location())
		}
		if contentResidual(strings.ToLower(plan.Text)) == "" {
			plan.Text = "" // pure intent: list the window, do not keyword-match
		}
	case intentNeedsAttention:
		if contentResidual(strings.ToLower(plan.Text)) == "" {
			plan.Text = "" // pure intent: broad candidate set, attention-ranked
		}
	case intentFindItem, intentFreeform:
		if hasWin && !calendarScoped && plan.From.IsZero() && plan.To.IsZero() {
			plan.From, plan.To = win.From, win.To
			plan.ToExclusive = true
		}
	}
	return plan
}

// intentDrivenEmptyText reports whether an empty residual text is legitimate
// because an intent (schedule/needs-attention) drives retrieval rather than
// query terms — so the server must not reject it as an empty query.
func intentDrivenEmptyText(plan parsedQuery) bool {
	return plan.Intent == intentScheduleLookup || plan.Intent == intentNeedsAttention
}

// docTypeNames renders DocType enums as their string names for the intent
// classifier.
func docTypeNames(types []askerv1.DocType) []string {
	if len(types) == 0 {
		return nil
	}
	out := make([]string, 0, len(types))
	for _, t := range types {
		out = append(out, t.String())
	}
	return out
}
