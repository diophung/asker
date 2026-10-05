package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestGatewaySearcherParsesResponse(t *testing.T) {
	var gotQuery url.Values
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hits":[{"doc_id":"d1"},{"doc_id":"d2"}],"total":2,"took_ms":37,"cached":false}`))
	}))
	defer srv.Close()

	s := NewGatewaySearcher(srv.URL, func(string) (string, error) { return "tok123", nil })
	p := Pipeline{Name: "hybrid_rerank", APIMode: "hybrid", Extra: url.Values{"rerank": {"1"}}}
	out, err := s.Search(context.Background(), "alice", "budget review", p, 20)
	if err != nil {
		t.Fatal(err)
	}

	if len(out.DocIDs) != 2 || out.DocIDs[0] != "d1" || out.DocIDs[1] != "d2" {
		t.Fatalf("docIDs=%v want [d1 d2]", out.DocIDs)
	}
	if out.ServerMs != 37 {
		t.Fatalf("serverMs=%d want 37", out.ServerMs)
	}
	if gotPath != "/v1/search" {
		t.Fatalf("path=%q want /v1/search", gotPath)
	}
	if gotAuth != "Bearer tok123" {
		t.Fatalf("auth=%q want Bearer tok123", gotAuth)
	}
	if gotQuery.Get("q") != "budget review" || gotQuery.Get("mode") != "hybrid" ||
		gotQuery.Get("limit") != "20" || gotQuery.Get("rerank") != "1" || gotQuery.Get("debug") != "1" {
		t.Fatalf("query params wrong: %v", gotQuery)
	}
}

func TestCandidateHeadRequiresCompleteValidTelemetry(t *testing.T) {
	valid := []string{
		`{"scope":"pre_rerank_head","doc_ids":["a","b"],"count":80,"depth":2,"complete":true}`,
		`{"scope":"pre_rerank_head","doc_ids":[],"count":0,"depth":0,"complete":true}`,
	}
	for _, raw := range valid {
		if decodeCandidateHead([]byte(raw)) == nil {
			t.Fatal("complete head IDs must be measurable")
		}
	}
	invalid := []string{
		`null`, `{}`, `"not an object"`,
		`{"scope":"final_hits","doc_ids":["a"],"count":1,"depth":1,"complete":true}`,
		`{"scope":"pre_rerank_head","doc_ids":["a"],"count":80,"depth":2,"complete":false}`,
		`{"scope":"pre_rerank_head","doc_ids":["a"],"count":80,"depth":2,"complete":true}`,
		`{"scope":"pre_rerank_head","doc_ids":["a","a"],"count":2,"depth":2,"complete":true}`,
		`{"scope":"pre_rerank_head","doc_ids":[""],"count":1,"depth":1,"complete":true}`,
		`{"scope":"pre_rerank_head","doc_ids":null,"count":0,"depth":0,"complete":true}`,
		`{"scope":"pre_rerank_head","doc_ids":[],"count":0,"complete":true}`,
		`{"scope":"pre_rerank_head","doc_ids":[],"depth":0,"complete":true}`,
		`{"scope":"pre_rerank_head","doc_ids":["a"],"count":0,"depth":1,"complete":true}`,
		`{"scope":"pre_rerank_head","doc_ids":[],"count":0,"depth":-1,"complete":true}`,
		`{"scope":"pre_rerank_head","doc_ids":[],"count":0,"depth":"0","complete":true}`,
		strings.Repeat(" ", 4097),
	}
	for _, raw := range invalid {
		if decodeCandidateHead([]byte(raw)) != nil {
			t.Fatal("missing, partial or malformed telemetry must remain unmeasured")
		}
	}
}

func TestCandidateTelemetryDoesNotReplaceFinalHitsOrFailValidSearchDecode(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "malformed"}[malformed], func(t *testing.T) {
			debug := `{"scope":"pre_rerank_head","doc_ids":["a","b"],"count":20,"depth":2,"complete":true}`
			if malformed {
				debug = `{"scope":"pre_rerank_head","doc_ids":"invalid","depth":"unknown"}`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("debug") != "1" {
					t.Error("evaluation must explicitly request bounded candidate telemetry")
				}
				_, _ = w.Write([]byte(`{"hits":[{"doc_id":"a"}],"rerank_requested":true,"rerank_applied":true,"candidate_debug":` + debug + `}`))
			}))
			defer server.Close()
			searcher := NewGatewaySearcher(server.URL, func(string) (string, error) { return "token", nil })
			out, err := searcher.Search(context.Background(), "tenant", "q", Pipeline{}, 10)
			if err != nil || len(out.DocIDs) != 1 || out.DocIDs[0] != "a" {
				t.Fatal("candidate telemetry must not corrupt the normal result boundary")
			}
			if malformed && out.PreRerankHead != nil {
				t.Fatal("malformed telemetry is unmeasured")
			}
			if !malformed && (out.PreRerankHead == nil || out.PreRerankHead.Depth != 2) {
				t.Fatal("complete head telemetry was lost")
			}
		})
	}
}

func TestGatewaySearcherNon200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"missing token"}`))
	}))
	defer srv.Close()

	s := NewGatewaySearcher(srv.URL, func(string) (string, error) { return "tok", nil })
	if _, err := s.Search(context.Background(), "alice", "q", Pipeline{APIMode: "hybrid"}, 10); err == nil {
		t.Fatalf("expected error on HTTP 401")
	}
}

func TestGatewaySearcherTimesCompleteBodyAndRequestsCacheBypass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cache-Control") != "no-cache" {
			t.Error("uncached benchmark must request cache bypass")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(80 * time.Millisecond)
		_, _ = w.Write([]byte(`{"hits":[{"doc_id":"a"}],"cached":true,"degraded":"embed_timeout","rerank_requested":true,"rerank_applied":false}`))
	}))
	defer srv.Close()
	s := NewGatewaySearcher(srv.URL, func(string) (string, error) { return "tok", nil })
	s.BypassCache = true
	out, err := s.Search(context.Background(), "alice", "q", Pipeline{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if out.WallMs < 75 {
		t.Fatalf("timer stopped before response body finished: %v ms", out.WallMs)
	}
	if !out.Cached || out.Degraded != "embed_timeout" || !out.ExecutionKnown || !out.RerankRequested || out.RerankApplied {
		t.Fatalf("execution telemetry lost: %+v", out)
	}
}

func TestGatewaySearcherBodyReadFailureIsMeasuredError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write([]byte(`{"hits":[]}`))
	}))
	defer srv.Close()
	s := NewGatewaySearcher(srv.URL, func(string) (string, error) { return "tok", nil })
	out, err := s.Search(context.Background(), "alice", "q", Pipeline{}, 10)
	if err == nil || !strings.Contains(err.Error(), "read response") {
		t.Fatalf("body truncation must be an error: %v", err)
	}
	if out.WallMs <= 0 {
		t.Fatalf("failed requests must retain complete elapsed time: %+v", out)
	}
}
