package main

// Cross-encoder rerank stage (Phase 1). This sits BETWEEN retrieval/fusion and
// the final ordering: it re-scores the top fused candidates with a cross-encoder
// (query, doc) model and rewrites each candidate's Score with that relevance.
//
// The rewrite is the whole trick: both downstream orderings already read
// Hit.Score — the personalized re-rank's Semantic feature is Hit.Score min-max
// normalized (personalize.go), and the non-personalized rerankByRecency blends
// normalized Hit.Score with freshness — so replacing Score with the cross-encoder
// relevance improves BOTH with no change to those stages. To keep the Semantic
// normalization on a single consistent scale, the candidate set is TRUNCATED to
// the reranked top-N (the page is drawn from it anyway).

import (
	"context"
	"sort"
	"strings"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
)

// applyRerank reranks the top maxCand candidates of hits with the cross-encoder
// and returns them ordered by the new relevance. On success it returns the
// reranked candidate slice (length <= maxCand) with each hit's Score rewritten;
// on any reranker error it returns (nil, err) and the caller keeps the fused
// order (degrading, never failing). A query with no candidates is a no-op.
func applyRerank(ctx context.Context, r reranker, query string, hits []*queryv1.Hit, maxCand, docChars int) ([]*queryv1.Hit, error) {
	if len(hits) == 0 {
		return hits, nil
	}
	n := len(hits)
	if maxCand > 0 && maxCand < n {
		n = maxCand
	}
	cand := hits[:n]

	docs := make([]string, n)
	for i, h := range cand {
		docs[i] = rerankDocText(h, docChars)
	}
	scores, err := r.Rerank(ctx, query, docs)
	if err != nil {
		return nil, err
	}
	if len(scores) != n {
		// The client already guards this, but re-check so a misbehaving
		// implementation cannot index out of range below.
		return nil, errRerankShape
	}

	reranked := make([]*queryv1.Hit, n)
	copy(reranked, cand)
	for i, h := range reranked {
		h.Score = scores[i]
	}
	sort.SliceStable(reranked, func(i, j int) bool {
		return reranked[i].GetScore() > reranked[j].GetScore()
	})
	return reranked, nil
}

// rerankDocText builds the passage handed to the cross-encoder for a hit: the
// title and snippet joined, capped at docChars runes (cross-encoders truncate
// long inputs anyway, and this bounds payload/latency). The snippet already
// carries the matched region, so it is a strong reranking signal even though the
// full body is not on the hit.
func rerankDocText(h *queryv1.Hit, docChars int) string {
	var b strings.Builder
	if t := strings.TrimSpace(h.GetTitle()); t != "" {
		b.WriteString(t)
	}
	if s := stripHighlights(h.GetSnippet()); s != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(s)
	}
	return truncateRunes(b.String(), docChars)
}

// stripHighlights removes the <hi>...</hi> markers the snippet uses for
// term highlighting, leaving the plain passage text for the reranker.
func stripHighlights(s string) string {
	s = strings.ReplaceAll(s, "<hi>", "")
	s = strings.ReplaceAll(s, "</hi>", "")
	return strings.TrimSpace(s)
}
