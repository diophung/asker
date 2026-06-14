package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// testSpec is a small deterministic spec used across the generation tests.
func testSpec() Spec {
	return Spec{
		Tenants: 8,
		Seed:    42,
		DocTypeMix: NormalizeMix([]TypeWeight{
			{askerv1.DocType_EMAIL, 5},
			{askerv1.DocType_FILE, 3},
			{askerv1.DocType_IMAGE, 2},
		}),
		RareTokenRate:     0.2,
		MinDocs:           4,
		MaxDocs:           40,
		SkewLargeFraction: 0.25,
	}
}

// generateAll runs the whole corpus in-memory, threading the rare-token base
// across tenants exactly as the Runner does, and returns every doc.
func generateAll(spec Spec) []GenDoc {
	var all []GenDoc
	rareBase := 0
	for _, tp := range PlanTenants(spec) {
		docs, used := GenerateTenant(spec, tp, rareBase)
		all = append(all, docs...)
		rareBase += used
	}
	return all
}

func TestGenerateDeterministic(t *testing.T) {
	spec := testSpec()
	a := generateAll(spec)
	b := generateAll(spec)
	if len(a) != len(b) {
		t.Fatalf("doc count differs across runs: %d vs %d", len(a), len(b))
	}
	for i := range a {
		da, db := a[i].Doc, b[i].Doc
		if da.GetDocId() != db.GetDocId() ||
			da.GetTenantId() != db.GetTenantId() ||
			da.GetTitle() != db.GetTitle() ||
			da.GetBodyText() != db.GetBodyText() ||
			da.GetType() != db.GetType() ||
			da.GetVersionEtag() != db.GetVersionEtag() ||
			da.GetTs().GetCreated().GetSeconds() != db.GetTs().GetCreated().GetSeconds() ||
			a[i].RareToken != b[i].RareToken {
			t.Fatalf("doc %d differs across identical (seed,params) runs:\n a=%+v\n b=%+v", i, da, db)
		}
	}
}

func TestDifferentSeedDiffersCorpus(t *testing.T) {
	s1 := testSpec()
	s2 := testSpec()
	s2.Seed = 99
	a := generateAll(s1)
	b := generateAll(s2)
	// Doc counts and bodies should differ for at least one doc (overwhelmingly
	// likely; a fixed assertion on a specific doc keeps it deterministic).
	if fmt.Sprintf("%v", docIDs(a)) == fmt.Sprintf("%v", docIDs(b)) {
		t.Fatal("different seeds produced identical doc-id sequences")
	}
}

func docIDs(docs []GenDoc) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.Doc.GetDocId()
	}
	return out
}

func TestDocIDMatchesPlatformConvention(t *testing.T) {
	spec := testSpec()
	for _, d := range generateAll(spec) {
		want := sdk.DocID(d.Doc.GetConnectorId(), d.Doc.GetSourceNativeId())
		if d.Doc.GetDocId() != want {
			t.Fatalf("doc_id %q != sdk.DocID(%q,%q)=%q",
				d.Doc.GetDocId(), d.Doc.GetConnectorId(), d.Doc.GetSourceNativeId(), want)
		}
	}
}

func TestDocTypeMixHolds(t *testing.T) {
	// Large corpus so the empirical mix is close to the configured weights.
	spec := Spec{
		Tenants: 50,
		Seed:    7,
		DocTypeMix: NormalizeMix([]TypeWeight{
			{askerv1.DocType_EMAIL, 6},
			{askerv1.DocType_FILE, 3},
			{askerv1.DocType_IMAGE, 1},
		}),
		RareTokenRate:     0,
		MinDocs:           200,
		MaxDocs:           200, // uniform => big sample
		SkewLargeFraction: 0,
	}
	counts := map[askerv1.DocType]int{}
	total := 0
	for _, d := range generateAll(spec) {
		counts[d.Doc.GetType()]++
		total++
	}
	if total == 0 {
		t.Fatal("no docs generated")
	}
	want := map[askerv1.DocType]float64{
		askerv1.DocType_EMAIL: 0.6,
		askerv1.DocType_FILE:  0.3,
		askerv1.DocType_IMAGE: 0.1,
	}
	for dt, frac := range want {
		got := float64(counts[dt]) / float64(total)
		if got < frac-0.05 || got > frac+0.05 {
			t.Errorf("type %v: empirical fraction %.3f, want ~%.2f (+/-0.05)", dt, got, frac)
		}
	}
	// Only the three configured types must appear (never UNSPECIFIED or others).
	for dt := range counts {
		if _, ok := want[dt]; !ok {
			t.Errorf("unexpected DocType %v in corpus (not in the mix)", dt)
		}
	}
}

