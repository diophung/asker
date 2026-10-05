package main

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"sync"
	"time"
)

// RunConfig is recorded with the report so acceptance criteria cannot change
// invisibly between runs. Repeated queries are separate latency samples, not
// additional independently judged tasks.
type RunConfig struct {
	K                 int        `json:"k"`
	Limit             int        `json:"limit"`
	Pipelines         []Pipeline `json:"pipelines"`
	Baseline          string     `json:"baseline"`
	Candidate         string     `json:"candidate"`
	Repeats           int        `json:"repeats"`
	Concurrency       int        `json:"concurrency"`
	MaxP95Ms          float64    `json:"max_p95_ms"`
	MinLatencySamples int        `json:"min_latency_samples"`
	MinTaskSuccess    float64    `json:"min_task_success"`
	MinExactSuccess   float64    `json:"min_exact_success"`
	MinNeedCoverage   float64    `json:"min_need_coverage"`
	RequireUncached   bool       `json:"require_uncached"`
	AllowEqual        bool       `json:"allow_equal"`
}

// A measurement records every requested task, including failed and degraded
// requests. Query text and bearer tokens are intentionally absent.
type queryScore struct {
	RecordID             string   `json:"record_id"`
	Slice                string   `json:"slice"`
	Pipeline             string   `json:"pipeline"`
	Repeat               int      `json:"repeat"`
	Recall               float64  `json:"recall_at_k"`
	NDCG                 float64  `json:"ndcg_at_k"`
	RR                   float64  `json:"reciprocal_rank"`
	QualityEligible      bool     `json:"quality_eligible"`
	ExactSuccess         bool     `json:"exact_success"`
	NeedEligible         bool     `json:"need_eligible"`
	AllNeedsCovered      bool     `json:"all_needs_covered"`
	TaskSuccess          bool     `json:"task_success"`
	ConstraintViolations int      `json:"constraint_violations"`
	WallMs               float64  `json:"wall_ms"`
	ServerMs             int64    `json:"server_ms"`
	Cached               bool     `json:"cached"`
	Degraded             string   `json:"degraded,omitempty"`
	RerankRequested      bool     `json:"rerank_requested"`
	RerankApplied        bool     `json:"rerank_applied"`
	RerankUnfulfilled    bool     `json:"rerank_unfulfilled"`
	ExecutionMissing     bool     `json:"execution_missing"`
	Err                  string   `json:"error,omitempty"`
	PreRerankRecall      *float64 `json:"pre_rerank_head_recall"`
	PreRerankCount       int      `json:"pre_rerank_population_count"`
	PreRerankDepth       int      `json:"pre_rerank_head_depth"`
}

// N is attempted tasks, QualityN is the ranked-relevance denominator and
// Scored is successfully decoded responses. Errors contribute zero to quality
// and task success; no-match tasks contribute only to task success.
type Aggregate struct {
	Pipeline             string   `json:"pipeline"`
	Slice                string   `json:"slice"`
	N                    int      `json:"n"`
	Expected             int      `json:"expected"`
	Scored               int      `json:"scored"`
	QualityN             int      `json:"quality_n"`
	Recall               float64  `json:"recall_at_k"`
	NDCG                 float64  `json:"ndcg_at_k"`
	MRR                  float64  `json:"mrr"`
	TaskSuccess          float64  `json:"task_success_at_k"`
	ExactN               int      `json:"exact_n"`
	ExactSuccess         float64  `json:"exact_success_at_1"`
	NeedN                int      `json:"multi_need_n"`
	NeedCoverage         float64  `json:"all_needs_coverage_at_k"`
	P50Ms                float64  `json:"p50_ms"`
	P95Ms                float64  `json:"p95_ms"`
	P99Ms                float64  `json:"p99_ms"`
	Errors               int      `json:"errors"`
	CacheHits            int      `json:"cache_hits"`
	Degraded             int      `json:"degraded"`
	RerankRequested      int      `json:"rerank_requested"`
	RerankApplied        int      `json:"rerank_applied"`
	RerankUnfulfilled    int      `json:"rerank_unfulfilled"`
	ExecutionMissing     int      `json:"execution_missing"`
	ConstraintViolations int      `json:"constraint_violations"`
	Supported            bool     `json:"supported"`
	PreRerankRecall      *float64 `json:"pre_rerank_head_recall"`
	PreRerankN           int      `json:"pre_rerank_head_n"`
	PreRerankUnknown     int      `json:"pre_rerank_head_unmeasured"`
}

