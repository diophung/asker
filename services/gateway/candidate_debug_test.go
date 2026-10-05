package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	"google.golang.org/grpc/metadata"
)

func TestCandidateDebugRequiresExplicitRequestAndTrustedBoundedMetadata(t *testing.T) {
	valid := `{"scope":"pre_rerank_head","doc_ids":["a","b"],"count":10,"depth":2,"complete":true}`
	for _, tc := range []struct {
		name  string
		debug bool
		body  string
		want  bool
	}{
		{"explicit", true, valid, true}, {"ordinary", false, valid, false},
		{"invalid", true, `{"scope":"unknown"}`, false},
		{"contradiction", true, `{"scope":"pre_rerank_head","doc_ids":["a"],"count":10,"depth":2,"complete":true}`, false},
		{"overflow", true, strings.Repeat(" ", 4097), false},
		{"duplicates", true, `{"scope":"pre_rerank_head","doc_ids":["a","a"],"count":2,"depth":2,"complete":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			(&deps{}).writeSearchResponse(w, &queryv1.SearchRequest{Debug: tc.debug}, &queryv1.SearchResponse{}, metadata.Pairs("x-asker-candidates-bin", tc.body))
			var response map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			_, present := response["candidate_debug"]
			if present != tc.want {
				t.Fatalf("candidate_debug present=%v want=%v", present, tc.want)
			}
		})
	}
}
