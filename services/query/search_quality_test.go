package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/platform/personalization"
	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

func TestQuotedAndExcludedContentIsHardFilteredAcrossArms(t *testing.T) {
	plan := understand(&queryv1.SearchRequest{Query: `budget "project phoenix" -draft -"last week" type:email`})
	if plan.SyntaxError != "" || len(plan.TextClauses) != 3 || strings.Contains(plan.Text, "draft") || !strings.Contains(plan.Text, `"project phoenix"`) {
		t.Fatalf("unexpected parsed plan: %+v", plan)
	}
	for _, kind := range []retrievalKind{retrieveKeyword, retrieveHybrid, retrieveVector, retrieveCLIP} {
		q := vespaQuery{Kind: kind, TextClauses: plan.TextClauses, DocTypes: plan.DocTypes}
		yql, err := buildYQL(q)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`phrase("project", "phoenix")`, `!(title contains ({stem:false}"draft")`, `phrase("last", "week")`, `type contains "EMAIL"`} {
			if !strings.Contains(yql, want) {
				t.Errorf("arm %d omitted hard clause %q: %s", kind, want, yql)
			}
		}
	}
}

func TestStandaloneIdentifierIsLiteralAcrossAllArms(t *testing.T) {
	for _, query := range []string{"AX-48271", "ax-48271", "REQ_72", "AX-48271 type:email"} {
		plan := understand(&queryv1.SearchRequest{Query: query})
		if len(plan.TextClauses) != 1 || plan.TextClauses[0].Exclude || plan.TextClauses[0].Text != plan.Text {
			t.Fatalf("identifier %q lost its literal constraint: %+v", query, plan)
		}
		for _, kind := range []retrievalKind{retrieveKeyword, retrieveHybrid, retrieveVector, retrieveCLIP} {
			yql, err := buildYQL(vespaQuery{Kind: kind, Text: plan.Text, TextClauses: plan.TextClauses})
			if err != nil || !strings.Contains(yql, `title contains ({stem:false}"`+plan.Text+`")`) || !strings.Contains(yql, `body contains ({stem:false}"`+plan.Text+`")`) || !strings.Contains(yql, `chunks contains ({stem:false}"`+plan.Text+`")`) {
				t.Fatalf("arm %d omitted identifier %q: %s, %v", kind, plan.Text, yql, err)
			}
		}
	}
	for _, query := range []string{"AX-48271 status", "2026-10-06", "Q4-2026", "budget 2026", "09:00", "AX-48-271"} {
		if plan := understand(&queryv1.SearchRequest{Query: query}); len(plan.TextClauses) != 0 {
			t.Fatalf("ordinary prose/date %q became an identifier constraint: %+v", query, plan)
		}
	}
}

func TestQuotedFiltersAndTemporalWordsRemainContent(t *testing.T) {
	plan := understand(&queryv1.SearchRequest{Query: `"from:alice after:2026-01-01" "last week report"`})
	if plan.Participant != "" || !plan.From.IsZero() || len(plan.TextClauses) != 2 {
		t.Fatalf("quoted content interpreted as filter: %+v", plan)
	}
	scoped := scopeQuery(plan, personalization.DefaultProfile(), time.Now())
	if scoped.Text != plan.Text || !scoped.From.IsZero() || !scoped.EventFrom.IsZero() {
		t.Fatalf("quoted temporal content scoped: %+v", scoped)
	}
}

func TestUnsupportedFiltersPreserveTextAndDisclose(t *testing.T) {
	for _, query := range []string{"subject:budget report", "type:unknown tips", "after:invalid dates"} {
		plan := understand(&queryv1.SearchRequest{Query: query})
		if plan.Text != query || !strings.Contains(joinDegraded(plan.Warnings), "unsupported-filter-syntax") {
			t.Fatalf("unsupported query silently changed: %+v", plan)
		}
	}
	if plan := understand(&queryv1.SearchRequest{Query: `"unterminated`}); plan.SyntaxError == "" {
		t.Fatal("unterminated quote accepted")
	}
	if plan := understand(&queryv1.SearchRequest{Query: `""`}); plan.SyntaxError == "" {
		t.Fatal("empty quoted phrase accepted")
	}
}

