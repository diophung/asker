package main

import (
	"encoding/json"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
)

const maxCandidateDebugBytes = 4096

// Candidate recall must be scored against the actual bounded model input, not
// the final response or the unscored retrieval tail. Binary gRPC metadata keeps
// arbitrary Unicode document IDs valid; the gateway projects it only for debug.
type candidateDebugPayload struct {
	Scope    string   `json:"scope"`
	DocIDs   []string `json:"doc_ids"`
	Count    int      `json:"count"`
	Depth    int      `json:"depth"`
	Complete bool     `json:"complete"`
}

func encodeCandidateDebug(hits []*queryv1.Hit, maxCand int) []byte {
	depth := len(hits)
	if maxCand > 0 {
		depth = min(depth, maxCand)
	}
	payload := candidateDebugPayload{Scope: "pre_rerank_head", DocIDs: make([]string, depth), Count: len(hits), Depth: depth, Complete: true}
	for i := range depth {
		payload.DocIDs[i] = hits[i].GetDocId()
	}
	for {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil
		}
		if len(data) <= maxCandidateDebugBytes {
			return data
		}
		payload.Complete = false
		payload.DocIDs = payload.DocIDs[:len(payload.DocIDs)-1]
	}
}
