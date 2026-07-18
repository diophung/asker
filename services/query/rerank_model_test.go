package main

import (
	"context"
	"errors"
	"testing"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
)

// fakeReranker returns a score per document, looked up by the document text.
type fakeReranker struct {
	byDoc    map[string]float64
	err      error
	gotQuery string
	gotDocs  []string
}

func (f *fakeReranker) Rerank(_ context.Context, query string, docs []string) ([]float64, error) {
	f.gotQuery, f.gotDocs = query, docs
	if f.err != nil {
		return nil, f.err
	}
	out := make([]float64, len(docs))
	for i, d := range docs {
		out[i] = f.byDoc[d]
	}
	return out, nil
}

func hitIDsOf(hits []*queryv1.Hit) []string {
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.GetDocId()
	}
	return ids
}

func TestApplyRerankReordersAndRewritesScores(t *testing.T) {
	hits := []*queryv1.Hit{
		{DocId: "a", Title: "alpha", Score: 0.1},
		{DocId: "b", Title: "bravo", Score: 0.9},
		{DocId: "c", Title: "charlie", Score: 0.5},
	}
	fr := &fakeReranker{byDoc: map[string]float64{"alpha": 10, "bravo": 1, "charlie": 5}}

	got, err := applyRerank(context.Background(), fr, "q", hits, 10, 100)
	if err != nil {
		t.Fatal(err)
	}
	if ids := hitIDsOf(got); len(ids) != 3 || ids[0] != "a" || ids[1] != "c" || ids[2] != "b" {
		t.Fatalf("order=%v want [a c b]", hitIDsOf(got))
	}
	// Scores rewritten to the cross-encoder relevance.
	if got[0].GetScore() != 10 || got[1].GetScore() != 5 || got[2].GetScore() != 1 {
		t.Fatalf("scores=%v/%v/%v", got[0].GetScore(), got[1].GetScore(), got[2].GetScore())
	}
	if fr.gotQuery != "q" {
		t.Fatalf("reranker saw query %q", fr.gotQuery)
	}
}

func TestApplyRerankTruncatesToDepth(t *testing.T) {
	hits := []*queryv1.Hit{
		{DocId: "a", Title: "a"}, {DocId: "b", Title: "b"}, {DocId: "c", Title: "c"},
	}
	fr := &fakeReranker{byDoc: map[string]float64{"a": 1, "b": 2, "c": 3}}
	got, err := applyRerank(context.Background(), fr, "q", hits, 2, 100) // depth 2
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d want 2 (truncated to rerank depth)", len(got))
	}
	if len(fr.gotDocs) != 2 {
		t.Fatalf("reranker was sent %d docs, want 2", len(fr.gotDocs))
	}
}

func TestApplyRerankErrorPassesThrough(t *testing.T) {
	hits := []*queryv1.Hit{{DocId: "a", Title: "a"}}
	fr := &fakeReranker{err: errors.New("boom")}
	got, err := applyRerank(context.Background(), fr, "q", hits, 10, 100)
	if err == nil || got != nil {
		t.Fatalf("want (nil, err), got (%v, %v)", got, err)
	}
}

func TestApplyRerankEmpty(t *testing.T) {
	got, err := applyRerank(context.Background(), &fakeReranker{}, "q", nil, 10, 100)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty: got=%v err=%v", got, err)
	}
}

func TestRerankDocText(t *testing.T) {
	h := &queryv1.Hit{Title: "Budget", Snippet: "the <hi>quarterly</hi> budget review"}
	got := rerankDocText(h, 1000)
	want := "Budget\nthe quarterly budget review"
	if got != want {
		t.Fatalf("rerankDocText=%q want %q", got, want)
	}
	// Rune cap applies.
	if capped := rerankDocText(&queryv1.Hit{Title: "abcdef"}, 3); len([]rune(capped)) > 4 { // 3 + possible ellipsis
		t.Fatalf("cap not applied: %q", capped)
	}
}
