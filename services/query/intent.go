package main

import "strings"

// Query understanding — intent class (spec v3.2 §1.5). A natural-language query
// carries an intent that selects which sources, filters, and ranking profile to
// use. We classify with a lightweight, deterministic rules layer (no LLM
// dependency — see DECISIONS D6) into one of:
//
//   - intentScheduleLookup ("what's on my calendar next week"): calendar
//     source, occurrence-time window, time-ordered.
//   - intentNeedsAttention ("what needs my attention this week"): multi-source,
//     urgency-weighted ranking (the attention scorer).
//   - intentFindItem: a specific lookup with content terms (the default when the
//     query has substantive content words).
//   - intentFreeform: a content query with no special handling.
//
// The interface is intentionally small so an embedding/LLM classifier can
// replace classifyIntent later without touching callers.
type intentClass int

const (
	intentFreeform intentClass = iota
	intentFindItem
	intentScheduleLookup
	intentNeedsAttention
)

func (c intentClass) String() string {
	switch c {
	case intentNeedsAttention:
		return "needs_attention"
	case intentScheduleLookup:
		return "schedule_lookup"
	case intentFindItem:
		return "find_item"
	default:
		return "freeform"
	}
}

// attentionKeywords signal the needs-attention intent (action/urgency framing).
var attentionKeywords = []string{
	"need my attention", "needs my attention", "need attention", "needs attention",
	"my attention", "attention", "action item", "action items", "needs action",
	"need action", "requires action", "require action", "to-do", "to do", "todo",
	"follow up", "follow-up", "followup", "urgent", "overdue", "respond to",
	"awaiting", "waiting on", "unanswered", "unread", "deadline", "due",
	"what should i", "focus on",
}

// scheduleKeywords signal the schedule-lookup intent (calendar framing).
var scheduleKeywords = []string{
	"on my calendar", "my calendar", "calendar", "my schedule", "schedule",
	"my agenda", "agenda", "meeting", "meetings", "appointment", "appointments",
	"event", "events", "what's on", "whats on", "do i have on",
}

// classifyIntent picks the intent from the residual query text and any explicit
// type filters. needs-attention is checked first (it is the salience use case
// and can co-occur with calendar words); schedule-lookup next; otherwise the
// query is a content lookup. An explicit calendar-only type filter also implies
// schedule-lookup.
func classifyIntent(text string, docTypes []string) intentClass {
	lower := strings.ToLower(text)
	if containsAny(lower, attentionKeywords) {
		return intentNeedsAttention
	}
	if containsAny(lower, scheduleKeywords) || onlyCalendarTypes(docTypes) {
		return intentScheduleLookup
	}
	if strings.TrimSpace(contentResidual(lower)) != "" {
		return intentFindItem
	}
	return intentFreeform
}

// onlyCalendarTypes reports whether the explicit type filter is exactly the
// calendar type (so a /v1/search/calendar tab counts as a schedule lookup).
func onlyCalendarTypes(docTypes []string) bool {
	if len(docTypes) != 1 {
		return false
	}
	return strings.EqualFold(docTypes[0], "CALENDAR_EVENT")
}

// intentStopwords are tokens that carry intent/phrasing but no document content,
// stripped when deciding whether a query has substantive content (and thus
// whether a schedule lookup should keyword-match or just list the window).
var intentStopwords = map[string]bool{
	// generic stopwords
	"what": true, "whats": true, "what's": true, "is": true, "are": true, "the": true,
	"a": true, "an": true, "on": true, "in": true, "my": true, "me": true, "i": true,
	"do": true, "does": true, "have": true, "has": true, "to": true, "of": true,
	"for": true, "should": true, "this": true, "that": true, "any": true, "show": true,
	"list": true, "find": true, "get": true, "all": true, "with": true, "and": true,
	"about": true,
	// intent words (calendar / attention)
	"calendar": true, "schedule": true, "agenda": true, "meeting": true, "meetings": true,
	"appointment": true, "appointments": true, "event": true, "events": true,
	"attention": true, "needs": true, "need": true, "action": true, "items": true,
	"item": true, "todo": true, "urgent": true, "focus": true, "upcoming": true,
}

// contentResidual returns the query text with intent/stopword tokens removed,
// i.e. the substantive content words. An empty result means the query was pure
// intent ("what's on my calendar") and a schedule lookup should list the whole
// window rather than keyword-match it.
func contentResidual(text string) string {
	var out []string
	for _, tok := range strings.Fields(text) {
		clean := strings.Trim(strings.ToLower(tok), ".,?!:;\"'")
		if clean == "" || intentStopwords[clean] {
			continue
		}
		out = append(out, tok)
	}
	return strings.Join(out, " ")
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
