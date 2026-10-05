package main

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/asker/asker/platform/personalization"
	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// The existing redundant-passage fixture plus a zero-relevance anchor keeps
// its normalized relevance [1, .99, .95] unchanged through the full ranker.
func diversificationScoreFixture() []*queryv1.Hit {
	return []*queryv1.Hit{
		{DocId: "first", Type: askerv1.DocType_FILE, Title: "Restore corrupted database backups", Score: 1,
			Metadata: map[string]string{"from": "alice@example.com"}},
		{DocId: "copy", Type: askerv1.DocType_FILE, Title: "Restore corrupted database backups", Score: .99,
			Metadata: map[string]string{"from": "alice@example.com"}},
		{DocId: "distinct", Type: askerv1.DocType_FILE, Title: "Release authorization signature", Score: .95,
			Metadata: map[string]string{"from": "alice@example.com"}},
		{DocId: "floor", Type: askerv1.DocType_FILE, Title: "Weekend weather forecast", Score: 0,
			Metadata: map[string]string{"from": "alice@example.com"}},
	}
}

func assertFiniteDescendingScores(t *testing.T, hits []*queryv1.Hit) {
	t.Helper()
	for i, h := range hits {
		if math.IsNaN(h.Score) || math.IsInf(h.Score, 0) {
			t.Fatalf("non-finite score at %d: %v", i, h.Score)
		}
		if i > 0 && hits[i-1].Score < h.Score-1e-12 {
			t.Fatalf("scores oppose final order: %v before %v", hits[i-1].Score, h.Score)
		}
	}
}

func TestPersonalizeMMRPublishesSelectionUtilityWithoutChangingOrder(t *testing.T) {
	cases := []struct {
		name    string
		novelty float64
		ids     []string
		scores  []float64
	}{
		{"cold-start", .15, []string{"first", "distinct", "copy", "floor"}, []float64{.85, .8075, .6915, 0}},
		{"existing-diversification", .3, []string{"first", "distinct", "copy", "floor"}, []float64{.7, .665, .393, 0}},
		{"maximum-novelty", 1, []string{"first", "distinct", "floor", "copy"}, []float64{0, 0, 0, -1}},
	}
	for _, tc := range cases {
		for _, debug := range []bool{false, true} {
			name := tc.name + "/public"
			if debug {
				name = tc.name + "/debug"
			}
			t.Run(name, func(t *testing.T) {
				profile := personalization.DefaultProfile()
				profile.RecencyVsImportance = 0
				profile.NoveltyVsFamiliarity = tc.novelty
				params := personalizeParams{profile: profile, limit: 20, debug: debug}
				got, total := personalizeRank(diversificationScoreFixture(), params)
				if !reflect.DeepEqual(hitIDsOf(got), tc.ids) || total != 4 {
					t.Fatalf("existing diversified order changed: %v total=%d", hitIDsOf(got), total)
				}
				assertFiniteDescendingScores(t, got)
				raw := map[string]float64{"first": 1, "copy": .99, "distinct": .95, "floor": 0}
				for i, h := range got {
					if math.Abs(h.Score-tc.scores[i]) > 1e-12 {
						t.Fatalf("%s score=%v want actual selected utility %v", h.DocId, h.Score, tc.scores[i])
					}
					if !debug {
						if len(h.Features) != 0 {
							t.Fatalf("debug scoring stages exposed without debug: %v", h.Features)
						}
						continue
					}
					if h.Features["retrieval_score"] != raw[h.DocId] ||
						math.Abs(h.Features["combined_score"]-(raw[h.DocId]+.25)) > 1e-12 ||
						h.Features["ranking_score"] != h.Score {
						t.Fatalf("earlier scores lost for %s: %v", h.DocId, h.Features)
					}
					reconstructed := (1-tc.novelty)*h.Features["mmr_relevance"] - h.Features["mmr_penalty"]
					if math.Abs(reconstructed-h.Score) > 1e-12 {
						t.Fatalf("debug stages cannot explain selected score: %v", h.Features)
					}
				}
			})
		}
	}
}

