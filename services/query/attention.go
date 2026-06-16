package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/asker/asker/platform/personalization"
	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// Attention / salience scorer (spec v3.2 §1.6). "Needs attention" is not a
// similarity problem, it is a SALIENCE problem: compute, per candidate, how much
// it warrants action right now. Each feature below is grounded in a documented
// metadata key (DECISIONS D12) and tagged with the psychological principle it
// encodes; absent keys simply contribute nothing (graceful degradation), so the
// scorer is correct on today's data and richer as connectors emit more.
//
// scoreAttention returns a salience score in [0,1] plus the human-readable
// reasons that fired (for the explanation). The attention-sensitivity slider is
// the Signal-Detection criterion and is applied by the CALLER (personalize.go),
// not here — this function reports the raw evidence.

// Attention feature weights. They may sum to > 1; the final score is clamped to
// [0,1]. Tuned so a single decisive open-loop (RSVP-pending, overdue) is enough
// to surface an item, while softer signals (recency, addressing) accumulate.
const (
	attnRSVPPending    = 0.9 // Zeigarnik (open loop) + Loss Aversion (expiring invite)
	attnOverdue        = 0.9 // Loss Aversion / Prospect Theory (a missed deadline is a loss)
	attnUpcoming       = 0.7 // Loss Aversion (an approaching, still-actionable deadline)
	attnUnread         = 0.4 // Zeigarnik (an unread item is an unclosed loop)
	attnImportant      = 0.5 // Social Proof / Authority (important sender/flag)
	attnDirectlyToYou  = 0.3 // Salience (addressed TO you outranks CC)
	attnRecent         = 0.2 // Recency / Mere-Exposure
	attentionHorizon   = 7 * 24 * time.Hour
	staleThreadHorizon = 14 * 24 * time.Hour
)

// attentionResult is a candidate's salience score and the reasons behind it.
type attentionResult struct {
	score   float64
	reasons []string
}

// scoreAttention computes the salience of one hit for the calling user.
func scoreAttention(hit *queryv1.Hit, profile personalization.Profile, win timeWindow, now time.Time) attentionResult {
	md := hit.GetMetadata()
	var score float64
	var reasons []string
	add := func(w float64, reason string) {
		score += w
		if reason != "" {
			reasons = append(reasons, reason)
		}
	}

	// RSVP-pending (calendar): the user's own response is still "needsAction".
	if rsvpPending(md, profile.SelfEmails) {
		add(attnRSVPPending, "RSVP pending")
	}

	// Upcoming event still ahead of now, within the attention horizon (or the
	// query's explicit window): sooner = more urgent (Loss Aversion).
	if hit.GetType() == askerv1.DocType_CALENDAR_EVENT {
		if start, ok := flexibleTime(md["start"]); ok {
			if w, reason, fired := upcomingScore(start, win, now); fired {
				add(w, reason)
			}
		}
	}

	// Overdue: a due/deadline in the past on an item that is not done.
	if due, ok := firstFlexibleTime(md, "due", "deadline"); ok && due.Before(now) && !isDone(md["status"]) {
		days := daysBetween(due, now)
		add(attnOverdue, fmt.Sprintf("overdue by %s", humanizeDays(days)))
	}

	// Unread / unanswered (Zeigarnik open loop).
	if isUnread(md) {
		add(attnUnread, "unread")
	}

	// Important: explicit flag or a sender on the important-people list (Authority).
	if isFlagged(md["important"]) {
		add(attnImportant, "marked important")
	} else if from := md["from"]; from != "" && profile.IsImportantPerson(from) {
		add(attnImportant, "from "+shortAddress(from)+" (important)")
	}

	// Directly addressed to you, not just CC'd (Salience / Von Restorff).
	if addressedTo(md["to"], profile.SelfEmails) {
		add(attnDirectlyToYou, "addressed to you")
	}

	// Recency / staleness: a recently active item is fresher in mind; a stale
	// open loop (old but unanswered) still nags (Zeigarnik) — both nudge up.
	if t := hitTime(hit); !t.IsZero() {
		age := now.Sub(t)
		switch {
		case age >= 0 && age <= attentionHorizon:
			add(attnRecent, "recent")
		case age > staleThreadHorizon && isUnread(md):
			add(attnRecent, "waiting a while")
		}
	}

	if score > 1 {
		score = 1
	}
	if score < 0 {
		score = 0
	}
	return attentionResult{score: score, reasons: reasons}
}

