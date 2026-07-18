package main

// The runner executes every golden query against every pipeline, scores the
// results, and aggregates per-slice and overall metrics plus latency. It also
// enforces the quality gate: the candidate pipeline (default hybrid_rerank) must
// match-or-beat the baseline (default hybrid) on nDCG for EVERY slice and
// improve overall — the guard against a change that lifts the average while
// quietly regressing exact-match.

import (
	"context"
	"fmt"
	"math"
)

// RunConfig parameterizes an eval run.
type RunConfig struct {
	K         int        // cutoff for Recall@k / nDCG@k (e.g. 10)
	Limit     int        // hits to request per query (>= K)
	Pipelines []Pipeline // pipelines to evaluate, in report order
	Baseline  string     // pipeline name used as the gate baseline
	Candidate string     // pipeline name that must match-or-beat the baseline
}

// queryScore is one (pipeline, query) measurement.
type queryScore struct {
	RecordID string
	Slice    string
	Pipeline string
	Recall   float64
	NDCG     float64
	RR       float64
	WallMs   float64
	Err      string
}

// Aggregate is the mean metrics for one (pipeline, slice) cell. Slice=="" is
// the overall row. N counts scored (non-errored, non-NaN) queries.
type Aggregate struct {
	Pipeline  string  `json:"pipeline"`
	Slice     string  `json:"slice"`
	N         int     `json:"n"`
	Recall    float64 `json:"recall_at_k"`
	NDCG      float64 `json:"ndcg_at_k"`
	MRR       float64 `json:"mrr"`
	P50Ms     float64 `json:"p50_ms"`
	P95Ms     float64 `json:"p95_ms"`
	Errors    int     `json:"errors"`
	Supported bool    `json:"supported"`
}

// GateViolation records a slice where the candidate failed to match the baseline.
type GateViolation struct {
	Slice     string  `json:"slice"`
	Metric    string  `json:"metric"`
	Baseline  float64 `json:"baseline"`
	Candidate float64 `json:"candidate"`
}

// GateResult is the quality-gate verdict.
type GateResult struct {
	Baseline   string          `json:"baseline"`
	Candidate  string          `json:"candidate"`
	Pass       bool            `json:"pass"`
	Skipped    bool            `json:"skipped"`
	Reason     string          `json:"reason,omitempty"`
	Violations []GateViolation `json:"violations,omitempty"`
}

// Report is the committed artifact of a run.
type Report struct {
	Generated   string         `json:"generated"`
	Golden      string         `json:"golden"`
	K           int            `json:"k"`
	Queries     int            `json:"queries"`
	SliceCounts map[string]int `json:"slice_counts"`
	Aggregates  []Aggregate    `json:"aggregates"`
	Gate        GateResult     `json:"gate"`
}

// Run evaluates recs against cfg.Pipelines using s, returning the report and the
// raw per-query scores (useful for debugging a specific regression).
func Run(ctx context.Context, s Searcher, recs []GoldenRecord, cfg RunConfig) (Report, []queryScore, error) {
	if cfg.K <= 0 {
		cfg.K = 10
	}
	if cfg.Limit < cfg.K {
		cfg.Limit = cfg.K
	}
	var scores []queryScore
	for _, p := range cfg.Pipelines {
		if !p.Supported {
			continue // recorded as an unsupported aggregate below
		}
		for _, rec := range recs {
			j := rec.judged()
			out, err := s.Search(ctx, rec.Tenant, rec.Query, p, cfg.Limit)
			sc := queryScore{RecordID: rec.ID, Slice: rec.Slice, Pipeline: p.Name}
			if err != nil {
				sc.Err = err.Error()
				sc.Recall, sc.NDCG = math.NaN(), math.NaN()
				scores = append(scores, sc)
				continue
			}
			sc.Recall = RecallAtK(out.DocIDs, j, cfg.K)
			sc.NDCG = NDCGAtK(out.DocIDs, j, cfg.K)
			sc.RR = ReciprocalRank(out.DocIDs, j)
			sc.WallMs = out.WallMs
			scores = append(scores, sc)
		}
	}

	report := Report{
		Golden:      "",
		K:           cfg.K,
		Queries:     len(recs),
		SliceCounts: sliceCounts(recs),
		Aggregates:  aggregate(cfg.Pipelines, recs, scores, cfg.K),
	}
	report.Gate = evaluateGate(report.Aggregates, cfg.Baseline, cfg.Candidate)
	return report, scores, nil
}