func TestSourceAndContentIntentPrecedence(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, query := range []string{"meeting notes", "due diligence", "calendar migration proposal", "eventual consistency", "deadline architecture"} {
		plan := scopeQuery(parsedQuery{Text: query}, personalization.DefaultProfile(), now)
		if plan.Intent != intentFindItem || len(plan.DocTypes) != 0 || !plan.EventFrom.IsZero() {
			t.Errorf("content query %q changed source/intent: %+v", query, plan)
		}
	}
	for _, query := range []string{"meetings tomorrow", "my calendar next week", "next week"} {
		plan := scopeQuery(parsedQuery{Text: query, DocTypes: []askerv1.DocType{askerv1.DocType_EMAIL}}, personalization.DefaultProfile(), now)
		if plan.Intent == intentScheduleLookup || !plan.EventFrom.IsZero() || !plan.EventTo.IsZero() || len(plan.DocTypes) != 1 || plan.DocTypes[0] != askerv1.DocType_EMAIL {
			t.Errorf("email source overruled by %q: %+v", query, plan)
		}
	}
	for _, query := range []string{"launch review", `"last year strategy"`} {
		plan := scopeQuery(understand(&queryv1.SearchRequest{Query: query, DocTypes: []askerv1.DocType{askerv1.DocType_CALENDAR_EVENT}}), personalization.DefaultProfile(), now)
		if plan.Intent == intentScheduleLookup || !plan.EventFrom.IsZero() || !plan.EventTo.IsZero() || !plan.From.IsZero() {
			t.Errorf("calendar content lookup %q excluded history: %+v", query, plan)
		}
	}
	plan := scopeQuery(understand(&queryv1.SearchRequest{Query: "launch review last week", DocTypes: []askerv1.DocType{askerv1.DocType_CALENDAR_EVENT}}), personalization.DefaultProfile(), now)
	if !plan.From.IsZero() || plan.EventFrom.IsZero() || plan.EventTo.IsZero() {
		t.Fatalf("calendar content date window used authoring time: %+v", plan)
	}
}

func TestCalendarExplicitDatesScopeOccurrenceAndWin(t *testing.T) {
	from := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	to := from.Add(24*time.Hour - time.Second)
	plan := understand(&queryv1.SearchRequest{Query: "launch review next week", DocTypes: []askerv1.DocType{askerv1.DocType_CALENDAR_EVENT}, FromDate: timestamppb.New(from), ToDate: timestamppb.New(to)})
	plan = scopeQuery(plan, personalization.DefaultProfile(), from.Add(-3*24*time.Hour))
	if !plan.From.IsZero() || !plan.To.IsZero() || !plan.EventFrom.Equal(from) || !plan.EventTo.Equal(to.Add(time.Second)) {
		t.Fatalf("calendar date semantics: %+v", plan)
	}
	yql, err := buildYQL(vespaQuery{Kind: retrieveKeyword, EventFrom: plan.EventFrom, EventTo: plan.EventTo})
	if err != nil || strings.Contains(yql, "created_at") || !strings.Contains(yql, "event_start") {
		t.Fatalf("calendar retrieval: %s, %v", yql, err)
	}
	attention := scopeQuery(understand(&queryv1.SearchRequest{Query: "what needs my attention next week", DocTypes: []askerv1.DocType{askerv1.DocType_CALENDAR_EVENT}, FromDate: timestamppb.New(from), ToDate: timestamppb.New(to)}), personalization.DefaultProfile(), from.Add(-3*24*time.Hour))
	if attention.Intent != intentNeedsAttention || !attention.From.IsZero() || !attention.To.IsZero() || !attention.EventFrom.Equal(from) || !attention.EventTo.Equal(to.Add(time.Second)) {
		t.Fatalf("attention intent changed explicit calendar date field: %+v", attention)
	}
}

