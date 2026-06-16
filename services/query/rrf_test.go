package main

import (
	"testing"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
)

func rrfHit(id string) *queryv1.Hit { return &queryv1.Hit{DocId: id} }

func TestRRFFuseRanksConsensusHighest(t *testing.T) {
	// keyword arm ranks: a, b, c ; vector arm ranks: b, d, a.
	// b is rank1+rank2, a is rank1+rank3 -> b and a should top the fused list,
	// with d and c (single-arm, deeper) below.
	keyword := []*queryv1.Hit{rrfHit("a"), rrfHit("b"), rrfHit("c")}
	vector := []*queryv1.Hit{rrfHit("b"), rrfHit("d"), rrfHit("a")}

	fused := rrfFuse(keyword, vector)

	if len(fused) != 4 {
		t.Fatalf("fused len = %d, want 4 (a,b,c,d deduped)", len(fused))
	}
	pos := map[string]int{}
	for i, h := range fused {
		pos[h.GetDocId()] = i
	}
	// b: 1/61 + 1/61 ; a: 1/61 + 1/63 ; so b > a, both above c and d.
	if pos["b"] != 0 {
		t.Errorf("b at %d, want top (appears high in both arms)", pos["b"])
	}
	if pos["a"] >= pos["c"] || pos["a"] >= pos["d"] {
		t.Errorf("a (two arms) did not outrank single-arm c/d: %v", pos)
	}
	// Fused scores are written onto Score, descending.
	for i := 1; i < len(fused); i++ {
		if fused[i-1].GetScore() < fused[i].GetScore() {
			t.Errorf("fused not sorted by score desc: %v", fused)
		}
	}
}

func TestRRFFuseSingleArm(t *testing.T) {
	one := []*queryv1.Hit{rrfHit("x"), rrfHit("y")}
	fused := rrfFuse(one)
	if len(fused) != 2 || fused[0].GetDocId() != "x" || fused[1].GetDocId() != "y" {
		t.Errorf("single-arm fuse changed order: %v", fused)
	}
}

func TestRRFFuseInheritsMediaFields(t *testing.T) {
	// A text-arm hit for doc-1 lacks media fields; the CLIP arm's doc-1 carries
	// them — the fused hit should inherit modality/thumbnail.
	text := []*queryv1.Hit{{DocId: "doc-1"}}
	clip := []*queryv1.Hit{{DocId: "doc-1", Modality: "caption", ThumbnailKey: "t/doc-1.jpg", StartMs: 5}}
	fused := rrfFuse(text, clip)
	if len(fused) != 1 {
		t.Fatalf("fused len = %d, want 1", len(fused))
	}
	if fused[0].GetModality() != "caption" || fused[0].GetThumbnailKey() != "t/doc-1.jpg" || fused[0].GetStartMs() != 5 {
		t.Errorf("media fields not inherited: %+v", fused[0])
	}
}

func TestRRFFuseSkipsEmptyDocID(t *testing.T) {
	fused := rrfFuse([]*queryv1.Hit{{DocId: ""}, {DocId: "a"}})
	if len(fused) != 1 || fused[0].GetDocId() != "a" {
		t.Errorf("empty doc_id not skipped: %v", fused)
	}
}
