package main

// Searcher abstracts "run a query, get back a ranked list of doc_ids". The
// harness scores whatever a Searcher returns, so the same runner works against
// the live gateway (GatewaySearcher, below) or an in-process fake (tests). This
// is the seam a future in-process bufconn searcher plugs into.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Pipeline is one retrieval configuration to evaluate. Name is the harness
// label shown in reports; APIMode is the gateway `mode` param; Extra carries
// additional query params (e.g. a future rerank toggle). Supported=false marks
// a pipeline whose backing feature is not built yet — the runner records it as
// "n/a" instead of silently aliasing it to another pipeline.
type Pipeline struct {
	Name      string     `json:"name"`
	APIMode   string     `json:"api_mode"`
	Extra     url.Values `json:"extra,omitempty"`
	Supported bool       `json:"supported"`
}

// standardPipelines are the four the prompt compares. hybrid_rerank drives the
// gateway's ?rerank=1 pass (Phase 1); it is a no-op unless the query service has
// a reranker wired (QUERY_RERANK_ENABLED), in which case the quality gate
// activates against the hybrid baseline.
func standardPipelines() []Pipeline {
	return []Pipeline{
		{Name: "lexical", APIMode: "keyword", Supported: true},
		{Name: "dense", APIMode: "vector", Supported: true},
		{Name: "hybrid", APIMode: "hybrid", Supported: true},
		{Name: "hybrid_rerank", APIMode: "hybrid", Extra: url.Values{"rerank": {"1"}}, Supported: true},
	}
}

// SearchOutcome is one query's result: the ranked doc_ids plus both latency
// views — WallMs (client round-trip, closest to user-perceived) and ServerMs
// (the query service's own took_ms).
type SearchOutcome struct {
	DocIDs          []string
	WallMs          float64
	ServerMs        int64
	Cached          bool
	Degraded        string
	RerankRequested bool
	RerankApplied   bool
	ExecutionKnown  bool           // false when the server omits execution telemetry
	PreRerankHead   *CandidateHead // nil means absent, incomplete or invalid telemetry
}

// CandidateHead is the actual authorized head supplied to the reranker, not
// the final hit list and not the complete eligible retrieval population.
type CandidateHead struct {
	DocIDs []string
	Count  int
	Depth  int
}

func decodeCandidateHead(raw json.RawMessage) *CandidateHead {
	if len(raw) == 0 || len(raw) > 4096 {
		return nil
	}
	var wire struct {
		Scope    string   `json:"scope"`
		DocIDs   []string `json:"doc_ids"`
		Count    *int     `json:"count"`
		Depth    *int     `json:"depth"`
		Complete *bool    `json:"complete"`
	}
	if json.Unmarshal(raw, &wire) != nil || wire.Scope != "pre_rerank_head" || wire.Complete == nil || !*wire.Complete ||
		wire.Count == nil || wire.Depth == nil || wire.DocIDs == nil || *wire.Depth < 0 || *wire.Count < *wire.Depth || len(wire.DocIDs) != *wire.Depth {
		return nil
	}
	if len(dedupeStable(wire.DocIDs)) != len(wire.DocIDs) {
		return nil
	}
	return &CandidateHead{DocIDs: wire.DocIDs, Count: *wire.Count, Depth: *wire.Depth}
}

// Searcher runs a single query for a tenant under a pipeline configuration.
type Searcher interface {
	Search(ctx context.Context, tenant, query string, p Pipeline, limit int) (SearchOutcome, error)
}

// TokenFunc resolves a bearer token for a tenant (dev user). The gateway
// derives the real tenant from the token's verified claims, so the golden
// "tenant" only needs to select the right token.
type TokenFunc func(tenant string) (string, error)

// GatewaySearcher hits the live gateway REST search API.
type GatewaySearcher struct {
	BaseURL     string // e.g. http://localhost:8080
	Token       TokenFunc
	Client      *http.Client
	BypassCache bool
}

// NewGatewaySearcher builds a GatewaySearcher with a sane default client.
func NewGatewaySearcher(baseURL string, token TokenFunc) *GatewaySearcher {
	return &GatewaySearcher{
		BaseURL: baseURL,
		Token:   token,
		Client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// gwHit / gwResponse mirror the gateway's pinned REST shape (services/gateway
// searchHitJSON / searchResponseJSON) — only the fields the harness needs.
type gwHit struct {
	DocID string `json:"doc_id"`
}

type gwResponse struct {
	Hits            []gwHit         `json:"hits"`
	TookMs          int64           `json:"took_ms"`
	Cached          bool            `json:"cached"`
	Degraded        string          `json:"degraded"`
	RerankRequested *bool           `json:"rerank_requested"`
	RerankApplied   *bool           `json:"rerank_applied"`
	CandidateDebug  json.RawMessage `json:"candidate_debug"`
}

// Search issues GET /v1/search and returns the ranked doc_ids.
func (g *GatewaySearcher) Search(ctx context.Context, tenant, query string, p Pipeline, limit int) (out SearchOutcome, err error) {
	tok, err := g.Token(tenant)
	if err != nil {
		return SearchOutcome{}, fmt.Errorf("token for tenant %q: %w", tenant, err)
	}

	params := url.Values{}
	params.Set("q", query)
	params.Set("debug", "1") // bounded actual pre-rerank head telemetry, when available
	if p.APIMode != "" {
		params.Set("mode", p.APIMode)
	}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	for k, vs := range p.Extra {
		for _, v := range vs {
			params.Add(k, v)
		}
	}
	endpoint := g.BaseURL + "/v1/search?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return SearchOutcome{}, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if g.BypassCache {
		req.Header.Set("Cache-Control", "no-cache")
	}

	start := time.Now()
	defer func() { out.WallMs = float64(time.Since(start).Microseconds()) / 1000 }()
	resp, err := g.Client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return SearchOutcome{}, fmt.Errorf("search transport: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil {
		return out, fmt.Errorf("search: read response: %w", err)
	}
	if len(body) > 8<<20 {
		return out, fmt.Errorf("search: response exceeds 8 MiB")
	}
	if resp.StatusCode != http.StatusOK {
		return SearchOutcome{}, fmt.Errorf("search: HTTP %d", resp.StatusCode)
	}

	var parsed gwResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return SearchOutcome{}, fmt.Errorf("search: decode response: %w", err)
	}
	ids := make([]string, 0, len(parsed.Hits))
	for _, h := range parsed.Hits {
		ids = append(ids, h.DocID)
	}
	out.DocIDs, out.ServerMs = ids, parsed.TookMs
	out.Cached, out.Degraded = parsed.Cached, parsed.Degraded
	out.ExecutionKnown = parsed.RerankRequested != nil && parsed.RerankApplied != nil
	if parsed.RerankRequested != nil {
		out.RerankRequested = *parsed.RerankRequested
	}
	if parsed.RerankApplied != nil {
		out.RerankApplied = *parsed.RerankApplied
	}
	if out.ExecutionKnown && out.RerankRequested {
		out.PreRerankHead = decodeCandidateHead(parsed.CandidateDebug)
	}
	return out, nil
}
