package main

// Golden set: the labeled (query -> relevant doc_ids) pairs the harness scores
// pipelines against. Stored as JSONL (one record per line) so records are
// diffable, appendable, and reviewable in a PR. Blank lines and lines whose
// first non-space rune is '#' are ignored, so a golden file can be commented.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
)

// Slice labels partition the golden set so the harness can report per-slice
// metrics — the quality gate is enforced on every slice, because naive vector
// search regresses hardest on exact-match while improving the overall average.
const (
	SliceExact        = "exact"        // filenames, identifiers, error codes, rare tokens
	SliceKeyword      = "keyword"      // ordinary lexical queries
	SliceSemantic     = "semantic"     // paraphrase / conceptual queries
	SliceMultilingual = "multilingual" // non-English or cross-lingual queries
	SliceFilter       = "filter"
	SliceNegation     = "negation"
	SliceNoMatch      = "no_match"
	SliceTypo         = "typo"
	SliceMultiNeed    = "multi_need"
)

var knownSlices = map[string]bool{
	SliceExact: true, SliceKeyword: true, SliceSemantic: true, SliceMultilingual: true,
	SliceFilter: true, SliceNegation: true, SliceNoMatch: true, SliceTypo: true, SliceMultiNeed: true,
}

// GoldenRecord is one labeled query. Relevance is binary via Relevant, or
// graded via Gains (doc_id -> gain); positive tasks require a relevant doc,
// while an explicit NoMatch task expects an empty result.
// When both are set, Gains wins and Relevant is treated as gain-1 fallbacks for
// any id not in Gains.
type GoldenRecord struct {
	ID        string             `json:"id"`                  // stable identifier for the query (for diffs/reports)
	Query     string             `json:"query"`               // the search text, verbatim
	Tenant    string             `json:"tenant"`              // whose corpus to search (resolves to a bearer token)
	Slice     string             `json:"slice"`               // one of the Slice* constants
	Relevant  []string           `json:"relevant"`            // expected relevant doc_ids (binary relevance)
	Gains     map[string]float64 `json:"gains,omitempty"`     // optional graded relevance, doc_id -> gain
	ModeHint  string             `json:"mode_hint,omitempty"` // informational: the mode this query is meant to exercise
	Note      string             `json:"note,omitempty"`      // free-form provenance / curation note
	Split     string             `json:"split,omitempty"`     // dev, regression, or frozen holdout
	NoMatch   bool               `json:"no_match,omitempty"`
	Forbidden []string           `json:"forbidden,omitempty"` // explicit constraint violations, checked across every returned hit
	Required  [][]string         `json:"required,omitempty"`  // each need must have at least one alternative in top-k
	Filters   map[string]string  `json:"filters,omitempty"`   // explicit supported REST filters
}

// judged builds the relevance map used by the metrics.
func (r GoldenRecord) judged() Judged {
	j := make(Judged, len(r.Relevant)+len(r.Gains))
	for _, id := range r.Relevant {
		if id != "" {
			j[id] = 1
		}
	}
	for id, g := range r.Gains {
		if id != "" {
			j[id] = g // graded gain overrides the binary fallback
		}
	}
	return j
}

// validate reports the first problem with a record, or nil.
func (r GoldenRecord) validate() error {
	if strings.TrimSpace(r.Query) == "" {
		return fmt.Errorf("empty query")
	}
	if r.Tenant == "" {
		return fmt.Errorf("empty tenant")
	}
	if !knownSlices[r.Slice] {
		return fmt.Errorf("unknown slice %q", r.Slice)
	}
	if r.Split != "" && r.Split != "dev" && r.Split != "regression" && r.Split != "holdout" {
		return fmt.Errorf("invalid split %q", r.Split)
	}
	for key := range r.Filters {
		if key != "types" && key != "participant" && key != "from" && key != "to" {
			return fmt.Errorf("unsupported filter %q", key)
		}
	}
	for id, gain := range r.Gains {
		if id == "" || math.IsNaN(gain) || math.IsInf(gain, 0) || gain < 0 {
			return fmt.Errorf("invalid relevance gain for %q", id)
		}
	}
	if r.NoMatch && (r.judged().totalRelevant() > 0 || len(r.Required) > 0) {
		return fmt.Errorf("no_match cannot also require relevant documents")
	}
	if r.Slice == SliceMultiNeed && len(r.Required) < 2 {
		return fmt.Errorf("multi_need slice requires at least two labeled need groups")
	}
	for _, alternatives := range r.Required {
		if len(alternatives) == 0 {
			return fmt.Errorf("empty required need")
		}
		for _, id := range alternatives {
			if r.judged().gainOf(id) <= 0 {
				return fmt.Errorf("required doc %q must have positive relevance", id)
			}
		}
	}
	for _, id := range r.Forbidden {
		if id == "" || r.judged().gainOf(id) > 0 {
			return fmt.Errorf("invalid forbidden doc %q", id)
		}
	}
	if !r.NoMatch && r.judged().totalRelevant() == 0 {
		return fmt.Errorf("no relevant docs (set \"relevant\" or \"gains\")")
	}
	return nil
}

// LoadGolden reads and validates a JSONL golden file. Every record must be
// valid; an error names the offending line so a bad golden set fails loudly
// rather than silently scoring against fewer queries than intended.
func LoadGolden(path string) ([]GoldenRecord, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied golden path
	if err != nil {
		return nil, fmt.Errorf("eval: open golden %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return parseGolden(f)
}

func parseGolden(r io.Reader) ([]GoldenRecord, error) {
	var out []GoldenRecord
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // tolerate long lines (many relevant ids)
	line := 0
	ids := map[string]bool{}
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var rec GoldenRecord
		dec := json.NewDecoder(strings.NewReader(text))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rec); err != nil {
			return nil, fmt.Errorf("eval: golden line %d: %w", line, err)
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			return nil, fmt.Errorf("eval: golden line %d: expected exactly one JSON record", line)
		}
		if err := rec.validate(); err != nil {
			return nil, fmt.Errorf("eval: golden line %d (%s): %w", line, rec.ID, err)
		}
		if rec.ID == "" {
			rec.ID = fmt.Sprintf("q%03d", line)
		}
		if ids[rec.ID] {
			return nil, fmt.Errorf("eval: golden line %d: duplicate record id %q", line, rec.ID)
		}
		ids[rec.ID] = true
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("eval: read golden: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("eval: golden set is empty")
	}
	return out, nil
}

// sliceCounts summarizes how many records fall in each slice, for the report
// header and to warn about thin slices.
func sliceCounts(recs []GoldenRecord) map[string]int {
	c := map[string]int{}
	for _, r := range recs {
		c[r.Slice]++
	}
	return c
}

// sortedSlices returns the slice labels present in recs, in a stable order.
func sortedSlices(recs []GoldenRecord) []string {
	seen := map[string]bool{}
	for _, r := range recs {
		seen[r.Slice] = true
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
