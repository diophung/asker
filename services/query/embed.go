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

// embedder turns the residual query text into a query vector.
type embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// errEmbedDim marks an EMBEDDING_DIM mismatch between the configured
// dimension and what TEI actually returned — an operator error per ADR-005
// (the env var must match the served model and the deployed Vespa schema).
// It is logged loudly even when the search itself degrades to keyword-only.
var errEmbedDim = errors.New("embedding dimension mismatch")

// teiEmbedder calls TEI's POST /embed: {"inputs":[...texts]} ->
// [[float,...],...]. One query is a single-input batch, far under the
// server's max-batch-tokens 4096 / 32 concurrent limits.
type teiEmbedder struct {
	embedURL string
	dim      int
	timeout  time.Duration
	httpc    *http.Client
}

func newTEIEmbedder(baseURL string, dim int, timeout time.Duration) *teiEmbedder {
	return &teiEmbedder{
		embedURL: strings.TrimRight(baseURL, "/") + "/embed",
		dim:      dim,
		timeout:  timeout,
		// The per-call context carries the timeout so a caller deadline that
		// is even tighter still wins.
		httpc: &http.Client{},
	}
}

// Embed implements embedder.
func (e *teiEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	payload, err := json.Marshal(struct {
		Inputs []string `json:"inputs"`
	}{Inputs: []string{text}})
	if err != nil {
		return nil, fmt.Errorf("tei: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.embedURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("tei: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tei: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("tei: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	var vectors [][]float32
	if err := json.NewDecoder(resp.Body).Decode(&vectors); err != nil {
		return nil, fmt.Errorf("tei: decode response: %w", err)
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("tei: got %d vectors for 1 input", len(vectors))
	}
	if len(vectors[0]) != e.dim {
		return nil, fmt.Errorf("%w: TEI returned %d values, EMBEDDING_DIM=%d (ADR-005: EMBEDDING_DIM must match the served model)",
			errEmbedDim, len(vectors[0]), e.dim)
	}
	return vectors[0], nil
}
