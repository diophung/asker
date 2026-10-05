package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/asker/asker/platform/personalization"
	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

func TestContentSearchDoesNotBorrowCalendarUrgency(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	profile := personalization.DefaultProfile()
	profile.RecencyVsImportance = 0
	file := &queryv1.Hit{DocId: "answer", Type: askerv1.DocType_FILE, Title: "Recover damaged records", Score: .9}
	event := &queryv1.Hit{DocId: "event", Type: askerv1.DocType_CALENDAR_EVENT, Title: "Planning review", Score: .85,
		Metadata: map[string]string{"start": now.Add(time.Hour).Format(time.RFC3339)}}
	params := personalizeParams{profile: profile, now: now, halfLife: defaultPersonalizationHalfLife, limit: 20, debug: true}
	got, _ := personalizeRank([]*queryv1.Hit{file, event, {DocId: "floor", Score: 0}}, params)
	if got[0].GetDocId() != "answer" || got[1].GetFeatures()["attention"] != 0 {
		t.Fatalf("content ranking borrowed urgency: %v", got)
	}
	params.intent = intentNeedsAttention
	attention, _ := personalizeRank([]*queryv1.Hit{file, event}, params)
	if len(attention) != 1 || attention[0].GetDocId() != "event" || attention[0].GetFeatures()["attention"] <= 0 {
		t.Fatalf("explicit attention intent lost salience: %v", attention)
	}
}

func TestMMRPreservesDistinctSameSourceAnswers(t *testing.T) {
	scored := []scoredHit{
		{hit: &queryv1.Hit{DocId: "runbook", Title: "Restore corrupted database backups"}, docType: "FILE", sender: "alice", relNorm: 1},
		{hit: &queryv1.Hit{DocId: "approval", Title: "Release authorization signature"}, docType: "FILE", sender: "alice", relNorm: .96},
		{hit: &queryv1.Hit{DocId: "unrelated", Title: "Team coffee gathering"}, docType: "CALENDAR_EVENT", sender: "bob", relNorm: .9},
	}
	got := mmrReorder(context.Background(), scored, .3)
	if got[0].hit.DocId != "runbook" || got[1].hit.DocId != "approval" {
		t.Fatalf("distinct same-source answers penalized: %v/%v/%v", got[0].hit.DocId, got[1].hit.DocId, got[2].hit.DocId)
	}
}

func TestMMRStillDiversifiesRedundantPassages(t *testing.T) {
	scored := []scoredHit{
		{hit: &queryv1.Hit{DocId: "first", Title: "Restore corrupted database backups"}, docType: "FILE", sender: "alice", relNorm: 1},
		{hit: &queryv1.Hit{DocId: "copy", Title: "Restore corrupted database backups"}, docType: "FILE", sender: "alice", relNorm: .99},
		{hit: &queryv1.Hit{DocId: "distinct", Title: "Release authorization signature"}, docType: "FILE", sender: "alice", relNorm: .95},
	}
	got := mmrReorder(context.Background(), scored, .3)
	if got[0].hit.DocId != "first" || got[1].hit.DocId != "distinct" || got[2].hit.DocId != "copy" {
		t.Fatalf("near-duplicate was not diversified: %v/%v/%v", got[0].hit.DocId, got[1].hit.DocId, got[2].hit.DocId)
	}
	if sim := tokenJaccard(redundancyTokens("Recuperación de datos"), redundancyTokens("RECUPERACIÓN DE DATOS")); sim != 1 {
		t.Fatalf("Unicode redundancy is not case insensitive: %v", sim)
	}
}

func TestMMRCanceledContextStopsRanking(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	scored := []scoredHit{{hit: &queryv1.Hit{DocId: "first"}}, {hit: &queryv1.Hit{DocId: "second"}}}
	got := mmrReorder(ctx, scored, .5)
	if len(got) != 2 || got[0].hit.DocId != "first" {
		t.Fatalf("canceled ranking changed candidate population: %v", got)
	}
}

