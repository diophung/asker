package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
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

func TestRunGateFailsWhenCandidateUnsupported(t *testing.T) {
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
	if report.Gate.Skipped || report.Gate.Pass {
		t.Fatalf("unavailable candidate must fail qualification: %+v", report.Gate)
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
	if a.Errors != 2 || a.N != 2 || a.Scored != 0 || a.TaskSuccess != 0 || a.NDCG != 0 {
		t.Fatalf("want 2 attempted errors/0 scored, got errors=%d n=%d", a.Errors, a.N)
	}
}

func TestPreRerankRecallUsesActualHeadWithExplicitMeasuredCoverage(t *testing.T) {
	pipeline := Pipeline{Name: "hybrid_rerank", Supported: true}
	positive := GoldenRecord{ID: "positive", Query: "q", Tenant: "t", Slice: SliceSemantic, Relevant: []string{"a", "b"}}
	complete := SearchOutcome{DocIDs: []string{"a"}, PreRerankHead: &CandidateHead{DocIDs: []string{"a", "b", "x"}, Count: 40, Depth: 3}}
	measured := scoreOutcome(positive, pipeline, complete, 10)
	if measured.Recall != .5 || measured.PreRerankRecall == nil || *measured.PreRerankRecall != 1 || measured.PreRerankCount != 40 || measured.PreRerankDepth != 3 {
		t.Fatal("final-hit recall and actual head recall must remain distinct")
	}
	empty := scoreOutcome(positive, pipeline, SearchOutcome{PreRerankHead: &CandidateHead{DocIDs: []string{}, Count: 0, Depth: 0}}, 10)
	if empty.PreRerankRecall == nil || *empty.PreRerankRecall != 0 {
		t.Fatal("a complete empty head is a measured zero on a positive task")
	}
	unknown := scoreOutcome(positive, pipeline, SearchOutcome{DocIDs: []string{"a", "b"}}, 10)
	if unknown.PreRerankRecall != nil {
		t.Fatal("final hits cannot infer unobserved candidate recall")
	}
	noMatch := GoldenRecord{ID: "absent", Query: "missing", Tenant: "t", Slice: SliceNoMatch, NoMatch: true}
	excluded := scoreOutcome(noMatch, pipeline, complete, 10)
	if excluded.PreRerankRecall != nil {
		t.Fatal("no-match tasks do not have a positive candidate-recall denominator")
	}
	aggregates := aggregate([]Pipeline{pipeline}, []GoldenRecord{positive, positive, positive, noMatch}, []queryScore{measured, empty, unknown, excluded}, 1)
	overall, _ := aggFor(aggregates, pipeline.Name, "")
	if overall.PreRerankN != 2 || overall.PreRerankUnknown != 1 || overall.QualityN != 3 || overall.PreRerankRecall == nil || *overall.PreRerankRecall != .5 {
		t.Fatal("candidate recall must show the measured denominator and unknown sample count")
	}
	if !strings.Contains(renderMarkdown(Report{Aggregates: aggregates}), "pre-rerank head recall") {
		t.Fatal("report must identify the measured scope")
	}
}

func TestNoMatchAndExplicitConstraints(t *testing.T) {
	p := Pipeline{Name: "hybrid"}
	empty := GoldenRecord{ID: "empty", Slice: SliceNoMatch, NoMatch: true}
	if sc := scoreOutcome(empty, p, SearchOutcome{}, 10); !sc.TaskSuccess || sc.QualityEligible {
		t.Fatalf("empty no-match should succeed: %+v", sc)
	}
	if sc := scoreOutcome(empty, p, SearchOutcome{DocIDs: []string{"unexpected"}}, 10); sc.TaskSuccess {
		t.Fatal("unexpected no-match result must fail")
	}
	rec := GoldenRecord{ID: "multi", Slice: SliceMultiNeed, Relevant: []string{"a", "b"}, Required: [][]string{{"a"}, {"b"}}, Forbidden: []string{"bad"}}
	for _, ids := range [][]string{{"a"}, {"a", "b", "bad"}} {
		if sc := scoreOutcome(rec, p, SearchOutcome{DocIDs: ids}, 10); sc.TaskSuccess {
			t.Fatalf("incomplete/forbidden task passed: %v", ids)
		}
	}
	if sc := scoreOutcome(rec, p, SearchOutcome{DocIDs: []string{"b", "a"}}, 10); !sc.TaskSuccess {
		t.Fatalf("covered needs should pass: %+v", sc)
	}
}

func TestGateRejectsCacheDegradationUnknownExecutionAndLatency(t *testing.T) {
	cfg := RunConfig{Baseline: "base", Candidate: "cand", AllowEqual: true, RequireUncached: true, MaxP95Ms: 5000, MinLatencySamples: 100}
	base := Aggregate{Pipeline: "base", Supported: true, N: 100, Expected: 100, Scored: 100, QualityN: 100, NDCG: 1, TaskSuccess: 1}
	cand := base
	cand.Pipeline = "cand"
	cases := map[string]Aggregate{}
	for _, label := range []string{"cache", "degraded", "execution", "rerank", "errors", "slow", "missing"} {
		c := cand
		switch label {
		case "cache":
			c.CacheHits = 1
		case "degraded":
			c.Degraded = 1
		case "execution":
			c.ExecutionMissing = 1
		case "rerank":
			c.RerankUnfulfilled = 1
		case "errors":
			c.Errors = 1
		case "slow":
			c.P95Ms = 5001
		case "missing":
			c.N = 99
		}
		cases[label] = c
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			if gate := evaluateGate([]Aggregate{base, c}, cfg); gate.Pass || gate.Skipped {
				t.Fatalf("unqualified request must fail: %+v", gate)
			}
		})
	}
	if gate := evaluateGate([]Aggregate{base, cand}, cfg); !gate.Pass {
		t.Fatalf("qualified equal pipeline should pass explicitly relaxed gate: %+v", gate)
	}
}

