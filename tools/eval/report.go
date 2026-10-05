package main

// Rendering the run into (a) a console table for the operator and (b) a
// committed markdown + JSON report, so quality changes are reviewable in a PR
// over time (the prompt asks for the reports to be committed).

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// fnum formats a metric, rendering NaN as "-" so empty slices read cleanly.
func fnum(v float64) string {
	if math.IsNaN(v) {
		return "-"
	}
	return fmt.Sprintf("%.3f", v)
}

func fms(v float64) string {
	if math.IsNaN(v) {
		return "-"
	}
	return fmt.Sprintf("%.0f", v)
}

func headRecall(a Aggregate) string {
	if a.PreRerankRecall == nil {
		return "unmeasured"
	}
	return fmt.Sprintf("%s (%d/%d)", fnum(*a.PreRerankRecall), a.PreRerankN, a.QualityN)
}

// renderText writes the console table: one block per pipeline, overall row then
// per-slice rows.
func renderText(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Asker retrieval eval — %d queries, k=%d\n", r.Queries, r.K)
	fmt.Fprintf(&b, "golden: %s   generated: %s\n", r.Golden, r.Generated)
	fmt.Fprintf(&b, "slices: %s\n\n", sliceCountsLine(r.SliceCounts))

	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PIPELINE\tSLICE\tTASKS\tRECALL@k\tnDCG@k\tMRR@k\tSUCCESS@k\tALL NEEDS@k\tp50ms\tp95ms\tp99ms\tERR\tCACHE\tDEGRADED\tRERANK APPLIED/REQUESTED\tPRE-RERANK HEAD RECALL (n)")
	for _, a := range r.Aggregates {
		slice := a.Slice
		if slice == "" {
			slice = "(overall)"
		}
		name := a.Pipeline
		if !a.Supported {
			name += " (n/a)"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d/%d\t%s\n",
			name, slice, a.N, a.Expected, fnum(a.Recall), fnum(a.NDCG), fnum(a.MRR), fnum(a.TaskSuccess), fnum(a.NeedCoverage), fms(a.P50Ms), fms(a.P95Ms), fms(a.P99Ms), a.Errors, a.CacheHits, a.Degraded, a.RerankApplied, a.RerankRequested, headRecall(a))
	}
	_ = tw.Flush()

	b.WriteString("\n")
	b.WriteString(gateLine(r.Gate))
	return b.String()
}

func sliceCountsLine(c map[string]int) string {
	parts := make([]string, 0, len(c))
	labels := make([]string, 0, len(c))
	for label := range c {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		parts = append(parts, fmt.Sprintf("%s=%d", label, c[label]))
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, " ")
}

func gateLine(g GateResult) string {
	switch {
	case g.Skipped:
		return fmt.Sprintf("QUALIFICATION GATE: skipped (unqualified) — %s\n", g.Reason)
	case g.Pass:
		return fmt.Sprintf("QUALIFICATION GATE: PASS — %q satisfies the recorded correctness, quality and latency criteria against %q\n", g.Candidate, g.Baseline)
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "QUALIFICATION GATE: FAIL — %q vs %q:\n", g.Candidate, g.Baseline)
		for _, v := range g.Violations {
			fmt.Fprintf(&b, "  - %s %s: threshold/reference %s; observed %s\n", v.Slice, v.Metric, fnum(v.Baseline), fnum(v.Candidate))
		}
		return b.String()
	}
}