type GateViolation struct {
	Slice     string  `json:"slice"`
	Metric    string  `json:"metric"`
	Baseline  float64 `json:"baseline"`
	Candidate float64 `json:"candidate"`
}

type GateResult struct {
	Baseline   string          `json:"baseline"`
	Candidate  string          `json:"candidate"`
	Pass       bool            `json:"pass"`
	Skipped    bool            `json:"skipped"`
	Reason     string          `json:"reason,omitempty"`
	Violations []GateViolation `json:"violations,omitempty"`
}

type Report struct {
	Generated    string            `json:"generated"`
	Golden       string            `json:"golden"`
	GoldenSHA256 string            `json:"golden_sha256"`
	Metadata     map[string]string `json:"metadata"`
	Config       RunConfig         `json:"config"`
	K            int               `json:"k"`
	Queries      int               `json:"queries"`
	SliceCounts  map[string]int    `json:"slice_counts"`
	Aggregates   []Aggregate       `json:"aggregates"`
	Measurements []queryScore      `json:"measurements"`
	Gate         GateResult        `json:"gate"`
}

func Run(ctx context.Context, s Searcher, recs []GoldenRecord, cfg RunConfig) (Report, []queryScore, error) {
	if len(recs) == 0 {
		return Report{}, nil, fmt.Errorf("eval: no records")
	}
	if cfg.K <= 0 {
		cfg.K = 10
	}
	if cfg.Limit < cfg.K {
		cfg.Limit = cfg.K
	}
	if cfg.Repeats <= 0 {
		cfg.Repeats = 1
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	seen := map[string]bool{}
	type job struct {
		p      Pipeline
		rec    GoldenRecord
		repeat int
	}
	var jobs []job
	for _, p := range cfg.Pipelines {
		if p.Name == "" || seen[p.Name] {
			return Report{}, nil, fmt.Errorf("eval: empty or duplicate pipeline %q", p.Name)
		}
		seen[p.Name] = true
		if !p.Supported {
			continue
		}
		for repetition := 0; repetition < cfg.Repeats; repetition++ {
			for _, rec := range recs {
				jobs = append(jobs, job{p, rec, repetition + 1})
			}
		}
	}
	scores := make([]queryScore, len(jobs))
	indices := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < cfg.Concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range indices {
				j := jobs[i]
				p := j.p
				p.Extra = cloneValues(p.Extra)
				for key, value := range j.rec.Filters {
					p.Extra.Set(key, value)
				}
				start := time.Now()
				out, err := s.Search(ctx, j.rec.Tenant, j.rec.Query, p, cfg.Limit)
				sc := scoreOutcome(j.rec, p, out, cfg.K)
				sc.Repeat = j.repeat
				if err != nil {
					sc.Err = err.Error()
					sc.PreRerankRecall = nil
					sc.Recall, sc.NDCG, sc.RR, sc.TaskSuccess, sc.ExactSuccess, sc.AllNeedsCovered = 0, 0, 0, false, false, false
					if sc.WallMs <= 0 {
						sc.WallMs = float64(time.Since(start).Microseconds()) / 1000
					}
				}
				scores[i] = sc
			}
		}()
	}
	for i := range jobs {
		indices <- i
	}
	close(indices)
	wg.Wait()
	report := Report{K: cfg.K, Queries: len(recs), SliceCounts: sliceCounts(recs), Config: cfg, Measurements: scores}
	report.Aggregates = aggregate(cfg.Pipelines, recs, scores, cfg.Repeats)
	report.Gate = evaluateGate(report.Aggregates, cfg)
	return report, scores, nil
}

func cloneValues(in url.Values) url.Values {
	out := url.Values{}
	for key, values := range in {
		out[key] = append([]string(nil), values...)
	}
	return out
}