func TestRerankExecutionMustBeProven(t *testing.T) {
	p := Pipeline{Name: "hybrid_rerank", Extra: url.Values{"rerank": {"1"}}}
	rec := GoldenRecord{ID: "q", Slice: SliceExact, Relevant: []string{"a"}}
	sc := scoreOutcome(rec, p, SearchOutcome{DocIDs: []string{"a"}}, 10)
	if !sc.ExecutionMissing || !sc.RerankUnfulfilled {
		t.Fatalf("omitted execution must be unqualified: %+v", sc)
	}
	sc = scoreOutcome(rec, p, SearchOutcome{DocIDs: []string{"a"}, ExecutionKnown: true, RerankRequested: true, RerankApplied: true}, 10)
	if sc.ExecutionMissing || sc.RerankUnfulfilled {
		t.Fatalf("confirmed execution must be qualified: %+v", sc)
	}
	sc = scoreOutcome(GoldenRecord{NoMatch: true}, p, SearchOutcome{ExecutionKnown: true, RerankRequested: true}, 10)
	if sc.RerankUnfulfilled {
		t.Fatal("empty result does not require a cross-encoder invocation")
	}
}

func TestUnsupportedAndAllErrorReportsMarshal(t *testing.T) {
	cfg := RunConfig{Pipelines: []Pipeline{{Name: "base", Supported: true}, {Name: "cand"}}, Baseline: "base", Candidate: "cand"}
	report, _, err := Run(context.Background(), &fakeSearcher{err: errors.New("failed")}, evalRecs, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(report); err != nil {
		t.Fatalf("all-error/unsupported reports must remain writable: %v", err)
	}
}

func TestRepeatedConcurrentRequestsHaveExplicitDenominators(t *testing.T) {
	s := &fakeSearcher{latency: 8, results: map[string]map[string][]string{"hybrid": {"qzx": {"a"}, "concept": {"b"}}}}
	cfg := RunConfig{Pipelines: []Pipeline{{Name: "hybrid", Supported: true}}, Baseline: "hybrid", Candidate: "hybrid", AllowEqual: true, Repeats: 3, Concurrency: 2}
	r, scores, err := Run(context.Background(), s, evalRecs, cfg)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := aggFor(r.Aggregates, "hybrid", "")
	if r.Queries != 2 || len(scores) != 6 || a.N != 6 || a.Expected != 6 || a.TaskSuccess != 1 {
		t.Fatalf("incorrect repeat denominators: %+v", r)
	}
}