func TestNaturalDatesHaveExclusiveUpperBound(t *testing.T) {
	plan := scopeQuery(parsedQuery{Text: "budget last week", DocTypes: []askerv1.DocType{askerv1.DocType_EMAIL}}, personalization.DefaultProfile(), time.Now())
	yql, err := buildYQL(vespaQuery{Kind: retrieveKeyword, From: plan.From, To: plan.To, ToExclusive: plan.ToExclusive})
	if err != nil || !strings.Contains(yql, "created_at < ") || strings.Contains(yql, "created_at <=") {
		t.Fatalf("natural date admitted next boundary: %s, %v", yql, err)
	}
}

func TestCalendarOverlapUsesHalfOpenWindows(t *testing.T) {
	from := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	window := timeWindow{From: from, To: from.Add(24 * time.Hour)}
	for _, tc := range []struct {
		name       string
		start, end time.Time
		want       bool
	}{
		{"multi-day continues", from.Add(-24 * time.Hour), from.Add(2 * time.Hour), true},
		{"ends at lower boundary", from.Add(-time.Hour), from, false},
		{"starts at upper boundary", window.To, window.To.Add(time.Hour), false},
		{"missing end inside", from.Add(time.Hour), time.Time{}, true},
		{"missing end before", from.Add(-time.Hour), time.Time{}, false},
		{"invalid end inside", from.Add(time.Hour), from, true},
		{"invalid end before", from.Add(-time.Hour), from.Add(-2 * time.Hour), false},
	} {
		if got := calendarOverlaps(tc.start, tc.end, window); got != tc.want {
			t.Errorf("%s: got %t, want %t", tc.name, got, tc.want)
		}
	}
	yql, err := buildYQL(vespaQuery{Kind: retrieveVector, EventFrom: window.From, EventTo: window.To})
	if err != nil || !strings.Contains(yql, "event_end >") || !strings.Contains(yql, "event_start <") || !strings.Contains(yql, "!(event_start = 0)") || strings.Contains(yql, "!=") {
		t.Fatalf("dense calendar overlap predicate missing: %s, %v", yql, err)
	}
}

func TestRerankRunsWithoutPersonalizationAndReportsFreshExecution(t *testing.T) {
	rr := &substringReranker{weights: map[string]float64{"alpha": 1, "beta": 5, "gamma": 9}}
	env := newQueryEnv(t, withReranker(rr))
	env.vespa.setProfileFixture("hybrid", buildFixture(t, rerankFixtureDocs()))
	req := &queryv1.SearchRequest{Query: "budget review", Rerank: true, DocTypes: []askerv1.DocType{askerv1.DocType_EMAIL}}
	var header metadata.MD
	resp, err := env.client.Search(tenantCtx(t, "alice"), req, grpc.Header(&header))
	if err != nil || len(resp.GetHits()) == 0 || resp.Hits[0].DocId != "m-gamma" || header.Get("x-asker-rerank-applied")[0] != "true" {
		t.Fatalf("nonpersonalized rerank failed: %+v, header=%v, err=%v", resp, header, err)
	}
	resp, err = env.client.Search(tenantCtx(t, "alice"), req, grpc.Header(&header))
	if err != nil || !resp.Cached || header.Get("x-asker-rerank-applied")[0] != "false" {
		t.Fatalf("cache reuse claimed fresh inference: %+v, header=%v, err=%v", resp, header, err)
	}
}

func TestRerankEmptyCandidatesRemainACleanNoMatch(t *testing.T) {
	rr := &fakeReranker{}
	env := newQueryEnv(t, withReranker(rr))
	env.vespa.setProfileFixture("hybrid", buildFixture(t, nil))
	var header metadata.MD
	resp, err := env.client.Search(tenantCtx(t, "alice"), &queryv1.SearchRequest{Query: "unfindable-item", Rerank: true, DocTypes: []askerv1.DocType{askerv1.DocType_EMAIL}}, grpc.Header(&header))
	if err != nil || len(resp.GetHits()) != 0 || resp.GetTotal() != 0 || resp.GetDegraded() != "" || resp.GetCached() {
		t.Fatalf("empty candidates did not remain a clean no-match: %+v, err=%v", resp, err)
	}
	if rr.gotQuery != "" || len(rr.gotDocs) != 0 {
		t.Fatalf("reranker called for no-match: query=%q, docs=%d", rr.gotQuery, len(rr.gotDocs))
	}
	if values := header.Get("x-asker-rerank-applied"); len(values) != 1 || values[0] != "false" {
		t.Fatalf("no-match claimed fresh reranking: %v", header)
	}
}

