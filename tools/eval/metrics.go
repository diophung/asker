package main

// Retrieval-quality metrics for the Asker eval harness: Recall@k, nDCG@k, and
// reciprocal rank (the per-query term of MRR). All functions are pure and
// operate on a ranked list of doc_ids plus a Judged relevance map, so they are
// engine-agnostic — the same code scores lexical-only, dense-only, hybrid, and
// hybrid+rerank result lists. Definitions follow the standard TREC/BEIR
// conventions (1-based ranks; log2 gain discount).

import (
	"math"
	"sort"
)

// Judged holds the graded relevance for one golden query: doc_id -> gain. A
// gain > 0 marks a relevant document; binary golden sets use gain 1. Docs not
// present in the map are treated as non-relevant (gain 0).
type Judged map[string]float64

// gainOf returns the relevance gain for docID (0 when not judged relevant).
func (j Judged) gainOf(docID string) float64 { return j[docID] }

// totalRelevant is the number of judged-relevant docs (gain > 0).
func (j Judged) totalRelevant() int {
	n := 0
	for _, g := range j {
		if g > 0 {
			n++
		}
	}
	return n
}

// dedupeStable returns ranked with empty and repeated doc_ids removed, keeping
// the first occurrence. A well-behaved query service already dedupes by doc_id;
// this makes the metrics robust if it doesn't, and keeps rank positions
// consistent across all three metrics.
func dedupeStable(ranked []string) []string {
	seen := make(map[string]bool, len(ranked))
	out := make([]string, 0, len(ranked))
	for _, id := range ranked {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// RecallAtK is the fraction of all relevant docs that appear in the top-k of
// ranked. k <= 0 means "no cutoff" (whole list). Returns NaN when the golden
// record judges no relevant docs (an ill-formed record), so callers can exclude
// it from an average rather than silently scoring it 0.
func RecallAtK(ranked []string, j Judged, k int) float64 {
	total := j.totalRelevant()
	if total == 0 {
		return math.NaN()
	}
	hits := 0
	for i, id := range dedupeStable(ranked) {
		if k > 0 && i >= k {
			break
		}
		if j.gainOf(id) > 0 {
			hits++
		}
	}
	return float64(hits) / float64(total)
}

// ReciprocalRank returns 1/rank of the first relevant doc (1-based), or 0 when
// no relevant doc appears in the list. Averaged over queries this is MRR.
func ReciprocalRank(ranked []string, j Judged) float64 {
	for i, id := range dedupeStable(ranked) {
		if j.gainOf(id) > 0 {
			return 1.0 / float64(i+1)
		}
	}
	return 0
}

// NDCGAtK is Discounted Cumulative Gain at k normalized by the ideal DCG.
// Graded gains are honored; binary golden sets (gain 1) reduce to standard
// nDCG. k <= 0 means "no cutoff". Returns 0 when the ideal DCG is 0 (no
// relevant docs), matching BEIR's convention.
func NDCGAtK(ranked []string, j Judged, k int) float64 {
	var dcg float64
	for i, id := range dedupeStable(ranked) {
		if k > 0 && i >= k {
			break
		}
		if g := j.gainOf(id); g > 0 {
			dcg += g / math.Log2(float64(i+2)) // rank i+1, discount log2(rank+1)
		}
	}

	gains := make([]float64, 0, len(j))
	for _, g := range j {
		if g > 0 {
			gains = append(gains, g)
		}
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(gains)))
	var idcg float64
	for i, g := range gains {
		if k > 0 && i >= k {
			break
		}
		idcg += g / math.Log2(float64(i+2))
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

// meanIgnoringNaN averages xs, skipping NaN entries, and returns the count of
// non-NaN values used. Returns (NaN, 0) when every entry is NaN/empty.
func meanIgnoringNaN(xs []float64) (float64, int) {
	var sum float64
	n := 0
	for _, x := range xs {
		if math.IsNaN(x) {
			continue
		}
		sum += x
		n++
	}
	if n == 0 {
		return math.NaN(), 0
	}
	return sum / float64(n), n
}

// percentile returns the nearest-rank p-th percentile (p in [0,100]) of xs.
// Returns NaN for an empty slice. Used for latency p50/p95.
func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	switch {
	case p <= 0:
		return s[0]
	case p >= 100:
		return s[len(s)-1]
	}
	rank := int(math.Ceil(p/100*float64(len(s)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(s) {
		rank = len(s) - 1
	}
	return s[rank]
}
