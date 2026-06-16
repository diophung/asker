package main

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/platform/personalization"
	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

func attHit(docType askerv1.DocType, md map[string]string, created time.Time) *queryv1.Hit {
	h := &queryv1.Hit{Type: docType, Metadata: md}
	if !created.IsZero() {
		h.Created = timestamppb.New(created)
		h.Modified = timestamppb.New(created)
	}
	return h
}

func TestScoreAttentionSignals(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	profile := personalization.Clamp(personalization.Profile{
		SelfEmails:      []string{"me@example.com"},
		ImportantPeople: []string{"boss@example.com"},
	})
	win := timeWindow{From: now, To: now.AddDate(0, 0, 7)}

	t.Run("RSVP pending fires Zeigarnik/loss-aversion", func(t *testing.T) {
		h := attHit(askerv1.DocType_CALENDAR_EVENT, map[string]string{
			"response_status:me@example.com": "needsAction",
			"start":                          now.Add(48 * time.Hour).Format(time.RFC3339),
		}, now)
		got := scoreAttention(h, profile, win, now)
		if got.score <= 0 {
			t.Fatalf("RSVP-pending event scored %v, want > 0", got.score)
		}
		if !hasReason(got.reasons, "RSVP pending") {
			t.Errorf("reasons = %v, want RSVP pending", got.reasons)
		}
	})

	t.Run("upcoming event proximity", func(t *testing.T) {
		h := attHit(askerv1.DocType_CALENDAR_EVENT, map[string]string{
			"start": now.Add(48 * time.Hour).Format(time.RFC3339),
		}, now)
		got := scoreAttention(h, profile, win, now)
		if !hasReasonPrefix(got.reasons, "starts") {
			t.Errorf("reasons = %v, want a 'starts ...' reason", got.reasons)
		}
	})

	t.Run("overdue task (loss aversion)", func(t *testing.T) {
		h := attHit(askerv1.DocType_TICKET, map[string]string{
			"due":    now.Add(-72 * time.Hour).Format(time.RFC3339),
			"status": "open",
		}, now)
		got := scoreAttention(h, profile, win, now)
		if !hasReasonPrefix(got.reasons, "overdue") {
			t.Errorf("reasons = %v, want an 'overdue ...' reason", got.reasons)
		}
	})

	t.Run("done task is not overdue", func(t *testing.T) {
		h := attHit(askerv1.DocType_TICKET, map[string]string{
			"due":    now.Add(-72 * time.Hour).Format(time.RFC3339),
			"status": "done",
		}, now)
		got := scoreAttention(h, profile, win, now)
		if hasReasonPrefix(got.reasons, "overdue") {
			t.Errorf("a done task was flagged overdue: %v", got.reasons)
		}
	})

	t.Run("unread + important email", func(t *testing.T) {
		h := attHit(askerv1.DocType_EMAIL, map[string]string{
			"from":   "boss@example.com",
			"to":     "me@example.com",
			"unread": "true",
		}, now)
		got := scoreAttention(h, profile, win, now)
		if !hasReason(got.reasons, "unread") {
			t.Errorf("reasons = %v, want unread", got.reasons)
		}
		if !hasReasonPrefix(got.reasons, "from boss") {
			t.Errorf("reasons = %v, want an important-sender reason", got.reasons)
		}
		if !hasReason(got.reasons, "addressed to you") {
			t.Errorf("reasons = %v, want addressed to you", got.reasons)
		}
	})

	t.Run("boring email has zero salience", func(t *testing.T) {
		// Old, read, from nobody important, not addressed to self.
		h := attHit(askerv1.DocType_EMAIL, map[string]string{
			"from": "newsletter@vendor.com",
			"to":   "list@example.com",
		}, now.AddDate(0, -2, 0))
		got := scoreAttention(h, profile, win, now)
		if got.score != 0 {
			t.Errorf("boring email scored %v, want 0 (no salience signal)", got.score)
		}
	})

	t.Run("score is clamped to [0,1]", func(t *testing.T) {
		h := attHit(askerv1.DocType_CALENDAR_EVENT, map[string]string{
			"response_status:me@example.com": "needsAction",
			"start":                          now.Add(time.Hour).Format(time.RFC3339),
			"unread":                         "true",
			"important":                      "true",
			"to":                             "me@example.com",
			"from":                           "boss@example.com",
		}, now)
		got := scoreAttention(h, profile, win, now)
		if got.score > 1 || got.score < 0 {
			t.Errorf("score = %v, want in [0,1]", got.score)
		}
	})
}

func hasReason(reasons []string, want string) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}

func hasReasonPrefix(reasons []string, prefix string) bool {
	for _, r := range reasons {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}
