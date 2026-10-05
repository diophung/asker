package main

// Cross-encoder rerank stage (Phase 1). This sits BETWEEN retrieval/fusion and
// the final ordering: it re-scores the top fused candidates with a cross-encoder
// (query, doc) model. The reranked head precedes the unchanged retrieval tail.
// Scores become ordinal relevance on one scale across that complete order, so
// downstream preference/recency ranking never compares raw logits with fused
// retrieval scores and pagination retains candidates below the model depth.

import (
	"context"
	"math"
	"sort"
	"strings"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
)

// applyRerank reranks the top maxCand candidates of hits with the cross-encoder
// and returns the reordered head followed by the original tail. On success all
// candidates receive a common ordinal Score of (population - index)/population;
// on any reranker error it returns (nil, err) and the caller keeps the fused
// order (degrading, never failing). A query with no candidates is a no-op.
func applyRerank(ctx context.Context, r reranker, query string, hits []*queryv1.Hit, passages map[string]string, correctTypos bool, maxCand, docChars int) ([]*queryv1.Hit, error) {
	if len(hits) == 0 {
		return hits, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n := len(hits)
	if maxCand > 0 && maxCand < n {
		n = maxCand
	}
	cand := hits[:n]

	docs := make([]string, n)
	for i, h := range cand {
		passage := passages[h.GetDocId()]
		if passage == "" {
			passage = h.GetSnippet()
		}
		docs[i] = rerankTitlePassage(h.GetTitle(), passage, docChars)
	}
	if correctTypos {
		query = correctRerankQuery(ctx, query, cand)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, score := range scores {
		if math.IsNaN(score) || math.IsInf(score, 0) {
			return nil, errRerankShape
		}
	}

	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		return scores[order[i]] > scores[order[j]]
	})
	reranked := make([]*queryv1.Hit, len(hits))
	for i, index := range order {
		reranked[i] = cand[index]
	}
	copy(reranked[n:], hits[n:])
	for i, h := range reranked {
		h.Score = float64(len(reranked)-i) / float64(len(reranked))
	}
	return reranked, nil
}

// rerankDocText builds the passage handed to the cross-encoder for a hit: the
// title and snippet joined, capped at docChars runes (cross-encoders truncate
// long inputs anyway, and this bounds payload/latency). The snippet already
// carries the matched region, so it is a strong reranking signal even though the
// full body is not on the hit.
func rerankDocText(h *queryv1.Hit, docChars int) string {
	return rerankTitlePassage(h.GetTitle(), h.GetSnippet(), docChars)
}

func rerankTitlePassage(title, passage string, docChars int) string {
	var b strings.Builder
	if t := strings.TrimSpace(title); t != "" {
		b.WriteString(t)
	}
	if s := stripHighlights(passage); s != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(s)
	}
	return truncateRunes(b.String(), docChars)
}

// maxRerankPassageRunes matches the largest supported reranker passage budget.
// Keeping the richer passage separate from the UI teaser avoids silently
// reducing a configured 1024-rune model input to a 300-rune display snippet.
const maxRerankPassageRunes = 8192

func chooseRerankPassage(f vespaHitFields) string {
	fragments := make([]string, 0, len(f.ChunkSnippets)+1)
	// Matched fragments lead; unhighlighted chunks then supply useful context
	// for paraphrases and dense-only matches without another retrieval request.
	if strings.Contains(f.Snippet, "<hi>") {
		fragments = append(fragments, f.Snippet)
	}
	for _, chunk := range f.ChunkSnippets {
		if strings.Contains(chunk, "<hi>") {
			fragments = append(fragments, chunk)
		}
	}
	fragments = append(fragments, f.Snippet)
	fragments = append(fragments, f.ChunkSnippets...)
	return joinRerankPassages(fragments...)
}

func joinRerankPassages(fragments ...string) string {
	var passage string
	for _, fragment := range fragments {
		fragment = stripHighlights(fragment)
		if fragment == "" || strings.Contains(passage, fragment) {
			continue
		}
		if strings.Contains(fragment, passage) {
			passage = fragment
		} else {
			passage += "\n" + fragment
		}
		if len([]rune(passage)) >= maxRerankPassageRunes {
			return truncateRunes(passage, maxRerankPassageRunes)
		}
	}
	return passage
}

func mergeRerankPassages(results ...vespaResult) map[string]string {
	passages := make(map[string]string)
	for _, result := range results {
		for id, passage := range result.Passages {
			passages[id] = joinRerankPassages(passages[id], passage)
		}
	}
	return passages
}

// stripHighlights removes the <hi>...</hi> markers the snippet uses for
// term highlighting, leaving the plain passage text for the reranker.
func stripHighlights(s string) string {
	s = strings.ReplaceAll(s, "<hi>", "")
	s = strings.ReplaceAll(s, "</hi>", "")
	return strings.TrimSpace(s)
}
