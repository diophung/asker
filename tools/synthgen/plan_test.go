package main

import (
	"path/filepath"
	"strings"
	"testing"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

func TestBuildPlanCountsExact(t *testing.T) {
	spec := testSpec()
	plan := BuildPlan(spec)

	// Total docs equals the sum of per-tenant counts.
	var wantTotal int64
	for _, tp := range PlanTenants(spec) {
		wantTotal += int64(tp.DocCount)
	}
	if plan.TotalDocs != wantTotal {
		t.Errorf("plan total docs %d, want %d", plan.TotalDocs, wantTotal)
	}

	// Rare-token count is exact: equal to what generating the whole corpus
	// actually emits.
	wantRare := 0
	for _, d := range generateAll(spec) {
		if d.RareToken != "" {
			wantRare++
		}
	}
	if plan.RareTokens != int64(wantRare) {
		t.Errorf("plan rare tokens %d, want exact %d", plan.RareTokens, wantRare)
	}
	if wantRare > 0 && plan.RareTokenLast != wantRare-1 {
		t.Errorf("plan rare token last ordinal %d, want %d", plan.RareTokenLast, wantRare-1)
	}
	if plan.EstBytes <= 0 {
		t.Error("plan estimated bytes should be positive")
	}
	if plan.Tenants != spec.Tenants {
		t.Errorf("plan tenants %d, want %d", plan.Tenants, spec.Tenants)
	}
}

func TestBuildPlanLargeScaleCheap(t *testing.T) {
	// Demonstrates the dry-run "100K tenants, TB-scale" planning works WITHOUT
	// generating the corpus (the spec line) and stays fast.
	spec := Spec{
		Tenants:           100_000,
		Seed:              1,
		DocTypeMix:        NormalizeMix([]TypeWeight{{askerv1.DocType_EMAIL, 5}, {askerv1.DocType_FILE, 5}}),
		RareTokenRate:     0.01,
		MinDocs:           10,
		MaxDocs:           5000,
		SkewLargeFraction: 0.05,
	}
	plan := BuildPlan(spec)
	if plan.Tenants != 100_000 {
		t.Fatalf("planned %d tenants, want 100000", plan.Tenants)
	}
	if plan.TotalDocs <= 0 {
		t.Fatal("expected positive total docs")
	}
	out := plan.String()
	for _, want := range []string{"DRY RUN", "tenants:", "total docs:", "est. feed bytes:", "rare tokens:"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output missing %q\n%s", want, out)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{512, "512 B"},
		{2048, "2.00 KiB"},
		{5 << 20, "5.00 MiB"},
		{3 << 30, "3.00 GiB"},
		{2 << 40, "2.00 TiB"},
	}
	for _, c := range cases {
		got := humanBytes(c.n)
		if !strings.HasPrefix(got, c.want) {
			t.Errorf("humanBytes(%d) = %q, want prefix %q", c.n, got, c.want)
		}
	}
}

func TestCheckpointRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ckpt.json")

	// Missing file => fresh.
	cp, err := loadCheckpoint(path)
	if err != nil {
		t.Fatalf("loadCheckpoint(missing): %v", err)
	}
	if cp.LastTenant != -1 {
		t.Errorf("fresh checkpoint LastTenant = %d, want -1", cp.LastTenant)
	}

	spec := testSpec()
	want := Checkpoint{Spec: spec, LastTenant: 5, RareBase: 12, DocsFed: 300, BytesFed: 99999}
	if err := saveCheckpoint(path, want); err != nil {
		t.Fatalf("saveCheckpoint: %v", err)
	}
	got, err := loadCheckpoint(path)
	if err != nil {
		t.Fatalf("loadCheckpoint: %v", err)
	}
	if got.LastTenant != want.LastTenant || got.RareBase != want.RareBase ||
		got.DocsFed != want.DocsFed || got.BytesFed != want.BytesFed {
		t.Errorf("checkpoint round-trip mismatch: got %+v want %+v", got, want)
	}
	if !specsEqual(got.Spec, want.Spec) {
		t.Error("checkpoint spec did not round-trip equal")
	}
}

func TestSpecsEqual(t *testing.T) {
	a := testSpec()
	b := testSpec()
	if !specsEqual(a, b) {
		t.Error("identical specs should be equal")
	}
	b.Seed = a.Seed + 1
	if specsEqual(a, b) {
		t.Error("specs with different seeds should not be equal")
	}
	c := testSpec()
	c.DocTypeMix = NormalizeMix([]TypeWeight{{askerv1.DocType_EMAIL, 1}})
	if specsEqual(a, c) {
		t.Error("specs with different mixes should not be equal")
	}
}
