package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// Plan is the result of planning a corpus WITHOUT generating it: how many
// tenants/docs, the doc-type counts, the rare-token index bounds, and the
// estimated byte size. --dry-run prints this and generates nothing, so it is
// runnable on the 8GB dev VM even for "100K tenants, TB-scale".
type Plan struct {
	Spec          Spec
	Tenants       int
	TotalDocs     int64
	MinTenantDocs int
	MaxTenantDocs int
	// TypeCounts is the approximate per-DocType document count (expected value
	// from the mix, exact totals depend on per-doc draws). Stored as the proto
	// enum name keyed map for a readable summary.
	TypeCounts map[string]int64
	// RareTokens is the exact number of docs that carry a rare token, and the
	// inclusive global ordinal range [RareTokenFirst, RareTokenLast] those
	// tokens span (RareToken(g) for g in that range). The load suite asserts
	// exact hit counts against this index.
	RareTokens      int64
	RareTokenFirst  int
	RareTokenLast   int
	EstBytes        int64 // estimated feed bytes (Vespa JSON)
	SampleTenantID  string
	SampleDocID     string
	SampleRareToken string
}

// BuildPlan computes the corpus plan for spec. It walks every tenant plan but
// generates only a single sample document (for the human-readable preview) and
// otherwise estimates counts/bytes from the per-tenant doc counts and the mix,
// so even a 100K-tenant plan is cheap. The rare-token count is EXACT (it
// replays the rare-token RNG draws per tenant), so the load suite can rely on
// it for exact hit-count assertions.
func BuildPlan(spec Spec) Plan {
	plans := PlanTenants(spec)
	p := Plan{
		Spec:       spec,
		Tenants:    len(plans),
		TypeCounts: map[string]int64{},
	}
	if len(plans) == 0 {
		return p
	}

	// Average feed bytes per doc, sampled once from a representative document
	// (doc 0 of tenant 0). At scale, the variance washes out and an exact byte
	// count would require generating the whole corpus — defeating --dry-run.
	sampleDocs, _ := GenerateTenant(spec, plans[0], 0)
	var avgBytes int64
	if len(sampleDocs) > 0 {
		avgBytes = int64(approxFeedBytes(sampleDocs[0].Doc))
		p.SampleTenantID = sampleDocs[0].TenantID()
		p.SampleDocID = sampleDocs[0].Doc.GetDocId()
		for _, d := range sampleDocs {
			if d.RareToken != "" {
				p.SampleRareToken = d.RareToken
				break
			}
		}
	}

	p.MinTenantDocs = plans[0].DocCount
	p.MaxTenantDocs = plans[0].DocCount
	rareBase := 0
	for _, tp := range plans {
		p.TotalDocs += int64(tp.DocCount)
		if tp.DocCount < p.MinTenantDocs {
			p.MinTenantDocs = tp.DocCount
		}
		if tp.DocCount > p.MaxTenantDocs {
			p.MaxTenantDocs = tp.DocCount
		}
		rareInTenant := RareTokensInTenant(spec, tp)
		rareBase += rareInTenant
	}
	p.RareTokens = int64(rareBase)
	p.RareTokenFirst = 0
	if rareBase > 0 {
		p.RareTokenLast = rareBase - 1
	} else {
		p.RareTokenLast = -1
	}
	p.EstBytes = p.TotalDocs * avgBytes

	// Expected per-type counts from the normalized mix.
	for _, tw := range spec.DocTypeMix {
		p.TypeCounts[docTypeName(tw.Type)] = int64(float64(p.TotalDocs) * tw.Weight)
	}
	return p
}

