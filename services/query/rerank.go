package main

import (
	"math"
	"sort"
	"time"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
)

// rerankByRecency reorders hits in place to implement the product rule
// "most recent, most relevant first": the page is sorted by a blend of the
// retrieval relevance score and the hit's freshness, rather than by relevance
// alone (which surfaces old-but-relevant items above fresh ones).
//
// The blended score for each hit is
//
//	blended = (1-weight)*normRelevance + weight*recency
//
// where:
//
//   - normRelevance is the hit's relevance min-max-normalized across the page,
//     so the blend is invariant to the absolute scale of the Vespa score (which
//     differs per ranking profile: nativeRank vs the hybrid linear blend). When
//     every hit shares the same relevance (e.g. a filter-only nativeRank tie),
//     normRelevance is 1 for all and recency alone decides the order.
//   - recency is an exponential decay of the hit's age with the given half-life,
//     in [0,1]: 1 at now, 0.5 one half-life old, →0 for ancient items. A hit with
//     no timestamp gets recency 0 — it cannot be shown to be fresh, so it ranks on
//     relevance only.
//
// weight is clamped to [0,1]; weight<=0 (or <2 hits, or a non-positive
// half-life) leaves the incoming relevance order untouched. The sort is stable,
// so hits with equal blended scores keep their arrival (relevance-ranked) order.
//
// This operates on the page the query already retrieved. For the gateway's
// single-page UI (offset 0) that is exactly the result surface; deeper
// pagination reorders within each page, which is acceptable and documented.
func rerankByRecency(hits []*queryv1.Hit, weight float64, halfLife time.Duration, now time.Time) {
	if weight <= 0 || halfLife <= 0 || len(hits) < 2 {
		return
	}
	if weight > 1 {
		weight = 1
	}

	minScore, maxScore := hits[0].GetScore(), hits[0].GetScore()
	for _, h := range hits {
		s := h.GetScore()
		if s < minScore {
			minScore = s
		}
		if s > maxScore {
			maxScore = s
		}
	}
	span := maxScore - minScore

	type scored struct {
		hit     *queryv1.Hit
		blended float64
	}
	arr := make([]scored, len(hits))
	for i, h := range hits {
		rel := 1.0 // all-equal relevance => let recency decide
		if span > 0 {
			rel = (h.GetScore() - minScore) / span
		}
		rec := recencyScore(hitTime(h), now, halfLife)
		arr[i] = scored{hit: h, blended: (1-weight)*rel + weight*rec}
	}
	sort.SliceStable(arr, func(i, j int) bool { return arr[i].blended > arr[j].blended })
	for i := range arr {
		hits[i] = arr[i].hit
	}
}

// recencyScore maps a timestamp to a freshness value in [0,1] via exponential
// decay: 1 at now, 0.5 at one half-life of age, approaching 0 for very old
// items. A zero timestamp (no date on the hit) yields 0; a future timestamp
// (clock skew) is treated as now.
func recencyScore(t, now time.Time, halfLife time.Duration) float64 {
	if t.IsZero() {
		return 0
	}
	age := now.Sub(t)
	if age <= 0 {
		return 1
	}
	return math.Exp2(-float64(age) / float64(halfLife))
}

// hitTime is the hit's effective date for recency: the modified time when set,
// else the created time, else the zero time (no date).
func hitTime(h *queryv1.Hit) time.Time {
	if ts := h.GetModified(); ts != nil {
		return ts.AsTime()
	}
	if ts := h.GetCreated(); ts != nil {
		return ts.AsTime()
	}
	return time.Time{}
}