func TestPersonalizeScoresDescribePreferencesWithoutMMR(t *testing.T) {
	profile := personalization.DefaultProfile()
	profile.RecencyVsImportance = 0
	profile.NoveltyVsFamiliarity = 0
	profile.ImportantPeople = []string{"vip@example.com"}
	hits := []*queryv1.Hit{
		{DocId: "plain", Type: askerv1.DocType_EMAIL, Score: .9},
		{DocId: "important", Type: askerv1.DocType_EMAIL, Score: .85,
			Metadata: map[string]string{"from": "vip@example.com"}},
		{DocId: "floor", Type: askerv1.DocType_EMAIL, Score: 0},
	}
	got, total := personalizeRank(hits, personalizeParams{profile: profile, limit: 20, debug: true})
	if !reflect.DeepEqual(hitIDsOf(got), []string{"important", "plain", "floor"}) || total != 3 {
		t.Fatalf("preference order changed: %v total=%d", hitIDsOf(got), total)
	}
	assertFiniteDescendingScores(t, got)
	if got[0].Features["retrieval_score"] != .85 || got[1].Features["retrieval_score"] != .9 {
		t.Fatalf("original retrieval scores were not retained: %v/%v", got[0].Features, got[1].Features)
	}
	for _, h := range got {
		if h.Score != h.Features["combined_score"] {
			t.Fatalf("non-diversified score does not expose combined relevance: %v", h.Features)
		}
		if _, ok := h.Features["mmr_penalty"]; ok {
			t.Fatalf("inactive MMR stage reported: %v", h.Features)
		}
	}
}

func TestPersonalizeFinalScoresAreStableAcrossPages(t *testing.T) {
	profile := personalization.DefaultProfile()
	profile.RecencyVsImportance = 0
	params := personalizeParams{profile: profile, limit: 20}
	full, _ := personalizeRank(diversificationScoreFixture(), params)
	params.offset, params.limit = 2, 2
	page, total := personalizeRank(diversificationScoreFixture(), params)
	if !reflect.DeepEqual(hitIDsOf(page), []string{"copy", "floor"}) || total != 4 {
		t.Fatalf("final scoring changed page membership: %v total=%d", hitIDsOf(page), total)
	}
	for i, h := range page {
		if h.Score != full[i+2].Score {
			t.Fatalf("pagination renormalized final score: %v vs %v", h.Score, full[i+2].Score)
		}
	}
}

func TestPersonalizeFinalScoresPreserveCalendarChronology(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	profile := personalization.DefaultProfile()
	profile.RecencyVsImportance = 0
	hits := []*queryv1.Hit{
		{DocId: "later", Type: askerv1.DocType_CALENDAR_EVENT, Score: .9,
			Metadata: map[string]string{"start": now.Add(2 * time.Hour).Format(time.RFC3339)}},
		{DocId: "earlier-low", Type: askerv1.DocType_CALENDAR_EVENT, Score: .2,
			Metadata: map[string]string{"start": now.Add(time.Hour).Format(time.RFC3339)}},
		{DocId: "earlier-high", Type: askerv1.DocType_CALENDAR_EVENT, Score: .8,
			Metadata: map[string]string{"start": now.Add(time.Hour).Format(time.RFC3339)}},
	}
	got, total := personalizeRank(hits, personalizeParams{
		profile: profile, intent: intentScheduleLookup, now: now, limit: 20, debug: true,
	})
	if !reflect.DeepEqual(hitIDsOf(got), []string{"earlier-high", "earlier-low", "later"}) || total != 3 {
		t.Fatalf("calendar chronology or relevance tiebreak changed: %v", hitIDsOf(got))
	}
	if got[1].Score >= got[2].Score {
		t.Fatalf("calendar results were forced into score order: %v/%v", got[1].Score, got[2].Score)
	}
	for _, h := range got {
		if h.Score != h.Features["combined_score"] || math.IsNaN(h.Score) || math.IsInf(h.Score, 0) {
			t.Fatalf("calendar relevance is not finite combined relevance: %v", h.Features)
		}
		if _, ok := h.Features["mmr_penalty"]; ok {
			t.Fatalf("calendar chronology was diversified: %v", h.Features)
		}
	}
}