// String renders the plan as a human-readable report for --dry-run.
func (p Plan) String() string {
	var b []byte
	w := func(format string, args ...any) { b = append(b, []byte(fmt.Sprintf(format, args...))...) }
	w("== synthgen plan (DRY RUN — nothing generated) ==\n")
	w("  seed:            %d\n", p.Spec.Seed)
	w("  tenants:         %d\n", p.Tenants)
	w("  docs/tenant:     %d..%d (skew large-fraction=%.2f)\n",
		p.MinTenantDocs, p.MaxTenantDocs, p.Spec.SkewLargeFraction)
	w("  total docs:      %d\n", p.TotalDocs)
	w("  est. feed bytes: %s\n", humanBytes(p.EstBytes))
	w("  rare-token rate: %.4f\n", p.Spec.RareTokenRate)
	if p.RareTokens > 0 {
		w("  rare tokens:     %d  (RareToken(%d)..RareToken(%d), i.e. %s..%s)\n",
			p.RareTokens, p.RareTokenFirst, p.RareTokenLast,
			RareToken(p.RareTokenFirst), RareToken(p.RareTokenLast))
	} else {
		w("  rare tokens:     0\n")
	}
	w("  doc-type mix (expected counts):\n")
	keys := make([]string, 0, len(p.TypeCounts))
	for k := range p.TypeCounts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w("    %-16s %d\n", k+":", p.TypeCounts[k])
	}
	if p.SampleDocID != "" {
		w("  sample doc:      tenant=%s doc_id=%s\n", p.SampleTenantID, p.SampleDocID)
		if p.SampleRareToken != "" {
			w("    rare token:    %s\n", p.SampleRareToken)
		}
	}
	w("  isolation:       every tenant's docs carry IsolationMarker(seed, idx);\n")
	w("                   marker(0)=%s marker(%d)=%s\n",
		IsolationMarker(p.Spec.Seed, 0), p.Tenants-1, IsolationMarker(p.Spec.Seed, p.Tenants-1))
	return string(b)
}

// humanBytes renders a byte count as a human-readable string (KiB/MiB/...).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB (%d bytes)", float64(n)/float64(div), "KMGTPE"[exp], n)
}

// --- Checkpoint / resume -----------------------------------------------------

// Checkpoint records progress so a TB-scale run can restart at a tenant
// boundary. It stores the last fully-completed tenant index and the global
// rare-token base reached at that point, so resuming reproduces the SAME
// rare-token ordinals (the index is append-only across tenants). The Spec is
// stored too, and a resume that disagrees with the saved Spec is rejected (the
// corpus would diverge).
type Checkpoint struct {
	Spec          Spec  `json:"spec"`
	LastTenant    int   `json:"last_tenant"` // -1 before any tenant completes
	RareBase      int   `json:"rare_base"`   // global rare-token ordinal after LastTenant
	DocsFed       int64 `json:"docs_fed"`    // cumulative, for the summary
	BytesFed      int64 `json:"bytes_fed"`   // cumulative (approx)
	UpdatedUnixMs int64 `json:"updated_unix_ms"`
}

// loadCheckpoint reads a checkpoint file; missing file => fresh run (LastTenant
// = -1). A corrupt file is a hard error (better than silently re-feeding).
func loadCheckpoint(path string) (Checkpoint, error) {
	fresh := Checkpoint{LastTenant: -1}
	if path == "" {
		return fresh, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied checkpoint path
	if err != nil {
		if os.IsNotExist(err) {
			return fresh, nil
		}
		return fresh, fmt.Errorf("synthgen: read checkpoint %s: %w", path, err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return fresh, fmt.Errorf("synthgen: parse checkpoint %s: %w", path, err)
	}
	return cp, nil
}

// saveCheckpoint writes the checkpoint atomically (temp file + rename) so a
// crash mid-write never leaves a corrupt checkpoint.
func saveCheckpoint(path string, cp Checkpoint) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return fmt.Errorf("synthgen: marshal checkpoint: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("synthgen: write checkpoint: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("synthgen: rename checkpoint: %w", err)
	}
	return nil
}

// specsEqual reports whether two specs would generate the identical corpus.
// Used to reject a --resume against a checkpoint built with different params.
func specsEqual(a, b Spec) bool {
	if a.Tenants != b.Tenants || a.Seed != b.Seed ||
		a.RareTokenRate != b.RareTokenRate || a.MinDocs != b.MinDocs ||
		a.MaxDocs != b.MaxDocs || a.SkewLargeFraction != b.SkewLargeFraction {
		return false
	}
	if len(a.DocTypeMix) != len(b.DocTypeMix) {
		return false
	}
	am := NormalizeMix(a.DocTypeMix)
	bm := NormalizeMix(b.DocTypeMix)
	for i := range am {
		if am[i].Type != bm[i].Type || am[i].Weight != bm[i].Weight {
			return false
		}
	}
	return true
}
