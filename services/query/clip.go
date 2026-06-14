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

// clipEmbedder turns the residual query text into a CLIP-space query vector
// for the text->image retrieval arm (ADR-013). It is a separate dependency
// from the bge-m3 embedder: the two vector spaces are distinct, queried
// against different Vespa tensors.
type clipEmbedder interface {
	EmbedText(ctx context.Context, text string) ([]float32, error)
}

// errClipDim marks a CLIP_DIM mismatch between the configured dimension and
// what the clip service actually returned — an operator error (the env var
// must match the served CLIP model and the deployed Vespa clip_embedding
// tensor), analogous to errEmbedDim for bge-m3 (ADR-005/ADR-013).
var errClipDim = errors.New("clip embedding dimension mismatch")

// clipHTTPEmbedder calls the clip service's POST /embed/text:
// {"inputs":[...texts]} -> {"embeddings":[[float,...],...]} (ADR-013). One
// query is a single-input batch. The image encoder lives on the same loaded
// model, so the text vector shares the image space; vectors are L2-normalized
// server-side, matching the Vespa clip_embedding 'angular' metric.
type clipHTTPEmbedder struct {
	embedURL string
	dim      int
	timeout  time.Duration
	httpc    *http.Client
}

func newClipEmbedder(baseURL string, dim int, timeout time.Duration) *clipHTTPEmbedder {
	return &clipHTTPEmbedder{
		embedURL: strings.TrimRight(baseURL, "/") + "/embed/text",
		dim:      dim,
		timeout:  timeout,
		httpc:    &http.Client{},
	}
}

// EmbedText implements clipEmbedder.
func (e *clipHTTPEmbedder) EmbedText(ctx context.Context, text string) ([]float32, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	payload, err := json.Marshal(struct {
		Inputs []string `json:"inputs"`
	}{Inputs: []string{text}})
	if err != nil {
		return nil, fmt.Errorf("clip: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.embedURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("clip: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("clip: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("clip: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	var body struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("clip: decode response: %w", err)
	}
	if len(body.Embeddings) != 1 {
		return nil, fmt.Errorf("clip: got %d embeddings for 1 input", len(body.Embeddings))
	}
	if len(body.Embeddings[0]) != e.dim {
		return nil, fmt.Errorf("%w: clip returned %d values, CLIP_DIM=%d (ADR-013: CLIP_DIM must match the served model)",
			errClipDim, len(body.Embeddings[0]), e.dim)
	}
	return body.Embeddings[0], nil
}