func TestRerankReceivesRichPassageWhilePublicSnippetStaysShort(t *testing.T) {
	passage := strings.Repeat("overview ", 45) + "recover damaged database"
	rr := &substringReranker{weights: map[string]float64{"recover damaged": 10}}
	env := newQueryEnv(t, withRRF(), withReranker(rr))
	fixture := func(text string) string {
		t.Helper()
		data, err := json.Marshal(map[string]any{"root": map[string]any{
			"fields": map[string]int{"totalCount": 2},
			"children": []map[string]any{
				{"relevance": .9, "fields": map[string]any{"doc_id": "overview", "type": "FILE", "title": "Overview", "snippet": "Overview document"}},
				{"relevance": .8, "fields": map[string]any{"doc_id": "recovery", "type": "FILE", "title": "Recovery instructions", "snippet": text}},
			},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	// The keyword representative has a short teaser, while the authorized
	// dense arm carries the useful context beyond the UI's 300-rune cap.
	env.vespa.setProfileFixture("keyword", fixture("overview"))
	env.vespa.setProfileFixture("hybrid", fixture(passage))
	resp, err := env.client.Search(tenantCtx(t, "alice"), &queryv1.SearchRequest{Query: "restore records", Rerank: true, DocTypes: []askerv1.DocType{askerv1.DocType_FILE}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 2 || resp.Hits[0].DocId != "recovery" {
		t.Fatalf("context beyond teaser was lost before rerank: %v", hitIDs(resp))
	}
	if resp.Hits[0].Snippet != "overview" || strings.Contains(resp.Hits[0].Snippet, "recover damaged") {
		t.Fatalf("internal model passage leaked into public snippet: %q", resp.Hits[0].Snippet)
	}
	if len(rr.gotDocs) != 2 || !strings.Contains(rr.gotDocs[1], "recover damaged") || len([]rune(rr.gotDocs[1])) > 1025 {
		t.Fatalf("rerank passage missing or unbounded: %v", rr.gotDocs)
	}
}

func TestChooseRerankPassageIsBoundedAndDeduplicated(t *testing.T) {
	got := chooseRerankPassage(vespaHitFields{Snippet: "<hi>restore</hi> records", ChunkSnippets: []string{"restore records", "backup procedure"}})
	if got != "restore records\nbackup procedure" {
		t.Fatalf("passage duplicated fragments or highlights: %q", got)
	}
	got = chooseRerankPassage(vespaHitFields{Snippet: strings.Repeat("文", maxRerankPassageRunes+100)})
	if len([]rune(got)) > maxRerankPassageRunes+1 {
		t.Fatalf("unbounded internal passage: %d runes", len([]rune(got)))
	}
}

func TestRRFCountsEachDocumentOncePerArm(t *testing.T) {
	got := rrfFuse([]*queryv1.Hit{rrfHit("a"), rrfHit("a"), rrfHit("b")}, []*queryv1.Hit{rrfHit("b")})
	if len(got) != 2 || got[0].DocId != "b" {
		t.Fatalf("duplicate within one arm fabricated consensus: %v", hitIDsOf(got))
	}
	if got[1].Score != 1.0/float64(rrfK+1) {
		t.Fatalf("duplicate document contributed twice: %v", got[1].Score)
	}
}

func TestDisabledResultCacheSkipsLookupAndInsertion(t *testing.T) {
	cache := newFakeCache()
	searches := 0
	s := newServer(nil, nil, testSearchFunc(func(context.Context, vespaQuery) (vespaResult, error) {
		searches++
		return vespaResult{Hits: []*queryv1.Hit{{DocId: "fresh"}}, Total: 1}, nil
	}), cache, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.resultCacheEnabled = false
	request := &queryv1.SearchRequest{Query: "budget", Mode: queryv1.SearchMode_KEYWORD}
	for range 2 {
		response, err := s.Search(tenantCtx(t, "alice"), request)
		if err != nil || response.Cached {
			t.Fatalf("disabled cache returned stale/cache response: %+v, %v", response, err)
		}
	}
	gets, sets := cache.counts()
	if gets != 0 || sets != 0 || searches != 2 {
		t.Fatalf("disabled result cache activity gets=%d sets=%d searches=%d", gets, sets, searches)
	}
}

func TestResultCacheConfigDefaultsAndOptOut(t *testing.T) {
	t.Setenv("QUERY_RESULT_CACHE_ENABLED", "")
	cfg, err := loadConfig()
	if err != nil || !cfg.ResultCacheEnabled {
		t.Fatalf("existing deployment default changed: %+v, %v", cfg, err)
	}
	t.Setenv("QUERY_RESULT_CACHE_ENABLED", "false")
	cfg, err = loadConfig()
	if err != nil || cfg.ResultCacheEnabled {
		t.Fatalf("result cache opt-out not loaded: %+v, %v", cfg, err)
	}
}