type cancelAfterScoreChecks struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (c *cancelAfterScoreChecks) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestPersonalizeFinalScoresDoNotMutateCanceledCandidates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		checks int
	}{{"before-ranking", 1}, {"during-mmr-selection", 14}} {
		t.Run(tc.name, func(t *testing.T) {
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &cancelAfterScoreChecks{Context: base, cancel: cancel, remaining: tc.checks}
			profile := personalization.DefaultProfile()
			profile.RecencyVsImportance = 0
			hits := diversificationScoreFixture()
			got, _ := personalizeRank(hits, personalizeParams{ctx: ctx, profile: profile, limit: 20, debug: true})
			if len(got) != 0 || ctx.Err() != context.Canceled {
				t.Fatalf("canceled ranking returned partial results: %v", got)
			}
			raw := []float64{1, .99, .95, 0}
			for i, h := range hits {
				if h.Score != raw[i] || len(h.Features) != 0 || h.Explanation != "" {
					t.Fatalf("canceled selection mutated hit %s: %v", h.DocId, h)
				}
			}
		})
	}
}

func TestPersonalizeFinalScoresRemainFiniteForAllowedLargeWeights(t *testing.T) {
	for _, novelty := range []float64{0, .15} {
		for _, debug := range []bool{false, true} {
			profile := personalization.DefaultProfile()
			profile.RecencyVsImportance = 0
			profile.NoveltyVsFamiliarity = novelty
			profile.Weights = personalization.Weights{Semantic: 1e308, Preference: 1e308, Behavioral: 1e308}
			profile.SourceWeights = map[string]float64{"FILE": 5, "EMAIL": 0}
			profile.Mute.Topics = []string{"ignoreme"}
			profile = personalization.Clamp(profile) // these finite weights are allowed
			if profile.Weights.Semantic != 1e308 {
				t.Fatal("test no longer exercises the allowed large-weight boundary")
			}
			hits := []*queryv1.Hit{
				{DocId: "high", Type: askerv1.DocType_FILE, Title: "Restoration procedures", Score: 1},
				{DocId: "middle", Type: askerv1.DocType_EMAIL, Title: "Routine message", Score: .5},
				{DocId: "low", Type: askerv1.DocType_EMAIL, Title: "ignoreme ignored content", Score: 0},
			}
			got, total := personalizeRank(hits, personalizeParams{profile: profile, limit: 20, debug: debug})
			if !reflect.DeepEqual(hitIDsOf(got), []string{"high", "middle", "low"}) || total != 3 {
				t.Fatalf("large-weight candidate order changed: %v", hitIDsOf(got))
			}
			assertFiniteDescendingScores(t, got)
			if _, err := json.Marshal(got); err != nil {
				t.Fatalf("score/debug features cannot be serialized: %v", err)
			}
			if debug {
				if got[0].Features["combined_score"] != math.MaxFloat64 || got[2].Features["combined_score"] != -5e307 {
					t.Fatalf("test did not exercise saturation/opposite-sign normalization: %v/%v", got[0].Features, got[2].Features)
				}
				for _, h := range got {
					for key, value := range h.Features {
						if math.IsNaN(value) || math.IsInf(value, 0) {
							t.Fatalf("non-finite debug feature %s=%v", key, value)
						}
					}
				}
			}
		}
	}
}

func TestPersonalizeFinalScoresNormalizeFiniteRetrievalExtremes(t *testing.T) {
	profile := personalization.DefaultProfile()
	profile.RecencyVsImportance = 0
	hits := []*queryv1.Hit{
		{DocId: "high", Title: "Restoration procedures", Score: math.MaxFloat64},
		{DocId: "middle", Title: "Routine message", Score: 0},
		{DocId: "low", Title: "Unrelated archive", Score: -math.MaxFloat64},
	}
	got, _ := personalizeRank(hits, personalizeParams{profile: profile, limit: 20, debug: true})
	if !reflect.DeepEqual(hitIDsOf(got), []string{"high", "middle", "low"}) {
		t.Fatalf("finite retrieval boundary changed order: %v", hitIDsOf(got))
	}
	assertFiniteDescendingScores(t, got)
	if _, err := json.Marshal(got); err != nil {
		t.Fatalf("finite retrieval scores/debug cannot be serialized: %v", err)
	}
	if got[0].Features["retrieval_score"] != math.MaxFloat64 || got[2].Features["retrieval_score"] != -math.MaxFloat64 {
		t.Fatal("finite original retrieval diagnostics changed")
	}
}
