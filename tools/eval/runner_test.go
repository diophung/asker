package main

import (
	"context"
	"errors"
	"testing"
)

// fakeSearcher returns canned rankings keyed by pipeline name and query text.
type fakeSearcher struct {
	results map[string]map[string][]string
	latency float64
	err     error
}

func (f *fakeSearcher) Search(_ context.Context, _, query string, p Pipeline, _ int) (SearchOutcome, error) {
	if f.err != nil {
		return SearchOutcome{}, f.err
	}
	return SearchOutcome{DocIDs: f.results[p.Name][query], WallMs: f.latency}, nil
}

func aggFor(aggs []Aggregate, pipeline, slice string) (Aggregate, bool) {
	for _, a := range aggs {
		if a.Pipeline == pipeline && a.Slice == slice {
			return a, true
		}
	}
	return Aggregate{}, false
}

var evalRecs = []GoldenRecord{
	{ID: "e1", Query: "qzx", Tenant: "t", Slice: SliceExact, Relevant: []string{"a"}},
	{ID: "s1", Query: "concept", Tenant: "t", Slice: SliceSemantic, Relevant: []string{"b"}},
}

func TestRunAggregatesAndGatePasses(t *testing.T) {
	s := &fakeSearcher{latency: 12, results: map[string]map[string][]string{
		"hybrid":        {"qzx": {"x", "a"}, "concept": {"b", "x"}},
		"hybrid_rerank": {"qzx": {"a", "x"}, "concept": {"b", "x"}},
	}}
	cfg := RunConfig{
		K:     10,
		Limit: 10,
		Pipelines: []Pipeline{
			{Name: "hybrid", APIMode: "hybrid", Supported: true},
			{Name: "hybrid_rerank", APIMode: "hybrid", Supported: true},
		},
		Baseline:  "hybrid",
		Candidate: "hybrid_rerank",
	}
	report, _, err := Run(context.Background(), s, evalRecs, cfg)
	if err != nil {
		t.Fatal(err)
	}

	hyb, ok := aggFor(report.Aggregates, "hybrid", "")
	if !ok || hyb.N != 2 {
		t.Fatalf("hybrid overall missing or N=%d", hyb.N)
	}
	cand, _ := aggFor(report.Aggregates, "hybrid_rerank", "")
	if !(cand.NDCG > hyb.NDCG) {
		t.Fatalf("candidate overall nDCG %v should beat baseline %v", cand.NDCG, hyb.NDCG)
	}
	if cand.P50Ms != 12 {
		t.Fatalf("p50 latency=%v want 12", cand.P50Ms)
	}
	if !report.Gate.Pass || report.Gate.Skipped {
		t.Fatalf("gate should pass: %+v", report.Gate)
	}
}

func TestRunGateFailsOnSliceRegression(t *testing.T) {
	// Candidate improves exact but regresses semantic below baseline.
	s := &fakeSearcher{results: map[string]map[string][]string{
		"hybrid":        {"qzx": {"x", "a"}, "concept": {"b", "x"}},
		"hybrid_rerank": {"qzx": {"a", "x"}, "concept": {"x", "b"}},
	}}
	cfg := RunConfig{
		K: 10, Limit: 10,
		Pipelines: []Pipeline{
			{Name: "hybrid", APIMode: "hybrid", Supported: true},
			{Name: "hybrid_rerank", APIMode: "hybrid", Supported: true},
		},
		Baseline: "hybrid", Candidate: "hybrid_rerank",
	}
	report, _, _ := Run(context.Background(), s, evalRecs, cfg)
	if report.Gate.Pass {
		t.Fatalf("gate should fail on semantic regression")
	}
	found := false
	for _, v := range report.Gate.Violations {
		if v.Slice == SliceSemantic {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a semantic violation, got %+v", report.Gate.Violations)
	}
}

func TestRunGateSkippedWhenCandidateUnsupported(t *testing.T) {
	s := &fakeSearcher{results: map[string]map[string][]string{
		"hybrid": {"qzx": {"a"}, "concept": {"b"}},
	}}
	cfg := RunConfig{
		K: 10, Limit: 10,
		Pipelines: []Pipeline{
			{Name: "hybrid", APIMode: "hybrid", Supported: true},
			{Name: "hybrid_rerank", APIMode: "hybrid", Supported: false},
		},
		Baseline: "hybrid", Candidate: "hybrid_rerank",
	}
	report, _, _ := Run(context.Background(), s, evalRecs, cfg)
	if !report.Gate.Skipped {
		t.Fatalf("gate should be skipped when candidate unsupported: %+v", report.Gate)
	}
	// The unsupported pipeline still appears as an aggregate row (marked so).
	if a, ok := aggFor(report.Aggregates, "hybrid_rerank", ""); !ok || a.Supported {
		t.Fatalf("unsupported pipeline should have a row with Supported=false")
	}
}

func TestRunCountsErrors(t *testing.T) {
	s := &fakeSearcher{err: errors.New("boom")}
	cfg := RunConfig{
		K: 10, Limit: 10,
		Pipelines: []Pipeline{{Name: "hybrid", APIMode: "hybrid", Supported: true}},
		Baseline:  "hybrid", Candidate: "",
	}
	report, _, _ := Run(context.Background(), s, evalRecs, cfg)
	a, _ := aggFor(report.Aggregates, "hybrid", "")
	if a.Errors != 2 || a.N != 0 {
		t.Fatalf("want 2 errors/0 scored, got errors=%d n=%d", a.Errors, a.N)
	}
}
