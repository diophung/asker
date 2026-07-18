package main

// Searcher abstracts "run a query, get back a ranked list of doc_ids". The
// harness scores whatever a Searcher returns, so the same runner works against
// the live gateway (GatewaySearcher, below) or an in-process fake (tests). This
// is the seam a future in-process bufconn searcher plugs into.

import (
	"context"
	"encoding/json"
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
	Name      string
	APIMode   string
	Extra     url.Values
	Supported bool
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
	DocIDs   []string
	WallMs   float64
	ServerMs int64
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
	BaseURL string // e.g. http://localhost:8080
	Token   TokenFunc
	Client  *http.Client
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
	Hits   []gwHit `json:"hits"`
	TookMs int64   `json:"took_ms"`
}

// Search issues GET /v1/search and returns the ranked doc_ids.
func (g *GatewaySearcher) Search(ctx context.Context, tenant, query string, p Pipeline, limit int) (SearchOutcome, error) {
	tok, err := g.Token(tenant)
	if err != nil {
		return SearchOutcome{}, fmt.Errorf("token for tenant %q: %w", tenant, err)
	}

	params := url.Values{}
	params.Set("q", query)
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

	start := time.Now()
	resp, err := g.Client.Do(req)
	if err != nil {
		return SearchOutcome{}, fmt.Errorf("search %q: %w", query, err)
	}
	defer func() { _ = resp.Body.Close() }()
	wallMs := float64(time.Since(start).Microseconds()) / 1000.0

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return SearchOutcome{}, fmt.Errorf("search %q: HTTP %d: %s", query, resp.StatusCode, truncate(string(body), 200))
	}

	var parsed gwResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return SearchOutcome{}, fmt.Errorf("search %q: decode response: %w", query, err)
	}
	ids := make([]string, 0, len(parsed.Hits))
	for _, h := range parsed.Hits {
		ids = append(ids, h.DocID)
	}
	return SearchOutcome{DocIDs: ids, WallMs: wallMs, ServerMs: parsed.TookMs}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
