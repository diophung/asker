package main

import "testing"

func frozenV2Config() RunConfig {
	return RunConfig{K: 10, Limit: 20, MaxP95Ms: 5000, MinTaskSuccess: .90,
		MinExactSuccess: .98, MinNeedCoverage: .95, MinLatencySamples: 100,
		RequireUncached: true, Concurrency: 1, Repeats: 3,
		Baseline: "dense", Candidate: "hybrid"}
}

func TestFrozenV2StructuralSplitsAndResolvedTokenLabels(t *testing.T) {
	const manifest = "fixtures/local-v2/manifest.json"
	if digest, err := verifyManifest(manifest); err != nil || len(digest) != 64 {
		t.Fatalf("v2 integrity validation failed: %v", err)
	}
	for split, count := range map[string]int{"dev": 38, "regression": 39, "holdout": 40} {
		t.Run(split, func(t *testing.T) {
			records, err := LoadGolden("fixtures/local-v2/" + split + ".jsonl")
			if err != nil || len(records) != count || len(sliceCounts(records)) != 9 {
				t.Fatal("declared split structure is invalid")
			}
			for index := range records {
				records[index].Tenant = "verified-eval-principal"
			}
			if err := validateFrozenBenchmark(manifest, records, frozenV2Config()); err != nil {
				t.Fatal("resolving a tenant token label must preserve the frozen task contract")
			}
		})
	}
}

func TestFrozenV2RejectsChangedCriteriaAndGoldenTasks(t *testing.T) {
	const manifest = "fixtures/local-v2/manifest.json"
	records, err := LoadGolden("fixtures/local-v2/dev.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*RunConfig){
		"cutoff":             func(c *RunConfig) { c.K = 20 },
		"returned hits":      func(c *RunConfig) { c.Limit = 50 },
		"latency":            func(c *RunConfig) { c.MaxP95Ms = 6000 },
		"task success":       func(c *RunConfig) { c.MinTaskSuccess = .89 },
		"exact success":      func(c *RunConfig) { c.MinExactSuccess = .97 },
		"need coverage":      func(c *RunConfig) { c.MinNeedCoverage = .94 },
		"samples":            func(c *RunConfig) { c.MinLatencySamples = 1 },
		"cached":             func(c *RunConfig) { c.RequireUncached = false },
		"equality":           func(c *RunConfig) { c.AllowEqual = true },
		"load qualification": func(c *RunConfig) { c.Concurrency = 2 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			config := frozenV2Config()
			mutate(&config)
			if validateFrozenBenchmark(manifest, records, config) == nil {
				t.Fatal("changed qualification criteria must fail before any service request")
			}
		})
	}
	if validateFrozenBenchmark(manifest, records[:len(records)-1], frozenV2Config()) == nil {
		t.Fatal("dropping a task must fail closed")
	}
	changed := append([]GoldenRecord(nil), records...)
	changed[0].Query += " edited"
	if validateFrozenBenchmark(manifest, changed, frozenV2Config()) == nil {
		t.Fatal("editing resolved labels must fail closed")
	}
}
