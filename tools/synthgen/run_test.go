package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

func TestRunnerFeedsWholeCorpus(t *testing.T) {
	spec := testSpec()
	ff := newFakeFeeder()
	r := &Runner{
		Spec:          spec,
		Feeder:        ff,
		Concurrency:   4,
		ProgressEvery: 0, // no ticker in tests
		Out:           &bytes.Buffer{},
	}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	plan := BuildPlan(spec)
	if res.Docs != plan.TotalDocs {
		t.Errorf("fed %d docs, plan says %d", res.Docs, plan.TotalDocs)
	}
	if ff.count() != int(plan.TotalDocs) {
		t.Errorf("feeder recorded %d docs, want %d", ff.count(), plan.TotalDocs)
	}
	if res.Tenants != int64(spec.Tenants) {
		t.Errorf("fed %d tenants, want %d", res.Tenants, spec.Tenants)
	}
	if res.RareTokens != plan.RareTokens {
		t.Errorf("runner counted %d rare tokens, plan says %d", res.RareTokens, plan.RareTokens)
	}
	if res.Errors != 0 {
		t.Errorf("unexpected feed errors: %d", res.Errors)
	}

	// Every fed doc is correctly tenant-scoped (its group is its tenant id) and
	// no two docs collide on doc_id (across tenants).
	ids := map[string]bool{}
	for _, d := range ff.fed() {
		if d.Doc.GetTenantId() != d.TenantID() {
			t.Fatalf("doc tenant mismatch")
		}
		if ids[d.Doc.GetDocId()] {
			t.Fatalf("duplicate doc_id %q across corpus", d.Doc.GetDocId())
		}
		ids[d.Doc.GetDocId()] = true
	}
}

func TestRunnerResumeFromCheckpoint(t *testing.T) {
	spec := testSpec()
	dir := t.TempDir()
	ckpt := filepath.Join(dir, "ckpt.json")

	// First run: stop after tenant 2 by failing on tenant index 3's docs.
	ff1 := newFakeFeeder()
	ff1.fail = func(d GenDoc) error {
		if d.Doc.GetMetadata()["tenant_index"] == "3" {
			return errors.New("boom")
		}
		return nil
	}
	r1 := &Runner{Spec: spec, Feeder: ff1, Concurrency: 1, CheckpointPath: ckpt, Out: &bytes.Buffer{}}
	_, err := r1.Run(context.Background())
	if err == nil {
		t.Fatal("expected the injected failure to abort the first run")
	}

	cp, err := loadCheckpoint(ckpt)
	if err != nil {
		t.Fatalf("loadCheckpoint: %v", err)
	}
	if cp.LastTenant != 2 {
		t.Fatalf("checkpoint LastTenant = %d, want 2 (tenants 0,1,2 completed)", cp.LastTenant)
	}

	// Second run resumes from the checkpoint with no failure; the union of both
	// runs' fed docs must equal exactly the full corpus, no duplicates.
	ff2 := newFakeFeeder()
	r2 := &Runner{Spec: spec, Feeder: ff2, Concurrency: 2, CheckpointPath: ckpt, Out: &bytes.Buffer{}}
	res2, err := r2.Run(context.Background())
	if err != nil {
		t.Fatalf("resume run: %v", err)
	}

	plan := BuildPlan(spec)
	ids := map[string]bool{}
	for _, d := range ff1.fed() {
		ids[d.Doc.GetDocId()] = true
	}
	for _, d := range ff2.fed() {
		if ids[d.Doc.GetDocId()] {
			t.Errorf("doc_id %q fed in BOTH runs (resume re-fed a completed tenant)", d.Doc.GetDocId())
		}
		ids[d.Doc.GetDocId()] = true
	}
	if int64(len(ids)) != plan.TotalDocs {
		t.Errorf("combined unique docs %d, want full corpus %d", len(ids), plan.TotalDocs)
	}
	// The resume must have advanced the rare base to the full corpus total.
	if res2.RareTokens != plan.RareTokens {
		t.Errorf("post-resume rare tokens %d, want %d", res2.RareTokens, plan.RareTokens)
	}
}

func TestRunnerRejectsMismatchedCheckpoint(t *testing.T) {
	dir := t.TempDir()
	ckpt := filepath.Join(dir, "ckpt.json")
	// Save a checkpoint for one spec...
	if err := saveCheckpoint(ckpt, Checkpoint{Spec: testSpec(), LastTenant: 1, RareBase: 2}); err != nil {
		t.Fatal(err)
	}
	// ...then try to resume with a different spec.
	other := testSpec()
	other.Seed = 999
	r := &Runner{Spec: other, Feeder: newFakeFeeder(), Concurrency: 1, CheckpointPath: ckpt, Out: &bytes.Buffer{}}
	if _, err := r.Run(context.Background()); err == nil {
		t.Fatal("expected resume to be rejected on a spec mismatch")
	}
}

func TestRunnerContextCancel(t *testing.T) {
	spec := testSpec()
	spec.Tenants = 200
	ff := newFakeFeeder()
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after the very first doc is fed.
	ff.fail = func(GenDoc) error {
		cancel()
		return nil
	}
	r := &Runner{Spec: spec, Feeder: ff, Concurrency: 1, Out: &bytes.Buffer{}}
	res, err := r.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	// It must have stopped early, not fed all 200 tenants.
	if res.Tenants >= int64(spec.Tenants) {
		t.Errorf("cancel did not stop the run early: fed %d/%d tenants", res.Tenants, spec.Tenants)
	}
}

