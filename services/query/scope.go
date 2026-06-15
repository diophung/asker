package main

import (
	"strings"
	"time"

	"github.com/asker/asker/platform/personalization"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// scopeQuery is the v3 query-understanding step layered on top of understand():
// it classifies intent and resolves natural-language temporal scope, then maps
// the window onto the CORRECT hard filter for that intent. It runs ONLY on the
// personalized path (server.go gates on a wired profile loader), so the
// non-personalized path — and every existing understand() test — is untouched.
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

	win, stripped, hasWin := parseTemporal(plan.Text, profile.Location(), now)
	if hasWin {
		plan.Text = stripped
		plan.WinFrom, plan.WinTo = win.From, win.To
	}

	switch plan.Intent {
	case intentScheduleLookup:
		if len(plan.DocTypes) == 0 {
			plan.DocTypes = []askerv1.DocType{askerv1.DocType_CALENDAR_EVENT}
		}
		if hasWin {
			plan.EventFrom, plan.EventTo = win.From, win.To
		}
		if contentResidual(strings.ToLower(plan.Text)) == "" {
			plan.Text = "" // pure intent: list the window, do not keyword-match
		}
	case intentNeedsAttention:
		if contentResidual(strings.ToLower(plan.Text)) == "" {
			plan.Text = "" // pure intent: broad candidate set, attention-ranked
		}
	case intentFindItem, intentFreeform:
		if hasWin && plan.From.IsZero() && plan.To.IsZero() {
			plan.From, plan.To = win.From, win.To
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
