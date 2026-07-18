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

// renderText writes the console table: one block per pipeline, overall row then
// per-slice rows.
func renderText(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Asker retrieval eval — %d queries, k=%d\n", r.Queries, r.K)
	fmt.Fprintf(&b, "golden: %s   generated: %s\n", r.Golden, r.Generated)
	fmt.Fprintf(&b, "slices: %s\n\n", sliceCountsLine(r.SliceCounts))

	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PIPELINE\tSLICE\tN\tRECALL@k\tnDCG@k\tMRR\tp50ms\tp95ms")
	for _, a := range r.Aggregates {
		slice := a.Slice
		if slice == "" {
			slice = "(overall)"
		}
		name := a.Pipeline
		if !a.Supported {
			name += " (n/a)"
		}
		errs := ""
		if a.Errors > 0 {
			errs = fmt.Sprintf("  !%d err", a.Errors)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s%s\n",
			name, slice, a.N, fnum(a.Recall), fnum(a.NDCG), fnum(a.MRR), fms(a.P50Ms), fms(a.P95Ms), errs)
	}
	_ = tw.Flush()

	b.WriteString("\n")
	b.WriteString(gateLine(r.Gate))
	return b.String()
}

func sliceCountsLine(c map[string]int) string {
	parts := make([]string, 0, len(c))
	for _, s := range []string{SliceExact, SliceKeyword, SliceSemantic, SliceMultilingual} {
		if n, ok := c[s]; ok {
			parts = append(parts, fmt.Sprintf("%s=%d", s, n))
		}
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, " ")
}

func gateLine(g GateResult) string {
	switch {
	case g.Skipped:
		return fmt.Sprintf("QUALITY GATE: skipped — %s\n", g.Reason)
	case g.Pass:
		return fmt.Sprintf("QUALITY GATE: PASS — %q matches-or-beats %q on every slice and improves overall\n", g.Candidate, g.Baseline)
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "QUALITY GATE: FAIL — %q regressed vs %q:\n", g.Candidate, g.Baseline)
		for _, v := range g.Violations {
			fmt.Fprintf(&b, "  - %s %s: baseline %s > candidate %s\n", v.Slice, v.Metric, fnum(v.Baseline), fnum(v.Candidate))
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

	fmt.Fprintf(&b, "| pipeline | slice | n | Recall@%d | nDCG@%d | MRR | p50ms | p95ms |\n", r.K, r.K)
	fmt.Fprintf(&b, "|---|---|--:|--:|--:|--:|--:|--:|\n")
	for _, a := range r.Aggregates {
		slice := a.Slice
		if slice == "" {
			slice = "**overall**"
		}
		name := a.Pipeline
		if !a.Supported {
			name += " _(n/a)_"
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %s | %s | %s | %s | %s |\n",
			name, slice, a.N, fnum(a.Recall), fnum(a.NDCG), fnum(a.MRR), fms(a.P50Ms), fms(a.P95Ms))
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