// renderMarkdown produces the committed report body.
func renderMarkdown(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Asker retrieval eval report\n\n")
	fmt.Fprintf(&b, "- generated: `%s`\n- golden: `%s`\n- queries: %d (k=%d)\n- slices: %s\n\n",
		r.Generated, r.Golden, r.Queries, r.K, sliceCountsLine(r.SliceCounts))

	if data, err := json.MarshalIndent(r.Config, "", "  "); err == nil {
		fmt.Fprintf(&b, "Recorded acceptance configuration:\n\n```json\n%s\n```\n\n", data)
	}
	fmt.Fprintf(&b, "Golden SHA-256: `%s`\n\n", r.GoldenSHA256)
	keys := make([]string, 0, len(r.Metadata))
	for key := range r.Metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&b, "- %s: %s\n", key, r.Metadata[key])
	}
	fmt.Fprintf(&b, "\nLatency covers the entire HTTP response and decode, including failed requests. It excludes browser rendering and token acquisition. No-match tasks are excluded from ranked-relevance means; failed positive tasks score zero. Repeated samples do not increase the number of independently judged queries. A small synthetic corpus does not establish Google-level quality or large-corpus performance.\n\n")
	fmt.Fprintf(&b, "Pre-rerank head recall uses complete authorized head IDs from debug telemetry, when available, and excludes no-match tasks. It does not measure recall over the full eligible retrieval population. Absent, truncated or malformed telemetry is unmeasured; partial sample coverage is shown explicitly. No candidate IDs or query text are retained in reports.\n\n")
	fmt.Fprintf(&b, "| pipeline | slice | attempted/expected | quality n | Recall@%d | nDCG@%d | MRR@%d | task success@%d | exact@1 | all-needs coverage (n) | p50 ms | p95 ms | p99 ms | errors | cache | degraded | rerank applied/requested | unfulfilled/unknown | constraint violations |\n", r.K, r.K, r.K, r.K)
	fmt.Fprintf(&b, "|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|\n")
	for _, a := range r.Aggregates {
		slice := a.Slice
		if slice == "" {
			slice = "**overall**"
		}
		name := a.Pipeline
		if !a.Supported {
			name += " _(n/a)_"
		}
		fmt.Fprintf(&b, "| %s | %s | %d/%d | %d | %s | %s | %s | %s | %s | %s (%d) | %s | %s | %s | %d | %d | %d | %d/%d | %d/%d | %d |\n",
			name, slice, a.N, a.Expected, a.QualityN, fnum(a.Recall), fnum(a.NDCG), fnum(a.MRR), fnum(a.TaskSuccess), fnum(a.ExactSuccess), fnum(a.NeedCoverage), a.NeedN, fms(a.P50Ms), fms(a.P95Ms), fms(a.P99Ms), a.Errors, a.CacheHits, a.Degraded, a.RerankApplied, a.RerankRequested, a.RerankUnfulfilled, a.ExecutionMissing, a.ConstraintViolations)
	}
	fmt.Fprintf(&b, "\n| pipeline | slice | pre-rerank head recall | measured / positive samples | unmeasured positive samples |\n|---|---|--:|--:|--:|\n")
	for _, a := range r.Aggregates {
		slice := a.Slice
		if slice == "" {
			slice = "overall"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %d/%d | %d |\n", a.Pipeline, slice, headRecall(a), a.PreRerankN, a.QualityN, a.PreRerankUnknown)
	}
	fmt.Fprintf(&b, "\n**%s**\n", strings.TrimSpace(gateLine(r.Gate)))
	return b.String()
}

// writeReports writes the JSON + markdown report into dir, both as a
// timestamped file and as latest.{json,md}. Returns the timestamped json path.
func writeReports(dir string, r Report, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("eval: mkdir %s: %w", dir, err)
	}
	stamp := now.UTC().Format("20060102-150405")
	jsonPath := filepath.Join(dir, "report-"+stamp+".json")

	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", fmt.Errorf("eval: marshal report: %w", err)
	}
	md := renderMarkdown(r)
	writes := map[string][]byte{
		jsonPath: data,
		filepath.Join(dir, "report-"+stamp+".md"): []byte(md),
		filepath.Join(dir, "latest.json"):         data,
		filepath.Join(dir, "latest.md"):           []byte(md),
	}
	for p, d := range writes {
		if err := os.WriteFile(p, d, 0o600); err != nil {
			return "", fmt.Errorf("eval: write %s: %w", p, err)
		}
	}
	return jsonPath, nil
}
