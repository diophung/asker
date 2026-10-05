package main

import (
	"strings"
	"testing"
)

func TestParseGoldenValid(t *testing.T) {
	in := `
# a comment line
{"id":"x1","query":"qzx00000001","tenant":"alice","slice":"exact","relevant":["doc-a"]}

{"query":"budget roadmap","tenant":"alice","slice":"keyword","gains":{"doc-b":3,"doc-c":1}}
`
	recs, err := parseGolden(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records want 2", len(recs))
	}
	if recs[0].ID != "x1" {
		t.Fatalf("id=%q want x1", recs[0].ID)
	}
	if recs[1].ID == "" {
		t.Fatalf("second record should get a synthesized id")
	}
	if g := recs[1].judged(); g["doc-b"] != 3 || g["doc-c"] != 1 {
		t.Fatalf("graded gains not honored: %v", g)
	}
}

func TestParseGoldenRejectsBadRecords(t *testing.T) {
	cases := map[string]string{
		"empty query":    `{"query":"","tenant":"a","slice":"exact","relevant":["d"]}`,
		"no relevant":    `{"query":"q","tenant":"a","slice":"exact"}`,
		"unknown slice":  `{"query":"q","tenant":"a","slice":"bogus","relevant":["d"]}`,
		"empty tenant":   `{"query":"q","tenant":"","slice":"exact","relevant":["d"]}`,
		"unknown field":  `{"query":"q","tenant":"a","slice":"exact","relevant":["d"],"oops":1}`,
		"malformed json": `{"query":`,
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseGolden(strings.NewReader(line)); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestParseGoldenRejectsDuplicateID(t *testing.T) {
	in := `{"id":"dup","query":"a","tenant":"t","slice":"exact","relevant":["d"]}
{"id":"dup","query":"b","tenant":"t","slice":"exact","relevant":["e"]}`
	if _, err := parseGolden(strings.NewReader(in)); err == nil {
		t.Fatalf("expected duplicate-id error")
	}
}

func TestParseGoldenEmptyIsError(t *testing.T) {
	if _, err := parseGolden(strings.NewReader("# only comments\n\n")); err == nil {
		t.Fatalf("expected empty-golden error")
	}
}

func TestSliceCounts(t *testing.T) {
	recs := []GoldenRecord{
		{Slice: SliceExact}, {Slice: SliceExact}, {Slice: SliceSemantic},
	}
	c := sliceCounts(recs)
	if c[SliceExact] != 2 || c[SliceSemantic] != 1 {
		t.Fatalf("counts=%v", c)
	}
	if got := sortedSlices(recs); len(got) != 2 || got[0] != SliceExact || got[1] != SliceSemantic {
		t.Fatalf("sortedSlices=%v", got)
	}
}

func TestExtendedGoldenLabelsAndFrozenFixtures(t *testing.T) {
	in := `{"id":"none","query":"unknown","tenant":"alice","slice":"no_match","no_match":true,"split":"dev","filters":{"types":"EMAIL"}}
{"id":"constraint","query":"approved -draft","tenant":"alice","slice":"negation","relevant":["a"],"forbidden":["b"],"required":[["a"]],"split":"regression"}`
	if _, err := parseGolden(strings.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	for _, split := range []string{"dev", "regression", "holdout"} {
		recs, err := LoadGolden("fixtures/local-v1/" + split + ".jsonl")
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) != 13 {
			t.Fatalf("%s frozen fixture query count changed: %d", split, len(recs))
		}
		for _, rec := range recs {
			if rec.Split != split {
				t.Fatalf("unexpected split: %+v", rec)
			}
		}
	}
}

func TestExtendedGoldenRejectsContradictoryLabels(t *testing.T) {
	for _, in := range []string{
		`{"query":"q","tenant":"a","slice":"no_match","no_match":true,"relevant":["d"]}`,
		`{"query":"q","tenant":"a","slice":"negation","relevant":["d"],"forbidden":["d"]}`,
		`{"query":"q","tenant":"a","slice":"filter","relevant":["d"],"filters":{"tenant":"victim"}}`,
		`{"query":"q","tenant":"a","slice":"multi_need","relevant":["d"],"required":[["missing"]]}`,
	} {
		if _, err := parseGolden(strings.NewReader(in)); err == nil {
			t.Fatalf("contradictory labels accepted: %s", in)
		}
	}
}