func scoreOutcome(rec GoldenRecord, p Pipeline, out SearchOutcome, k int) queryScore {
	sc := queryScore{RecordID: rec.ID, Slice: rec.Slice, Pipeline: p.Name, QualityEligible: !rec.NoMatch,
		WallMs: out.WallMs, ServerMs: out.ServerMs, Cached: out.Cached, Degraded: out.Degraded, NeedEligible: len(rec.Required) > 0}
	judged := rec.judged()
	ids := dedupeStable(out.DocIDs)
	top := ids
	if len(top) > k {
		top = top[:k]
	}
	if !rec.NoMatch {
		if head := out.PreRerankHead; head != nil {
			recall := RecallAtK(head.DocIDs, judged, 0)
			sc.PreRerankRecall = &recall
			sc.PreRerankCount, sc.PreRerankDepth = head.Count, head.Depth
		}
		sc.Recall, sc.NDCG, sc.RR = RecallAtK(ids, judged, k), NDCGAtK(ids, judged, k), ReciprocalRank(top, judged)
		sc.TaskSuccess = sc.Recall > 0
		sc.ExactSuccess = len(ids) > 0 && judged.gainOf(ids[0]) > 0
		for _, alternatives := range rec.Required {
			found := false
			for _, id := range top {
				for _, needID := range alternatives {
					if id == needID {
						found = true
					}
				}
			}
			sc.TaskSuccess = sc.TaskSuccess && found
		}
	} else {
		sc.TaskSuccess = len(ids) == 0
	}
	for _, id := range ids {
		for _, forbidden := range rec.Forbidden {
			if id == forbidden {
				sc.ConstraintViolations++
			}
		}
	}
	if sc.ConstraintViolations > 0 {
		sc.TaskSuccess = false
	}
	sc.AllNeedsCovered = sc.NeedEligible && sc.TaskSuccess
	sc.RerankRequested = p.Extra.Get("rerank") == "1" || p.Extra.Get("rerank") == "true"
	sc.RerankApplied = out.RerankApplied
	sc.ExecutionMissing = sc.RerankRequested && (!out.ExecutionKnown || !out.RerankRequested)
	// An empty result needs no cross-encoder work. Nonempty requested results
	// must prove execution rather than silently aliasing the hybrid baseline.
	sc.RerankUnfulfilled = sc.RerankRequested && len(ids) > 0 && !out.RerankApplied
	return sc
}

func aggregate(pipelines []Pipeline, recs []GoldenRecord, scores []queryScore, repeats int) []Aggregate {
	slices := sortedSlices(recs)
	counts := sliceCounts(recs)
	var out []Aggregate
	for _, p := range pipelines {
		for _, slice := range append([]string{""}, slices...) {
			expected := len(recs) * repeats
			if slice != "" {
				expected = counts[slice] * repeats
			}
			a := Aggregate{Pipeline: p.Name, Slice: slice, Expected: expected, Supported: p.Supported}
			var headRecallSum float64
			var wall []float64
			for _, sc := range scores {
				if sc.Pipeline != p.Name || (slice != "" && sc.Slice != slice) {
					continue
				}
				a.N++
				wall = append(wall, sc.WallMs)
				if sc.Err != "" {
					a.Errors++
				} else {
					a.Scored++
				}
				if sc.QualityEligible {
					a.QualityN++
					if sc.PreRerankRecall != nil {
						a.PreRerankN++
						headRecallSum += *sc.PreRerankRecall
					} else {
						a.PreRerankUnknown++
					}
					a.Recall += sc.Recall
					a.NDCG += sc.NDCG
					a.MRR += sc.RR
				}
				if sc.TaskSuccess {
					a.TaskSuccess++
				}
				if sc.Slice == SliceExact {
					a.ExactN++
					if sc.ExactSuccess {
						a.ExactSuccess++
					}
				}
				if sc.NeedEligible {
					a.NeedN++
					if sc.AllNeedsCovered {
						a.NeedCoverage++
					}
				}
				if sc.Cached {
					a.CacheHits++
				}
				if sc.Degraded != "" {
					a.Degraded++
				}
				if sc.RerankRequested {
					a.RerankRequested++
				}
				if sc.RerankApplied {
					a.RerankApplied++
				}
				if sc.RerankUnfulfilled {
					a.RerankUnfulfilled++
				}
				if sc.ExecutionMissing {
					a.ExecutionMissing++
				}
				a.ConstraintViolations += sc.ConstraintViolations
			}
			if a.QualityN > 0 {
				den := float64(a.QualityN)
				a.Recall /= den
				a.NDCG /= den
				a.MRR /= den
			}
			if a.PreRerankN > 0 {
				mean := headRecallSum / float64(a.PreRerankN)
				a.PreRerankRecall = &mean
			}
			if a.N > 0 {
				a.TaskSuccess /= float64(a.N)
			}
			if a.ExactN > 0 {
				a.ExactSuccess /= float64(a.ExactN)
			}
			if a.NeedN > 0 {
				a.NeedCoverage /= float64(a.NeedN)
			}
			a.P50Ms, a.P95Ms, a.P99Ms = finiteOrZero(percentile(wall, 50)), finiteOrZero(percentile(wall, 95)), finiteOrZero(percentile(wall, 99))
			out = append(out, a)
		}
	}
	return out
}

