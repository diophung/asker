package main

import (
	"sort"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
)

// rrfK is the Reciprocal Rank Fusion damping constant. 60 is the value from the
// original Cormack et al. RRF paper and the de-facto standard: large enough that
// the difference between rank 1 and rank 2 does not dominate, small enough that
// deep ranks contribute little.
const rrfK = 60

// rrfFuse fuses several ranked hit lists into one by Reciprocal Rank Fusion
// (spec v3.2 §1.4 "fuse with Reciprocal Rank Fusion … the standard that makes
// results robust"). Each document's fused score is
//
//	Σ over lists  1 / (rrfK + rank)      (rank is 1-based within each list)
//
// so a document ranked highly by EITHER the sparse/keyword arm OR the dense/
// vector arm rises, which is exactly why hybrid + RRF beats a single linear
// blend for queries where one arm misses (exact names/IDs vs. paraphrased
// intent). RRF needs only the RANK from each arm, so arms with incomparable raw
// score scales (nativeRank vs. cosine closeness vs. CLIP closeness) fuse
// cleanly.
//
// Documents are deduped by doc_id; the first list a document appears in is
// authoritative for its non-score fields, and media deep-link fields (modality/
// offsets/thumbnail) are inherited from whichever arm carries them (as in
// mergeHits). The fused score is written to Hit.Score so the downstream
// personalized re-rank sees a meaningful, min-max-normalizable relevance.
// Result order is fused score desc, stable by first-seen order on ties.
func rrfFuse(lists ...[]*queryv1.Hit) []*queryv1.Hit {
	order := make([]string, 0)
	byDoc := make(map[string]*queryv1.Hit)
	fused := make(map[string]float64)

	for _, list := range lists {
		for rank, h := range list {
			id := h.GetDocId()
			if id == "" {
				continue
			}
			fused[id] += 1.0 / float64(rrfK+rank+1) // rank+1 => 1-based
			existing, ok := byDoc[id]
			if !ok {
				// The arms' hits are freshly allocated by parseVespaResponse and
				// used nowhere else after fusion, so the first arm's hit is the
				// representative and is mutated in place (as mergeHits does) — its
				// Score is overwritten with the fused score below.
				byDoc[id] = h
				order = append(order, id)
				continue
			}
			inheritMediaFields(existing, h)
		}
	}

	out := make([]*queryv1.Hit, 0, len(order))
	for _, id := range order {
		h := byDoc[id]
		h.Score = fused[id]
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].GetScore() > out[j].GetScore()
	})
	return out
}

// inheritMediaFields fills media deep-link fields on dst from src when dst lacks
// them (a doc matched by a text arm inheriting the visual chunk anchor from the
// CLIP arm) — the same rule mergeHits applies.
func inheritMediaFields(dst, src *queryv1.Hit) {
	if dst.GetModality() == "" && src.GetModality() != "" {
		dst.StartMs = src.GetStartMs()
		dst.EndMs = src.GetEndMs()
		dst.Modality = src.GetModality()
	}
	if dst.GetThumbnailKey() == "" && src.GetThumbnailKey() != "" {
		dst.ThumbnailKey = src.GetThumbnailKey()
	}
}