func TestVespaFieldsFor(t *testing.T) {
	spec := testSpec()
	docs, _ := GenerateTenant(spec, PlanTenants(spec)[0], 0)
	if len(docs) == 0 {
		t.Skip("no docs")
	}
	d := docs[0].Doc
	f := vespaFieldsFor(d)
	if f.DocID != d.GetDocId() || f.Type != d.GetType().String() || f.Title != d.GetTitle() {
		t.Errorf("vespaFieldsFor mismatch: %+v", f)
	}
	if f.CreatedAt != d.GetTs().GetCreated().GetSeconds() {
		t.Errorf("created_at %d, want %d", f.CreatedAt, d.GetTs().GetCreated().GetSeconds())
	}
	// metadata_json must be valid sorted-key JSON carrying the isolation marker.
	if f.MetadataJSON == "" || !bytes.Contains([]byte(f.MetadataJSON), []byte(docs[0].IsolationMarker)) {
		t.Errorf("metadata_json missing isolation marker: %q", f.MetadataJSON)
	}
}

func TestSummaryRenders(t *testing.T) {
	var buf bytes.Buffer
	res := RunResult{Tenants: 3, Docs: 30, Bytes: 1 << 20, RareTokens: 4, Elapsed: 2 * time.Second}
	PrintSummary(&buf, newFakeFeeder(), res)
	for _, want := range []string{"synthgen summary", "tenants fed:", "docs fed:", "rare tokens:", "throughput:"} {
		if !bytes.Contains(buf.Bytes(), []byte(want)) {
			t.Errorf("summary missing %q\n%s", want, buf.String())
		}
	}
}

func TestParseDocTypes(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
		wantLen int
	}{
		{"EMAIL:5,FILE:3,IMAGE:2", false, 3},
		{"email:1", false, 1}, // case-insensitive
		{"EMAIL", false, 1},   // default weight 1
		{"", true, 0},
		{"NOPE:1", true, 0},
		{"DOC_TYPE_UNSPECIFIED:1", true, 0},
		{"EMAIL:-1", true, 0},
	}
	for _, c := range cases {
		mix, err := parseDocTypes(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseDocTypes(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseDocTypes(%q): %v", c.in, err)
			continue
		}
		if len(mix) != c.wantLen {
			t.Errorf("parseDocTypes(%q): got %d entries, want %d", c.in, len(mix), c.wantLen)
		}
		var sum float64
		for _, tw := range mix {
			sum += tw.Weight
		}
		if sum < 0.999 || sum > 1.001 {
			t.Errorf("parseDocTypes(%q): weights sum to %v, want 1", c.in, sum)
		}
	}
}

func TestSpecFromConfigValidation(t *testing.T) {
	base := defaultConfig()
	base.dryRun = true

	good := base
	if _, err := specFromConfig(good); err != nil {
		t.Errorf("default config should be valid: %v", err)
	}

	bad := base
	bad.tenants = 0
	if _, err := specFromConfig(bad); err == nil {
		t.Error("expected error for tenants=0")
	}

	bad = base
	bad.rareRate = 2
	if _, err := specFromConfig(bad); err == nil {
		t.Error("expected error for rare-token-rate>1")
	}

	bad = base
	bad.minDocs = 100
	bad.maxDocs = 10
	if _, err := specFromConfig(bad); err == nil {
		t.Error("expected error for max-docs<min-docs")
	}

	// docs-per-tenant pins min==max.
	uni := base
	uni.docsPerTenant = 12
	spec, err := specFromConfig(uni)
	if err != nil {
		t.Fatalf("uniform config: %v", err)
	}
	if spec.MinDocs != 12 || spec.MaxDocs != 12 {
		t.Errorf("docs-per-tenant=12 => min/max %d/%d, want 12/12", spec.MinDocs, spec.MaxDocs)
	}
}

func TestFeederFromConfig(t *testing.T) {
	cfg := defaultConfig()
	cfg.target = "vespa-direct"
	if _, err := feederFromConfig(cfg); err != nil {
		t.Errorf("vespa-direct feeder: %v", err)
	}

	cfg.target = "gateway-upload"
	if _, err := feederFromConfig(cfg); err == nil {
		t.Error("gateway-upload without token should error")
	}
	cfg.token = "tok"
	if _, err := feederFromConfig(cfg); err != nil {
		t.Errorf("gateway-upload feeder: %v", err)
	}

	cfg.target = "nonsense"
	if _, err := feederFromConfig(cfg); err == nil {
		t.Error("unknown target should error")
	}
}

// TestGatewayFeederSkipsMediaParticipants is a guard that the generator does not
// attach participants to file/media doc types (the M1 model), which the
// gateway-upload path would otherwise carry meaninglessly.
func TestMediaTypesHaveNoParticipants(t *testing.T) {
	for _, dt := range []askerv1.DocType{askerv1.DocType_IMAGE, askerv1.DocType_VIDEO, askerv1.DocType_AUDIO, askerv1.DocType_FILE} {
		if ps := participantsFor(dt, "x@example.com", true); ps != nil {
			t.Errorf("type %v should have no participants, got %v", dt, ps)
		}
	}
	if ps := participantsFor(askerv1.DocType_EMAIL, "x@example.com", true); len(ps) != 2 {
		t.Errorf("EMAIL should have 2 participants, got %d", len(ps))
	}
}