func finiteOrZero(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	return x
}

// Gates fail closed on unavailable pipelines, incomplete slices, errors,
// degraded execution, unverified requested reranking, and latency violations.
func evaluateGate(aggs []Aggregate, cfg RunConfig) GateResult {
	res := GateResult{Baseline: cfg.Baseline, Candidate: cfg.Candidate}
	if cfg.Baseline == "" || cfg.Candidate == "" {
		res.Skipped, res.Reason = true, "no baseline/candidate configured; this is not a qualified result"
		return res
	}
	lookup := map[string]Aggregate{}
	for _, a := range aggs {
		lookup[a.Pipeline+"\x00"+a.Slice] = a
	}
	violate := func(slice, metric string, expected, actual float64) {
		res.Violations = append(res.Violations, GateViolation{slice, metric, expected, actual})
	}
	for _, name := range []string{cfg.Baseline, cfg.Candidate} {
		a, ok := lookup[name+"\x00"]
		if !ok || !a.Supported {
			violate("(overall)", name+".supported", 1, 0)
			continue
		}
		if a.N != a.Expected || a.N == 0 {
			violate("(overall)", name+".completed", float64(a.Expected), float64(a.N))
		}
		for _, check := range []struct {
			metric string
			count  int
		}{{"errors", a.Errors}, {"degraded", a.Degraded}, {"constraint_violations", a.ConstraintViolations}, {"rerank_unfulfilled", a.RerankUnfulfilled}, {"execution_missing", a.ExecutionMissing}} {
			if check.count > 0 {
				violate("(overall)", name+"."+check.metric, 0, float64(check.count))
			}
		}
		if cfg.RequireUncached && a.CacheHits > 0 {
			violate("(overall)", name+".cache_hits", 0, float64(a.CacheHits))
		}
	}
	const tol = 1e-6
	for _, base := range aggs {
		if base.Pipeline != cfg.Baseline || base.Slice == "" {
			continue
		}
		cand, ok := lookup[cfg.Candidate+"\x00"+base.Slice]
		if !ok || cand.N != base.Expected || cand.Errors > 0 || !cand.Supported {
			violate(base.Slice, "completed", float64(base.Expected), float64(cand.Scored))
			continue
		}
		if base.QualityN > 0 && cand.NDCG+tol < base.NDCG {
			violate(base.Slice, "ndcg", base.NDCG, cand.NDCG)
		}
		if cand.TaskSuccess+tol < base.TaskSuccess {
			violate(base.Slice, "task_success", base.TaskSuccess, cand.TaskSuccess)
		}
	}
	base, cand := lookup[cfg.Baseline+"\x00"], lookup[cfg.Candidate+"\x00"]
	if cand.NDCG+tol < base.NDCG || (!cfg.AllowEqual && cand.NDCG <= base.NDCG+tol) {
		violate("(overall)", "ndcg", base.NDCG, cand.NDCG)
	}
	if cfg.MaxP95Ms > 0 && cand.P95Ms > cfg.MaxP95Ms {
		violate("(overall)", "p95_ms_max", cfg.MaxP95Ms, cand.P95Ms)
	}
	if cfg.MinLatencySamples > 0 && cand.N < cfg.MinLatencySamples {
		violate("(overall)", "latency_samples_min", float64(cfg.MinLatencySamples), float64(cand.N))
	}
	if cfg.MinTaskSuccess > 0 && cand.TaskSuccess+tol < cfg.MinTaskSuccess {
		violate("(overall)", "task_success_min", cfg.MinTaskSuccess, cand.TaskSuccess)
	}
	if cfg.MinExactSuccess > 0 && cand.ExactN == 0 {
		violate(SliceExact, "judged_exact_samples_min", 1, 0)
	}
	if cfg.MinExactSuccess > 0 && cand.ExactN > 0 && cand.ExactSuccess+tol < cfg.MinExactSuccess {
		violate(SliceExact, "success_at_1_min", cfg.MinExactSuccess, cand.ExactSuccess)
	}
	if cfg.MinNeedCoverage > 0 && cand.NeedN == 0 {
		violate(SliceMultiNeed, "judged_multi_need_samples_min", 1, 0)
	}
	if cfg.MinNeedCoverage > 0 && cand.NeedN > 0 && cand.NeedCoverage+tol < cfg.MinNeedCoverage {
		violate(SliceMultiNeed, "all_needs_coverage_min", cfg.MinNeedCoverage, cand.NeedCoverage)
	}
	res.Pass = len(res.Violations) == 0
	return res
}
