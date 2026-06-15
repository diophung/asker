package main

import (
	"math"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
)

var rerankNow = time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

func hitAt(id string, score float64, modified time.Time) *queryv1.Hit {
	h := &queryv1.Hit{DocId: id, Score: score}
	if !modified.IsZero() {
		h.Modified = timestamppb.New(modified)
	}
	return h
}

func ids(hits []*queryv1.Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.GetDocId()
	}
	return out
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRerankByRecencyDisabled(t *testing.T) {
	day := 24 * time.Hour
	for name, weight := range map[string]float64{"zero": 0, "negative": -0.5} {
		t.Run(name, func(t *testing.T) {
			hits := []*queryv1.Hit{
				hitAt("a", 0.9, rerankNow.Add(-365*day)), // most relevant, ancient
				hitAt("b", 0.1, rerankNow),               // least relevant, fresh
			}
			rerankByRecency(hits, weight, 30*day, rerankNow)
			if got := ids(hits); !eqStrings(got, []string{"a", "b"}) {
				t.Errorf("order = %v, want [a b] (unchanged when disabled)", got)
			}
		})
	}
}

func TestRerankByRecencyShortInputsAndHalfLife(t *testing.T) {
	day := 24 * time.Hour
	// <2 hits: nothing to reorder, must not panic.
	rerankByRecency(nil, 0.4, 30*day, rerankNow)
	rerankByRecency([]*queryv1.Hit{hitAt("only", 0.5, rerankNow)}, 0.4, 30*day, rerankNow)
	// Non-positive half-life disables the blend.
	hits := []*queryv1.Hit{hitAt("a", 0.9, rerankNow.Add(-365*day)), hitAt("b", 0.1, rerankNow)}
	rerankByRecency(hits, 0.4, 0, rerankNow)
	if got := ids(hits); !eqStrings(got, []string{"a", "b"}) {
		t.Errorf("order = %v, want [a b] (half-life<=0 disables)", got)
	}
}

func TestRerankByRecencyFreshRises(t *testing.T) {
	day := 24 * time.Hour
	// a is most relevant but a year old; b is slightly less relevant but brand
	// new. With a strong recency weight the fresh item should win.
	hits := []*queryv1.Hit{
		hitAt("a", 1.0, rerankNow.Add(-365*day)),
		hitAt("b", 0.8, rerankNow),
	}
	rerankByRecency(hits, 0.6, 30*day, rerankNow)
	if got := ids(hits); got[0] != "b" {
		t.Errorf("order = %v, want fresh 'b' first", got)
	}
}

func TestRerankByRecencyRelevanceStillWinsWhenClose(t *testing.T) {
	day := 24 * time.Hour
	// Both recent (same day): relevance alone should order them, regardless of
	// the recency term (equal recency contribution).
	hits := []*queryv1.Hit{
		hitAt("low", 0.2, rerankNow.Add(-1*day)),
		hitAt("high", 0.95, rerankNow.Add(-1*day)),
	}
	rerankByRecency(hits, 0.4, 30*day, rerankNow)
	if got := ids(hits); got[0] != "high" {
		t.Errorf("order = %v, want 'high' first (equal recency => relevance decides)", got)
	}
}

func TestRerankByRecencyTiesBrokenByRecency(t *testing.T) {
	day := 24 * time.Hour
	// All-equal relevance (e.g. filter-only nativeRank ties): normRelevance is 1
	// for all, so recency fully decides — newest first.
	hits := []*queryv1.Hit{
		hitAt("old", 0.5, rerankNow.Add(-100*day)),
		hitAt("new", 0.5, rerankNow.Add(-1*day)),
		hitAt("mid", 0.5, rerankNow.Add(-10*day)),
	}
	rerankByRecency(hits, 0.4, 30*day, rerankNow)
	if got := ids(hits); !eqStrings(got, []string{"new", "mid", "old"}) {
		t.Errorf("order = %v, want [new mid old]", got)
	}
}

func TestRerankByRecencyNoTimestampRanksOnRelevance(t *testing.T) {
	day := 24 * time.Hour
	// A hit with no date gets recency 0; with a low recency weight a much more
	// relevant undated hit still beats a fresh but barely-relevant one.
	hits := []*queryv1.Hit{
		hitAt("fresh-weak", 0.05, rerankNow),
		hitAt("undated-strong", 1.0, time.Time{}),
	}
	rerankByRecency(hits, 0.3, 30*day, rerankNow)
	if got := ids(hits); got[0] != "undated-strong" {
		t.Errorf("order = %v, want 'undated-strong' first", got)
	}
}

func TestRerankByRecencyStableForEqualBlend(t *testing.T) {
	// Identical score AND identical timestamp => identical blend => stable order
	// (arrival order preserved).
	ts := rerankNow.Add(-5 * 24 * time.Hour)
	hits := []*queryv1.Hit{
		hitAt("first", 0.5, ts),
		hitAt("second", 0.5, ts),
		hitAt("third", 0.5, ts),
	}
	rerankByRecency(hits, 0.4, 30*24*time.Hour, rerankNow)
	if got := ids(hits); !eqStrings(got, []string{"first", "second", "third"}) {
		t.Errorf("order = %v, want stable [first second third]", got)
	}
}

func TestRecencyScore(t *testing.T) {
	halfLife := 30 * 24 * time.Hour
	if got := recencyScore(time.Time{}, rerankNow, halfLife); got != 0 {
		t.Errorf("zero time recency = %v, want 0", got)
	}
	if got := recencyScore(rerankNow.Add(time.Hour), rerankNow, halfLife); got != 1 {
		t.Errorf("future recency = %v, want 1 (clamped)", got)
	}
	if got := recencyScore(rerankNow, rerankNow, halfLife); got != 1 {
		t.Errorf("now recency = %v, want 1", got)
	}
	// One half-life old => 0.5.
	if got := recencyScore(rerankNow.Add(-halfLife), rerankNow, halfLife); math.Abs(got-0.5) > 1e-9 {
		t.Errorf("one-half-life recency = %v, want 0.5", got)
	}
	// Two half-lives old => 0.25.
	if got := recencyScore(rerankNow.Add(-2*halfLife), rerankNow, halfLife); math.Abs(got-0.25) > 1e-9 {
		t.Errorf("two-half-life recency = %v, want 0.25", got)
	}
}

func TestHitTimePrefersModified(t *testing.T) {
	created := rerankNow.Add(-10 * 24 * time.Hour)
	modified := rerankNow.Add(-1 * 24 * time.Hour)
	h := &queryv1.Hit{Created: timestamppb.New(created), Modified: timestamppb.New(modified)}
	if got := hitTime(h); !got.Equal(modified) {
		t.Errorf("hitTime = %v, want modified %v", got, modified)
	}
	// Created-only falls back to created.
	h2 := &queryv1.Hit{Created: timestamppb.New(created)}
	if got := hitTime(h2); !got.Equal(created) {
		t.Errorf("hitTime = %v, want created %v", got, created)
	}
	// Neither => zero.
	if got := hitTime(&queryv1.Hit{}); !got.IsZero() {
		t.Errorf("hitTime = %v, want zero", got)
	}
}
