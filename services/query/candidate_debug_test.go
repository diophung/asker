package main

import (
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/asker/asker/platform/personalization"
	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

func TestCandidateDebugDescribesActualEligibleRerankHead(t *testing.T) {
	loader := newFakeLoader()
	loader.set("alice", personalization.Profile{Mute: personalization.MuteList{Sources: []string{"CHAT_MESSAGE"}}})
	rr := &fakeReranker{}
	env := newQueryEnv(t, withProfiles(loader, false), withReranker(rr))
	env.vespa.setProfileFixture("hybrid", buildFixture(t, []docSpec{
		{id: "muted", typ: "CHAT_MESSAGE", title: "muted instructions", relevance: 1},
		{id: "answer-文", typ: "FILE", title: "recover data", relevance: .9},
		{id: "other", typ: "FILE", title: "release approval", relevance: .8},
	}))
	var header metadata.MD
	request := &queryv1.SearchRequest{Query: "restore records", Rerank: true, Debug: true, DocTypes: []askerv1.DocType{askerv1.DocType_FILE}}
	_, err := env.client.Search(tenantCtx(t, "alice"), request, grpc.Header(&header))
	if err != nil {
		t.Fatal(err)
	}
	values := header.Get("x-asker-candidates-bin")
	if len(values) != 1 {
		t.Fatalf("candidate debug absent: %v", header)
	}
	var got candidateDebugPayload
	if err := json.Unmarshal([]byte(values[0]), &got); err != nil {
		t.Fatal(err)
	}
	if got.Scope != "pre_rerank_head" || got.Count != 2 || got.Depth != 2 || !got.Complete || strings.Join(got.DocIDs, ",") != "answer-文,other" {
		t.Fatalf("candidate scope/depth did not match model input: %+v", got)
	}
	if len(rr.gotDocs) != 2 || strings.Contains(strings.Join(rr.gotDocs, " "), "muted") {
		t.Fatalf("hard-filtered document reached inference: %v", rr.gotDocs)
	}
	// A cached response did not execute candidate retrieval in this request.
	header = nil
	response, err := env.client.Search(tenantCtx(t, "alice"), request, grpc.Header(&header))
	if err != nil || !response.Cached || len(header.Get("x-asker-candidates-bin")) != 0 {
		t.Fatalf("cache hit advertised fresh candidates: %+v, %v, %v", response, header, err)
	}
}

func TestCandidateDebugIsAbsentWithoutDebugRequest(t *testing.T) {
	env := newQueryEnv(t, withReranker(&fakeReranker{}))
	var header metadata.MD
	_, err := env.client.Search(tenantCtx(t, "alice"), &queryv1.SearchRequest{Query: "budget", Rerank: true}, grpc.Header(&header))
	if err != nil || len(header.Get("x-asker-candidates-bin")) != 0 {
		t.Fatalf("ordinary request exposed debug candidates: %v, %v", header, err)
	}
}

func TestCandidateDebugBoundsIDsAndPreservesPopulationCounts(t *testing.T) {
	hits := make([]*queryv1.Hit, 100)
	for i := range hits {
		hits[i] = &queryv1.Hit{DocId: strings.Repeat("文", 100)}
	}
	data := encodeCandidateDebug(hits, 30)
	var got candidateDebugPayload
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(data) > maxCandidateDebugBytes || got.Count != 100 || got.Depth != 30 || got.Complete || len(got.DocIDs) >= got.Depth {
		t.Fatalf("truncated telemetry claimed completeness: bytes=%d %+v", len(data), got)
	}
	if err := json.Unmarshal(encodeCandidateDebug(nil, 30), &got); err != nil || !got.Complete || got.Count != 0 || got.Depth != 0 || len(got.DocIDs) != 0 {
		t.Fatalf("clean empty head not represented honestly: %+v, %v", got, err)
	}
}
