package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
)

// substringReranker scores a candidate by the weight of the first marker
// substring it contains — a deterministic stand-in for a cross-encoder that
// lets a test dictate the reranked order independent of retrieval relevance.
type substringReranker struct {
	weights  map[string]float64
	err      error
	gotQuery string
	gotDocs  []string
}

func (s *substringReranker) Rerank(_ context.Context, query string, docs []string) ([]float64, error) {
	s.gotQuery, s.gotDocs = query, docs
	if s.err != nil {
		return nil, s.err
	}
	out := make([]float64, len(docs))
	for i, d := range docs {
		for sub, w := range s.weights {
			if strings.Contains(d, sub) {
				out[i] = w
			}
		}
	}
	return out, nil
}

// rerankFixtureDocs: retrieval relevance ranks alpha > beta > gamma; the
// reranker weights invert that (gamma > beta > alpha).
func rerankFixtureDocs() []docSpec {
	return []docSpec{
		{id: "m-alpha", typ: "EMAIL", title: "alpha budget", relevance: 0.9},
		{id: "m-beta", typ: "EMAIL", title: "beta budget", relevance: 0.5},
		{id: "m-gamma", typ: "EMAIL", title: "gamma budget", relevance: 0.1},
	}
}

func TestRerankReordersEndToEnd(t *testing.T) {
	loader := newFakeLoader() // alice cold-starts to DefaultProfile
	rr := &substringReranker{weights: map[string]float64{"alpha": 1, "beta": 5, "gamma": 9}}
	// clip down so the (personalized, non-RRF) hybrid arm's fixture score is the
	// retrieval relevance — no CLIP fusion muddying the baseline order.
	env := newQueryEnv(t, withProfiles(loader, false), withClipDown(), withReranker(rr))
	env.vespa.setProfileFixture("hybrid", buildFixture(t, rerankFixtureDocs()))

	// Baseline: rerank OFF -> order follows retrieval relevance (alpha first).
	off, err := env.client.Search(tenantCtx(t, "alice"), &queryv1.SearchRequest{Query: "budget review"})
	if err != nil {
		t.Fatalf("search (rerank off): %v", err)
	}
	if ids := hitIDs(off); len(ids) == 0 || ids[0] != "m-alpha" {
		t.Fatalf("baseline top = %v, want m-alpha first", hitIDs(off))
	}

	// Rerank ON -> the cross-encoder inverts the order (gamma first).
	on, err := env.client.Search(tenantCtx(t, "alice"), &queryv1.SearchRequest{Query: "budget review", Rerank: true})
	if err != nil {
		t.Fatalf("search (rerank on): %v", err)
	}
	if ids := hitIDs(on); len(ids) == 0 || ids[0] != "m-gamma" {
		t.Fatalf("reranked top = %v, want m-gamma first", hitIDs(on))
	}
	if on.GetDegraded() == "rerank-unavailable" || strings.Contains(on.GetDegraded(), "rerank-unavailable") {
		t.Fatalf("rerank should have applied, not degraded: %q", on.GetDegraded())
	}
	if rr.gotQuery != "budget review" || len(rr.gotDocs) != 3 {
		t.Fatalf("reranker saw query=%q, %d docs", rr.gotQuery, len(rr.gotDocs))
	}
}

func TestRerankFailureDegradesGracefully(t *testing.T) {
	loader := newFakeLoader()
	rr := &substringReranker{err: errors.New("reranker down")}
	env := newQueryEnv(t, withProfiles(loader, false), withClipDown(), withReranker(rr))
	env.vespa.setProfileFixture("hybrid", buildFixture(t, rerankFixtureDocs()))

	resp, err := env.client.Search(tenantCtx(t, "alice"), &queryv1.SearchRequest{Query: "budget review", Rerank: true})
	if err != nil {
		t.Fatalf("search: %v", err) // a reranker failure must NOT fail the search
	}
	if !strings.Contains(resp.GetDegraded(), "rerank-unavailable") {
		t.Fatalf("degraded = %q, want it to contain rerank-unavailable", resp.GetDegraded())
	}
	// Fused order still served (falls back to retrieval relevance: alpha first).
	if ids := hitIDs(resp); len(ids) == 0 || ids[0] != "m-alpha" {
		t.Fatalf("degraded results = %v, want fused order (m-alpha first)", hitIDs(resp))
	}
}

func TestRerankIgnoredWithoutRerankerWired(t *testing.T) {
	// Rerank requested but no reranker on the server -> flag is a no-op, no degrade.
	loader := newFakeLoader()
	env := newQueryEnv(t, withProfiles(loader, false), withClipDown())
	env.vespa.setProfileFixture("hybrid", buildFixture(t, rerankFixtureDocs()))

	resp, err := env.client.Search(tenantCtx(t, "alice"), &queryv1.SearchRequest{Query: "budget review", Rerank: true})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if strings.Contains(resp.GetDegraded(), "rerank-unavailable") {
		t.Fatalf("no reranker wired should not degrade rerank: %q", resp.GetDegraded())
	}
	if ids := hitIDs(resp); len(ids) == 0 || ids[0] != "m-alpha" {
		t.Fatalf("top = %v, want retrieval order m-alpha", hitIDs(resp))
	}
}