func TestRareTokenRateAndUniqueness(t *testing.T) {
	spec := Spec{
		Tenants:           30,
		Seed:              3,
		DocTypeMix:        NormalizeMix([]TypeWeight{{askerv1.DocType_EMAIL, 1}}),
		RareTokenRate:     0.25,
		MinDocs:           100,
		MaxDocs:           100,
		SkewLargeFraction: 0,
	}
	all := generateAll(spec)
	seen := map[string]int{}
	rareCount := 0
	for _, d := range all {
		if d.RareToken == "" {
			continue
		}
		rareCount++
		seen[d.RareToken]++
		if !strings.HasPrefix(d.RareToken, "qzx") {
			t.Errorf("rare token %q lacks qzx prefix", d.RareToken)
		}
		if !strings.Contains(d.Doc.GetBodyText(), d.RareToken) {
			t.Errorf("rare token %q not present in its doc body", d.RareToken)
		}
	}
	// Uniqueness across the whole corpus (the load suite relies on exact hits).
	for tok, n := range seen {
		if n != 1 {
			t.Errorf("rare token %q appears in %d docs, want exactly 1", tok, n)
		}
	}
	// Rate within tolerance.
	frac := float64(rareCount) / float64(len(all))
	if frac < 0.20 || frac > 0.30 {
		t.Errorf("rare-token fraction %.3f, want ~0.25 (+/-0.05)", frac)
	}
	// The rare tokens are exactly the contiguous global ordinals 0..rareCount-1.
	for g := 0; g < rareCount; g++ {
		if seen[RareToken(g)] != 1 {
			t.Errorf("expected contiguous rare token %s missing", RareToken(g))
		}
	}
}

func TestRareTokensCountMatchesReplay(t *testing.T) {
	// RareTokensInTenant must equal the rare tokens GenerateTenant actually
	// emits (the dry-run plan and the resume base both rely on this).
	spec := testSpec()
	for _, tp := range PlanTenants(spec) {
		docs, used := GenerateTenant(spec, tp, 0)
		replay := RareTokensInTenant(spec, tp)
		actual := 0
		for _, d := range docs {
			if d.RareToken != "" {
				actual++
			}
		}
		if used != actual {
			t.Fatalf("tenant %d: GenerateTenant reported %d rare used but %d docs carry one", tp.Index, used, actual)
		}
		if replay != actual {
			t.Fatalf("tenant %d: RareTokensInTenant=%d but GenerateTenant emitted %d", tp.Index, replay, actual)
		}
	}
}

func TestIsolationMarkersCorrect(t *testing.T) {
	spec := testSpec()
	byTenant := map[string]string{} // tenant id -> its marker
	for _, tp := range PlanTenants(spec) {
		byTenant[tp.TenantID] = tp.IsolationMarker
	}
	// Every doc carries exactly its own tenant's marker (in body + metadata),
	// and no doc carries another tenant's marker.
	allMarkers := map[string]bool{}
	for _, m := range byTenant {
		allMarkers[m] = true
	}
	for _, d := range generateAll(spec) {
		own := byTenant[d.Doc.GetTenantId()]
		if d.IsolationMarker != own {
			t.Fatalf("doc in tenant %s carries marker %q, want %q", d.Doc.GetTenantId(), d.IsolationMarker, own)
		}
		if !strings.Contains(d.Doc.GetBodyText(), own) {
			t.Errorf("doc in tenant %s missing own marker %q in body", d.Doc.GetTenantId(), own)
		}
		if d.Doc.GetMetadata()["isolation"] != own {
			t.Errorf("doc metadata isolation=%q, want %q", d.Doc.GetMetadata()["isolation"], own)
		}
		// No foreign marker leaks into this doc's body.
		for m := range allMarkers {
			if m != own && strings.Contains(d.Doc.GetBodyText(), m) {
				t.Errorf("doc in tenant %s leaks foreign marker %q", d.Doc.GetTenantId(), m)
			}
		}
	}
	// Markers are pairwise distinct across tenants.
	if len(allMarkers) != len(byTenant) {
		t.Errorf("isolation markers collide: %d unique for %d tenants", len(allMarkers), len(byTenant))
	}
}

func TestTenantIDsDistinctAndPrefixed(t *testing.T) {
	spec := testSpec()
	seen := map[string]bool{}
	for _, tp := range PlanTenants(spec) {
		if !strings.HasPrefix(tp.TenantID, tenantIDPrefix) {
			t.Errorf("tenant id %q lacks prefix %q", tp.TenantID, tenantIDPrefix)
		}
		if seen[tp.TenantID] {
			t.Errorf("duplicate tenant id %q", tp.TenantID)
		}
		seen[tp.TenantID] = true
	}
	if len(seen) != spec.Tenants {
		t.Errorf("got %d distinct tenant ids, want %d", len(seen), spec.Tenants)
	}
}

