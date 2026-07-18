package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// reranker scores a batch of candidate documents against the query with a
// cross-encoder, returning one relevance score per document IN INPUT ORDER
// (higher = more relevant). It is a separate dependency from the embedders:
// it does not produce vectors, it produces a (query, doc) relevance used to
// reorder the top fused candidates (Phase 1). nil on the server disables it.
type reranker interface {
	Rerank(ctx context.Context, query string, documents []string) ([]float64, error)
}

// errRerankShape marks a reranker response that does not line up with the
// request (wrong number of scores) — a contract violation, not a transient
// failure. Surfaced so the pipeline can degrade (drop the rerank pass) loudly.
var errRerankShape = errors.New("reranker response shape mismatch")

// rerankHTTPClient calls the reranker service's POST /rerank:
//
//	{"query": "...", "documents": ["...", ...]}
//	  -> {"scores": [float, ...]}   // one per document, input order
//
// The service (services/reranker) serves bge-reranker-v2-m3 by default. Vectors
// are not involved; only a scalar relevance per pair.
type rerankHTTPClient struct {
	rerankURL string
	timeout   time.Duration
	httpc     *http.Client
}

func newReranker(baseURL string, timeout time.Duration) *rerankHTTPClient {
	return &rerankHTTPClient{
		rerankURL: strings.TrimRight(baseURL, "/") + "/rerank",
		timeout:   timeout,
		httpc:     &http.Client{},
	}
}

// Rerank implements reranker.
func (c *rerankHTTPClient) Rerank(ctx context.Context, query string, documents []string) ([]float64, error) {
	if len(documents) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	payload, err := json.Marshal(struct {
		Query     string   `json:"query"`
		Documents []string `json:"documents"`
	}{Query: query, Documents: documents})
	if err != nil {
		return nil, fmt.Errorf("reranker: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.rerankURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("reranker: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reranker: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("reranker: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	var body struct {
		Scores []float64 `json:"scores"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("reranker: decode response: %w", err)
	}
	if len(body.Scores) != len(documents) {
		return nil, fmt.Errorf("%w: got %d scores for %d documents", errRerankShape, len(body.Scores), len(documents))
	}
	return body.Scores, nil
}