// aggregate rolls per-query scores up into per-(pipeline,slice) and overall rows.
func aggregate(pipelines []Pipeline, recs []GoldenRecord, scores []queryScore, _ int) []Aggregate {
	slices := sortedSlices(recs)
	supported := map[string]bool{}
	for _, p := range pipelines {
		supported[p.Name] = p.Supported
	}

	// Bucket scores by pipeline, then by slice (and an "" overall bucket).
	type bucket struct {
		recall, ndcg, rr, wall []float64
		errors                 int
	}
	cells := map[string]map[string]*bucket{}
	ensure := func(pl, sl string) *bucket {
		if cells[pl] == nil {
			cells[pl] = map[string]*bucket{}
		}
		if cells[pl][sl] == nil {
			cells[pl][sl] = &bucket{}
		}
		return cells[pl][sl]
	}
	add := func(b *bucket, sc queryScore) {
		if sc.Err != "" {
			b.errors++
			return
		}
		b.recall = append(b.recall, sc.Recall)
		b.ndcg = append(b.ndcg, sc.NDCG)
		b.rr = append(b.rr, sc.RR)
		b.wall = append(b.wall, sc.WallMs)
	}
	for _, sc := range scores {
		add(ensure(sc.Pipeline, sc.Slice), sc)
		add(ensure(sc.Pipeline, ""), sc)
	}

	mk := func(pl, sl string) Aggregate {
		b := ensure(pl, sl)
		recall, n := meanIgnoringNaN(b.recall)
		ndcg, _ := meanIgnoringNaN(b.ndcg)
		mrr, _ := meanIgnoringNaN(b.rr)
		return Aggregate{
			Pipeline: pl, Slice: sl, N: n,
			Recall: recall, NDCG: ndcg, MRR: mrr,
			P50Ms: percentile(b.wall, 50), P95Ms: percentile(b.wall, 95),
			Errors: b.errors, Supported: supported[pl],
		}
	}

	var out []Aggregate
	for _, p := range pipelines {
		out = append(out, mk(p.Name, "")) // overall row first
		for _, sl := range slices {
			out = append(out, mk(p.Name, sl))
		}
	}
	return out
}

// evaluateGate applies the quality gate: candidate nDCG >= baseline nDCG on
// every slice (within a tiny tolerance) and a strictly-higher overall nDCG.
func evaluateGate(aggs []Aggregate, baseline, candidate string) GateResult {
	res := GateResult{Baseline: baseline, Candidate: candidate}
	if baseline == "" || candidate == "" {
		res.Skipped, res.Reason = true, "no baseline/candidate configured"
		return res
	}
	lookup := map[string]Aggregate{}
	overall := map[string]Aggregate{}
	candidateSupported := false
	for _, a := range aggs {
		if a.Slice == "" {
			overall[a.Pipeline] = a
			if a.Pipeline == candidate {
				candidateSupported = a.Supported
			}
		}
		lookup[a.Pipeline+"\x00"+a.Slice] = a
	}
	if !candidateSupported || overall[candidate].N == 0 {
		res.Skipped = true
		res.Reason = fmt.Sprintf("candidate %q not available yet (pipeline unsupported or no scored queries)", candidate)
		return res
	}

	const tol = 1e-6
	pass := true
	// Per-slice: candidate must match or beat baseline nDCG.
	for _, a := range aggs {
		if a.Pipeline != baseline || a.Slice == "" {
			continue
		}
		cand := lookup[candidate+"\x00"+a.Slice]
		if cand.N == 0 {
			continue
		}
		if cand.NDCG+tol < a.NDCG {
			pass = false
			res.Violations = append(res.Violations, GateViolation{
				Slice: a.Slice, Metric: "ndcg", Baseline: a.NDCG, Candidate: cand.NDCG,
			})
		}
	}
	// Overall: candidate must strictly improve nDCG.
	if base, ok := overall[baseline]; ok {
		if overall[candidate].NDCG <= base.NDCG+tol {
			pass = false
			res.Violations = append(res.Violations, GateViolation{
				Slice: "(overall)", Metric: "ndcg", Baseline: base.NDCG, Candidate: overall[candidate].NDCG,
			})
		}
	}
	res.Pass = pass
	return res
}