func TestTenantIDOverrideSeedsExactTenant(t *testing.T) {
	// The load suite seeds the EXACT tenant its OIDC token resolves to (an opaque
	// id, not synthgen-NNNNNNNN) so queries actually hit. PlanTenants must honor
	// the override and produce exactly that one group.
	const want = "a1b2c3d4-5e6f-7890-abcd-ef0123456789" // a sub-UUID-shaped id
	spec := testSpec()
	spec.Tenants = 1
	spec.TenantIDOverride = want
	plans := PlanTenants(spec)
	if len(plans) != 1 {
		t.Fatalf("override should yield 1 tenant, got %d", len(plans))
	}
	if plans[0].TenantID != want {
		t.Errorf("tenant id = %q, want override %q", plans[0].TenantID, want)
	}
	// Every generated doc must be scoped to the override group (queryability +
	// isolation depend on this).
	docs, _ := GenerateTenant(spec, plans[0], 0)
	if len(docs) == 0 {
		t.Fatal("override tenant generated zero docs")
	}
	for _, g := range docs {
		if got := g.Doc.GetTenantId(); got != want {
			t.Fatalf("doc tenant_id = %q, want %q", got, want)
		}
	}
}

func TestDocsPerTenantSkew(t *testing.T) {
	spec := Spec{
		Tenants:           500,
		Seed:              11,
		DocTypeMix:        NormalizeMix([]TypeWeight{{askerv1.DocType_EMAIL, 1}}),
		MinDocs:           1,
		MaxDocs:           1000,
		SkewLargeFraction: 0.1,
	}
	plans := PlanTenants(spec)
	mid := spec.MinDocs + (spec.MaxDocs-spec.MinDocs+1)/2
	large := 0
	for _, tp := range plans {
		if tp.DocCount < spec.MinDocs || tp.DocCount > spec.MaxDocs {
			t.Fatalf("tenant %d doc count %d out of [%d,%d]", tp.Index, tp.DocCount, spec.MinDocs, spec.MaxDocs)
		}
		if tp.DocCount >= mid {
			large++
		}
	}
	// ~10% large (drawn from the upper half). Allow a generous band.
	frac := float64(large) / float64(len(plans))
	if frac < 0.03 || frac > 0.20 {
		t.Errorf("large-tenant fraction %.3f, want ~0.10", frac)
	}
}

func TestUniformDocsPerTenant(t *testing.T) {
	spec := testSpec()
	spec.MinDocs = 17
	spec.MaxDocs = 17
	for _, tp := range PlanTenants(spec) {
		if tp.DocCount != 17 {
			t.Fatalf("uniform docs-per-tenant: tenant %d got %d, want 17", tp.Index, tp.DocCount)
		}
	}
}

func TestResumeReproducesRareTokens(t *testing.T) {
	// Generating tenants 0..N from scratch must yield the same rare tokens as
	// resuming from a checkpoint at tenant K (the base advances identically).
	spec := testSpec()
	plans := PlanTenants(spec)

	// Full run, recording rare tokens in order.
	var full []string
	rareBase := 0
	for _, tp := range plans {
		docs, used := GenerateTenant(spec, tp, rareBase)
		for _, d := range docs {
			if d.RareToken != "" {
				full = append(full, d.RareToken)
			}
		}
		rareBase += used
	}

	// Resume from tenant 3: recompute the base from tenants 0..2 via the cheap
	// replay, then continue.
	resumeBase := 0
	for i := 0; i < 3; i++ {
		resumeBase += RareTokensInTenant(spec, plans[i])
	}
	var resumed []string
	for i := 3; i < len(plans); i++ {
		docs, used := GenerateTenant(spec, plans[i], resumeBase)
		for _, d := range docs {
			if d.RareToken != "" {
				resumed = append(resumed, d.RareToken)
			}
		}
		resumeBase += used
	}

	// The resumed tail must be the suffix of the full sequence starting at the
	// first token of tenant 3.
	skip := 0
	for i := 0; i < 3; i++ {
		skip += RareTokensInTenant(spec, plans[i])
	}
	if skip > len(full) {
		t.Fatalf("skip %d exceeds full token count %d", skip, len(full))
	}
	want := full[skip:]
	if fmt.Sprintf("%v", want) != fmt.Sprintf("%v", resumed) {
		t.Fatalf("resumed rare tokens differ from full-run suffix:\n want=%v\n got =%v", want, resumed)
	}
}