func TestRerankPagingPreservesTheCandidatePopulation(t *testing.T) {
	rr := &fakeReranker{byDoc: make(map[string]float64)}
	for i := range 100 {
		rr.byDoc[fmt.Sprintf("item-%02d", i)] = float64(i)
	}
	s := newServer(testEmbedFunc(func(context.Context, string) ([]float32, error) { return []float32{1}, nil }), nil,
		testSearchFunc(func(context.Context, vespaQuery) (vespaResult, error) {
			hits := make([]*queryv1.Hit, 100)
			for i := range hits {
				hits[i] = &queryv1.Hit{DocId: fmt.Sprintf("item-%02d", i), Title: fmt.Sprintf("item-%02d", i), Score: float64(100 - i)}
			}
			return vespaResult{Hits: hits, Total: 1000}, nil
		}), newFakeCache(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.reranker, s.rerankCandidates, s.rerankDocChars, s.candidateCap = rr, 30, 1024, 100
	seen := make(map[string]bool)
	for offset := int32(0); offset < 100; offset += 20 {
		resp, err := s.Search(tenantCtx(t, "alice"), &queryv1.SearchRequest{Query: "budget", Rerank: true, DocTypes: []askerv1.DocType{askerv1.DocType_EMAIL}, Limit: 20, Offset: offset})
		if err != nil || len(resp.GetHits()) != 20 || resp.GetTotal() != 100 || len(rr.gotDocs) != 30 {
			t.Fatalf("offset %d lost candidate population: %+v, model docs=%d, err=%v", offset, resp, len(rr.gotDocs), err)
		}
		if offset == 20 && (resp.Hits[0].DocId != "item-09" || resp.Hits[10].DocId != "item-30" || resp.Hits[19].DocId != "item-39") {
			t.Fatalf("second page did not preserve reranked head/original tail: %v", hitIDsOf(resp.Hits))
		}
		for _, hit := range resp.Hits {
			if seen[hit.DocId] {
				t.Fatalf("duplicate across candidate pages: %s", hit.DocId)
			}
			seen[hit.DocId] = true
		}
	}
	if len(seen) != 100 {
		t.Fatalf("available candidate population=%d, want 100", len(seen))
	}
}

func TestUnconfiguredRerankIsDisclosed(t *testing.T) {
	env := newQueryEnv(t)
	resp, err := env.client.Search(tenantCtx(t, "alice"), &queryv1.SearchRequest{Query: "budget", Rerank: true, DocTypes: []askerv1.DocType{askerv1.DocType_EMAIL}})
	if err != nil || !strings.Contains(resp.Degraded, degradedRerankNotEnabled) {
		t.Fatalf("rerank no-op undisclosed: %+v, %v", resp, err)
	}
}

func TestRRFDiscoversDenseOnlyDocsWithoutPersonalization(t *testing.T) {
	env := newQueryEnv(t, withRRF())
	env.vespa.setProfileFixture("keyword", buildFixture(t, []docSpec{{id: "lexical", typ: "EMAIL", title: "budget", relevance: 0.9}}))
	env.vespa.setProfileFixture("hybrid", buildFixture(t, []docSpec{{id: "semantic-only", typ: "EMAIL", title: "financial planning", relevance: 0.95}}))
	resp, err := env.client.Search(tenantCtx(t, "alice"), &queryv1.SearchRequest{Query: "budget", DocTypes: []askerv1.DocType{askerv1.DocType_EMAIL}})
	if err != nil || len(resp.GetHits()) != 2 {
		t.Fatalf("dense-only match missing: %+v, %v", resp, err)
	}
	if len(env.vespa.recorded()) != 2 {
		t.Fatalf("expected independently scoped lexical+dense arms, got %d", len(env.vespa.recorded()))
	}
}

func TestCacheBypassSkipsReadsAndWrites(t *testing.T) {
	env := newQueryEnv(t)
	req := &queryv1.SearchRequest{Query: "budget", DocTypes: []askerv1.DocType{askerv1.DocType_EMAIL}}
	if _, err := env.client.Search(tenantCtx(t, "alice"), req); err != nil {
		t.Fatal(err)
	}
	beforeGets, beforeSets := env.cache.counts()
	ctx := metadata.AppendToOutgoingContext(tenantCtx(t, "alice"), "x-asker-cache-bypass", "true")
	resp, err := env.client.Search(ctx, req)
	if err != nil || resp.Cached {
		t.Fatalf("bypass reused cache: %+v, %v", resp, err)
	}
	gets, sets := env.cache.counts()
	if gets != beforeGets || sets != beforeSets {
		t.Fatalf("bypass touched cache: %d/%d -> %d/%d", beforeGets, beforeSets, gets, sets)
	}
}

type testEmbedFunc func(context.Context, string) ([]float32, error)

func (f testEmbedFunc) Embed(ctx context.Context, text string) ([]float32, error) {
	return f(ctx, text)
}

type testClipFunc func(context.Context, string) ([]float32, error)

func (f testClipFunc) EmbedText(ctx context.Context, text string) ([]float32, error) {
	return f(ctx, text)
}

type testSearchFunc func(context.Context, vespaQuery) (vespaResult, error)

func (f testSearchFunc) Search(ctx context.Context, q vespaQuery) (vespaResult, error) {
	return f(ctx, q)
}

func TestIndependentEmbeddingsStartInParallel(t *testing.T) {
	started := make(chan bool, 2)
	release := make(chan struct{})
	embed := func(ctx context.Context, _ string) ([]float32, error) {
		started <- true
		select {
		case <-release:
			return []float32{1}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s := newServer(testEmbedFunc(embed), testClipFunc(embed), testSearchFunc(func(context.Context, vespaQuery) (vespaResult, error) { return vespaResult{}, nil }), newFakeCache(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan error, 1)
	defer func() {
		close(release)
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("embedding search did not finish")
		}
	}()
	go func() { _, err := s.Search(tenantCtx(t, "alice"), &queryv1.SearchRequest{Query: "beach"}); done <- err }()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("independent embeddings started sequentially")
		}
	}
}

func TestIndependentRetrievalArmsStartInParallel(t *testing.T) {
	started := make(chan retrievalKind, 2)
	release := make(chan struct{})
	s := newServer(nil, nil, testSearchFunc(func(ctx context.Context, q vespaQuery) (vespaResult, error) {
		started <- q.Kind
		select {
		case <-release:
			return vespaResult{}, nil
		case <-ctx.Done():
			return vespaResult{}, ctx.Err()
		}
	}), newFakeCache(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan error, 1)
	defer func() {
		close(release)
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("retrieval did not finish")
		}
	}()
	go func() {
		_, err := s.parallelSearch(context.Background(), []vespaQuery{{Kind: retrieveKeyword}, {Kind: retrieveVector}}, false)
		done <- err
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("independent retrieval arms started sequentially")
		}
	}
}

func TestSearchDeadlineCancelsBeforeRetrievalOrFallback(t *testing.T) {
	var searches atomic.Int32
	s := newServer(testEmbedFunc(func(ctx context.Context, _ string) ([]float32, error) { <-ctx.Done(); return nil, ctx.Err() }), nil, testSearchFunc(func(context.Context, vespaQuery) (vespaResult, error) { searches.Add(1); return vespaResult{}, nil }), newFakeCache(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.searchTimeout = 25 * time.Millisecond
	started := time.Now()
	_, err := s.Search(tenantCtx(t, "alice"), &queryv1.SearchRequest{Query: "budget", DocTypes: []askerv1.DocType{askerv1.DocType_EMAIL}})
	if status.Code(err) != codes.DeadlineExceeded || time.Since(started) > time.Second || searches.Load() != 0 {
		t.Fatalf("deadline continued work: err=%v wall=%s searches=%d", err, time.Since(started), searches.Load())
	}
}