// rsvpPending reports whether any of the user's self-emails has a calendar
// response_status of "needsAction".
func rsvpPending(md map[string]string, selfEmails []string) bool {
	if len(selfEmails) == 0 {
		return false
	}
	for k, v := range md {
		rest, ok := strings.CutPrefix(k, "response_status:")
		if !ok {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(v), "needsAction") {
			continue
		}
		for _, self := range selfEmails {
			if self != "" && strings.Contains(strings.ToLower(rest), strings.ToLower(self)) {
				return true
			}
		}
	}
	return false
}

// upcomingScore scores a future event start by proximity: an event that already
// passed scores nothing; one starting now scores the full weight; the weight
// decays to ~0 at the attention horizon. An event inside the query's explicit
// window always fires (the user asked for that window).
func upcomingScore(start time.Time, win timeWindow, now time.Time) (float64, string, bool) {
	if start.Before(now) {
		return 0, "", false
	}
	until := start.Sub(now)
	inWindow := !win.isZero() && !start.Before(win.From) && start.Before(win.To)
	if until > attentionHorizon && !inWindow {
		return 0, "", false
	}
	// Linear proximity decay over the horizon, floored so an in-window event
	// beyond the horizon still registers.
	frac := 1 - float64(until)/float64(attentionHorizon)
	if frac < 0 {
		frac = 0
	}
	w := attnUpcoming * frac
	if inWindow && w < attnUpcoming*0.5 {
		w = attnUpcoming * 0.5
	}
	return w, "starts " + humanizeUntil(until), true
}

// isDone reports whether a status string denotes a completed/closed item.
func isDone(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "done", "closed", "resolved", "complete", "completed", "cancelled", "canceled": //nolint:misspell // "cancelled" is a real source status value (Calendar/Jira)
		return true
	default:
		return false
	}
}

// isUnread reads the documented unread signals (emitted by the Gmail connector
// from the UNREAD label; other connectors may set read_status).
func isUnread(md map[string]string) bool {
	return isFlagged(md["unread"]) || strings.EqualFold(strings.TrimSpace(md["read_status"]), "unread")
}

func isFlagged(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes":
		return true
	default:
		return false
	}
}

// addressedTo reports whether any self-email appears in a To header.
func addressedTo(to string, selfEmails []string) bool {
	if to == "" || len(selfEmails) == 0 {
		return false
	}
	lower := strings.ToLower(to)
	for _, self := range selfEmails {
		if self != "" && strings.Contains(lower, strings.ToLower(self)) {
			return true
		}
	}
	return false
}

// firstFlexibleTime returns the first parseable time among the named metadata
// keys.
func firstFlexibleTime(md map[string]string, keys ...string) (time.Time, bool) {
	for _, k := range keys {
		if t, ok := flexibleTime(md[k]); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

// flexibleTime parses a metadata timestamp that may be RFC3339, an all-day date
// (YYYY-MM-DD), or unix epoch seconds. Returns UTC.
func flexibleTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	if sec, err := strconv.ParseInt(s, 10, 64); err == nil && sec > 0 {
		return time.Unix(sec, 0).UTC(), true
	}
	return time.Time{}, false
}

func daysBetween(a, b time.Time) int {
	d := int(math.Round(b.Sub(a).Hours() / 24))
	if d < 0 {
		d = -d
	}
	return d
}

func humanizeDays(days int) string {
	switch {
	case days <= 0:
		return "today"
	case days == 1:
		return "1 day"
	default:
		return strconv.Itoa(days) + " days"
	}
}

// humanizeUntil renders a duration-until as "today", "tomorrow", or "in N days".
func humanizeUntil(d time.Duration) string {
	days := int(d.Hours() / 24)
	switch {
	case days <= 0:
		return "today"
	case days == 1:
		return "tomorrow"
	default:
		return "in " + strconv.Itoa(days) + " days"
	}
}

// shortAddress trims an address header to a compact display fragment (display
// name when present, else the email local part), for explanations.
func shortAddress(from string) string {
	from = strings.TrimSpace(from)
	if i := strings.IndexByte(from, '<'); i > 0 {
		if name := strings.TrimSpace(from[:i]); name != "" {
			return strings.Trim(name, `"`)
		}
	}
	from = strings.Trim(from, "<>")
	if i := strings.IndexByte(from, '@'); i > 0 {
		return from[:i]
	}
	return from
}
