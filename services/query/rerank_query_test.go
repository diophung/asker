package main

import (
	"context"
	"testing"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

func correctionHead(titles ...string) []*queryv1.Hit {
	hits := make([]*queryv1.Hit, len(titles))
	for i, title := range titles {
		hits[i] = &queryv1.Hit{DocId: string(rune('a' + i)), Title: title}
	}
	return hits
}

func TestRerankQueryConservativelyCorrectsSoftTypos(t *testing.T) {
	head := correctionHead("Restoring records recovery document", "Restoring recovery document archive", "Charging station purchase", "Charging station purchase proposal")
	for _, tc := range []struct{ query, want string }{
		{"restroing recovrey", "restoring recovery"},
		{"chargng staton purchse", "charging station purchse"}, // at most two corrections
		{"charging staton purchse", "charging station purchase"},
		{"documnet recovery", "document recovery"},
		{"restoring recovery", "restoring recovery"}, // existing terms stay exact
		{"Restroing recovrey", "Restroing recovery"}, // names/capitalized words stay literal
		{"AX-48271 restroing", "AX-48271 restoring"}, // identifier is never rewritten
		{`"restroing recovery"`, `"restroing recovery"`},
		{"from:alice restroing", "from:alice restroing"},
		{"restroing@example.invalid", "restroing@example.invalid"},
		{"未知 restore42 recovery", "未知 restore42 recovery"},
	} {
		if got := correctRerankQuery(context.Background(), tc.query, head); got != tc.want {
			t.Errorf("correction(%q)=%q want %q", tc.query, got, tc.want)
		}
	}
}

func TestRerankQueryNeedsUnambiguousIndependentTitleSupport(t *testing.T) {
	for _, head := range [][]*queryv1.Hit{
		correctionHead("forms directory", "forms archive", "farms records", "farms inventory"),
		correctionHead("forms directory"), // one document is insufficient
		{{DocId: "same", Title: "forms directory"}, {DocId: "same", Title: "forms archive"}},
	} {
		if got := correctRerankQuery(context.Background(), "firms", head); got != "firms" {
			t.Errorf("weak/ambiguous candidate vocabulary changed query to %q", got)
		}
	}
}

func TestThirdRerankCorrectionNeedsUnchangedRepeatedContentAnchor(t *testing.T) {
	head := correctionHead(
		"Equipment charging station purchase approval document after",
		"Equipment charging station purchase approval archive after",
	)
	for _, tc := range []struct{ query, want string }{
		{"equipment chargng staton purchse documnet", "equipment charging station purchase documnet"},
		{"Equipment chargng staton purchse", "Equipment charging station purchase"},
		{"chargng staton purchse", "charging station purchse"},
		{"after chargng staton purchse", "after charging station purchse"},
		{"equipmnt chargng staton purchse", "equipment charging staton purchse"},
		{`"equipment chargng staton purchse"`, `"equipment chargng staton purchse"`},
	} {
		if got := correctRerankQuery(context.Background(), tc.query, head); got != tc.want {
			t.Errorf("anchored correction(%q)=%q want %q", tc.query, got, tc.want)
		}
	}
	// A single-title context is insufficient, even though the correction targets
	// themselves have independent support in two titles.
	head[1].Title = "Charging station purchase approval archive"
	if got := correctRerankQuery(context.Background(), "equipment chargng staton purchse", head); got != "equipment charging station purchse" {
		t.Fatalf("weak anchor enabled a third change: %q", got)
	}
	// Strong context never resolves an ambiguous third word by map iteration.
	head = correctionHead("Equipment charging station forms farms", "Equipment charging station forms farms archive")
	if got := correctRerankQuery(context.Background(), "equipment chargng staton firms", head); got != "equipment charging station firms" {
		t.Fatalf("anchor weakened individual ambiguity checks: %q", got)
	}
}

func TestRerankQueryDoesNotUseBodyOnlyVocabulary(t *testing.T) {
	head := []*queryv1.Hit{
		{DocId: "a", Title: "Overview", Snippet: "restoring records"},
		{DocId: "b", Title: "Instructions", Snippet: "restoring records"},
	}
	if got := correctRerankQuery(context.Background(), "restroing records", head); got != "restroing records" {
		t.Fatalf("untrusted weak body vocabulary rewrote query: %q", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := correctRerankQuery(ctx, "restroing records", correctionHead("restoring records", "restoring archive")); got != "restroing records" {
		t.Fatalf("canceled correction performed work: %q", got)
	}
}

func TestOneEditApart(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"restroing", "restoring", true}, // adjacent transposition
		{"chargng", "charging", true},    // insertion
		{"chargingg", "charging", true},  // deletion
		{"restaring", "restoring", true}, // substitution
		{"restoring", "restoring", false},
		{"rstaring", "restoring", false},
		{"abcdef", "abcfed", false},
	} {
		if got := oneEditApart(tc.a, tc.b); got != tc.want {
			t.Errorf("oneEditApart(%q,%q)=%v want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestRerankCorrectionOnlyChangesModelInput(t *testing.T) {
	rr := &fakeReranker{}
	env := newQueryEnv(t, withReranker(rr))
	env.vespa.setProfileFixture("hybrid", buildFixture(t, []docSpec{
		{id: "a", typ: "FILE", title: "Restoring records document", relevance: .9},
		{id: "b", typ: "FILE", title: "Restoring recovery guide", relevance: .8},
	}))
	request := &queryv1.SearchRequest{Query: "restroing records", Rerank: true}
	if _, err := env.client.Search(tenantCtx(t, "alice"), request); err != nil {
		t.Fatal(err)
	}
	if rr.gotQuery != "restoring records" || request.Query != "restroing records" {
		t.Fatalf("model input or original query changed incorrectly: model=%q original=%q", rr.gotQuery, request.Query)
	}
	if got := env.vespa.bodyForProfile(t, "hybrid")["query"]; got != "restroing records" {
		t.Fatalf("retrieval was silently rewritten: %v", got)
	}
	// Even a supported hard source filter keeps the model query literal.
	request.DocTypes = []askerv1.DocType{askerv1.DocType_FILE}
	if _, err := env.client.Search(tenantCtx(t, "alice"), request); err != nil {
		t.Fatal(err)
	}
	if rr.gotQuery != "restroing records" {
		t.Fatalf("hard-filtered request was rewritten: %q", rr.gotQuery)
	}
}
