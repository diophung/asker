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

	"google.golang.org/protobuf/types/known/timestamppb"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

// retrievalKind selects the Vespa retrieval clause and ranking profile.
type retrievalKind int

const (
	// retrieveKeyword: userQuery() with the keyword profile.
	retrieveKeyword retrievalKind = iota
	// retrieveHybrid: rank(userQuery(), nearestNeighbor) with the hybrid
	// profile. Only the FIRST argument of rank() determines which documents
	// match, so the keyword terms set the match set (a rare term returns only
	// the documents containing it — precision preserved). The second argument,
	// the nearestNeighbor operator, matches nothing on its own but makes the
	// per-document vector distance available so closeness(field, embedding) in
	// the hybrid profile actually contributes to ranking. OR-ing the
	// nearestNeighbor into the match set instead would make every document
	// match (in streaming mode targetHits:100 returns the whole small group),
	// destroying keyword precision; dropping it entirely leaves closeness with
	// no distance to read, so the vector signal silently goes to zero. Pure-
	// vector recall of documents that share NO keywords is out of scope for M1
	// (RRF / dense retrieval, deferred to M5 — ADR-006).
	retrieveHybrid
	// retrieveVector: nearestNeighbor alone with the hybrid profile — there is
	// no keyword text to match on, so the vector arm IS the match set and
	// closeness ranks; nativeRank contributes 0.
	retrieveVector
	// retrieveFilterOnly: 'true' clause — empty residual text with filters
	// present matches everything in the tenant group, filtered and ranked by
	// the keyword profile.
	retrieveFilterOnly
)

// profile maps the retrieval kind to the ranking profile (vespa/README.md).
func (k retrievalKind) profile() string {
	if k == retrieveHybrid || k == retrieveVector {
		return "hybrid"
	}
	return "keyword"
}

const (
	// vespaQueryTimeout is the server-side query timeout body parameter.
	vespaQueryTimeout = "2s"
	// vespaHTTPTimeout caps the whole HTTP exchange: Vespa's own 2s budget
	// plus transport overhead.
	vespaHTTPTimeout = 5 * time.Second
	// nearestNeighborClause: targetHits is REQUIRED; in streaming mode the
	// scan is exact, so 100 is a recall floor for the vector arm, not an
	// approximation knob (vespa/README.md).
	nearestNeighborClause = "({targetHits:100}nearestNeighbor(embedding,q))"
	// maxVespaResponseBytes bounds response reads (≤100 hits of summary
	// fields fits comfortably).
	maxVespaResponseBytes = 8 << 20
	// snippetFallbackMaxRunes truncates non-highlighted fallback snippets
	// (raw chunks can be ~2048 chars).
	snippetFallbackMaxRunes = 300
)

// errInvalidFilterValue marks a filter value that cannot be rendered safely
// into YQL; the server maps it to InvalidArgument.
var errInvalidFilterValue = errors.New("invalid filter value")

// vespaQuery is one tenant-scoped retrieval. Tenant comes from the verified
// request context (tenancygrpc interceptor) — NEVER from request fields.
type vespaQuery struct {
	Tenant tenancy.TenantID
	Kind   retrievalKind
	// Text is the residual query text. It is passed ONLY via the query=
	// body parameter consumed by userQuery() — never interpolated into YQL —
	// so quotes/backslashes in user text cannot inject YQL.
	Text   string
	Vector []float32

	DocTypes    []askerv1.DocType
	From, To    time.Time
	Participant string

	Hits, Offset int32
}

// vespaResult is a parsed Vespa search response.
type vespaResult struct {
	Hits  []*queryv1.Hit
	Total int64
}

// vespaSearcher is the retrieval dependency of the server (stubbed in tests
// via httptest, but the interface also keeps the degradation ladder testable
// in isolation).
type vespaSearcher interface {
	Search(ctx context.Context, q vespaQuery) (vespaResult, error)
}

type vespaClient struct {
	searchURL string
	httpc     *http.Client
}

func newVespaClient(baseURL string) *vespaClient {
	return &vespaClient{
		searchURL: strings.TrimRight(baseURL, "/") + "/search/",
		httpc:     &http.Client{Timeout: vespaHTTPTimeout},
	}
}

// buildYQL assembles the retrieval clause plus ANDed filters. Filter values
// are rendered exclusively through yqlStringLiteral; the free-text query is
// never part of the YQL string.
//
// Participant filtering limitation: participants are indexed as tokenized
// strings ("Alice Smith <alice@example.com>"), and the {substring:true}
// annotation matches substrings of individual tokens. A single token like
// "alice" therefore matches "alice@example.com", but a multi-word value
// ("Alice Smith") is matched as tokens, not as a phrase across the original
// string — single-token filters (email or name fragment) are what works.
func buildYQL(q vespaQuery) (string, error) {
	clauses := make([]string, 0, 5)

	switch q.Kind {
	case retrieveFilterOnly:
		clauses = append(clauses, "true")
	case retrieveKeyword:
		clauses = append(clauses, "userQuery()")
	case retrieveHybrid:
		// rank(): match on userQuery() (keyword precision); the
		// nearestNeighbor second arg is rank-only, so closeness() in the
		// hybrid profile gets a real per-document distance to blend.
		clauses = append(clauses, "rank(userQuery(), "+nearestNeighborClause+")")
	case retrieveVector:
		clauses = append(clauses, nearestNeighborClause)
	}

	if len(q.DocTypes) > 0 {
		parts := make([]string, 0, len(q.DocTypes))
		for _, dt := range q.DocTypes {
			// Enum names are [A-Z_] by construction, but they are still
			// escaped like any other literal rather than trusted.
			lit, err := yqlStringLiteral(dt.String())
			if err != nil {
				return "", fmt.Errorf("type filter: %w", err)
			}
			parts = append(parts, "type contains "+lit)
		}
		clause := strings.Join(parts, " or ")
		if len(parts) > 1 {
			clause = "(" + clause + ")"
		}
		clauses = append(clauses, clause)
	}
	if !q.From.IsZero() {
		clauses = append(clauses, fmt.Sprintf("created_at >= %d", q.From.Unix()))
	}
	if !q.To.IsZero() {
		clauses = append(clauses, fmt.Sprintf("created_at <= %d", q.To.Unix()))
	}
	if q.Participant != "" {
		lit, err := yqlStringLiteral(q.Participant)
		if err != nil {
			return "", fmt.Errorf("participant filter: %w", err)
		}
		clauses = append(clauses, "participants contains ({substring:true}"+lit+")")
	}

	return "select * from sources * where " + strings.Join(clauses, " and "), nil
}

// yqlStringLiteral renders s as a double-quoted YQL string literal.
// Backslashes and double quotes are escaped so a filter value can never
// terminate the literal and inject YQL; control characters (which YQL string
// literals cannot represent raw) are rejected outright.
func yqlStringLiteral(s string) (string, error) {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%w: control character U+%04X", errInvalidFilterValue, r)
		}
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`, nil
}

// Search implements vespaSearcher: POST {VESPA_URL}/search/ with the JSON
// query API. Transport errors, client-side timeouts, and HTTP 5xx are wrapped
// as degradable (degrade.go); HTTP 4xx and response-body query errors are
// terminal.
func (c *vespaClient) Search(ctx context.Context, q vespaQuery) (vespaResult, error) {
	yql, err := buildYQL(q)
	if err != nil {
		return vespaResult{}, err
	}

	body := map[string]any{
		"yql": yql,
		// THE tenant-isolation property: every query is scoped to exactly
		// one streaming group, named by the context tenant.
		"streaming.groupname":  string(q.Tenant),
		"hits":                 q.Hits,
		"offset":               q.Offset,
		"ranking.profile":      q.Kind.profile(),
		"presentation.summary": "search",
		"timeout":              vespaQueryTimeout,
	}
	if q.Text != "" && q.Kind != retrieveVector {
		body["query"] = q.Text
	}
	if len(q.Vector) > 0 && (q.Kind == retrieveHybrid || q.Kind == retrieveVector) {
		body["input.query(q)"] = q.Vector
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return vespaResult{}, fmt.Errorf("vespa: marshal query: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.searchURL, bytes.NewReader(payload))
	if err != nil {
		return vespaResult{}, fmt.Errorf("vespa: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpc.Do(req)
	if err != nil {
		return vespaResult{}, &degradableError{err: fmt.Errorf("vespa: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxVespaResponseBytes))
	if err != nil {
		return vespaResult{}, &degradableError{err: fmt.Errorf("vespa: read response: %w", err)}
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		return vespaResult{}, &degradableError{err: fmt.Errorf("vespa: status %d: %s", resp.StatusCode, truncateForError(raw))}
	}
	if resp.StatusCode != http.StatusOK {
		return vespaResult{}, fmt.Errorf("vespa: status %d: %s", resp.StatusCode, truncateForError(raw))
	}
	return parseVespaResponse(raw)
}

// vespaSearchResponse mirrors the slice of the Vespa default JSON renderer
// the query path consumes.
type vespaSearchResponse struct {
	Root struct {
		Fields struct {
			TotalCount int64 `json:"totalCount"`
		} `json:"fields"`
		Errors []struct {
			Code    int    `json:"code"`
			Summary string `json:"summary"`
			Message string `json:"message"`
		} `json:"errors"`
		Children []struct {
			Relevance float64        `json:"relevance"`
			Fields    vespaHitFields `json:"fields"`
		} `json:"children"`
	} `json:"root"`
}

// vespaHitFields are the "search" document-summary fields (vespa/README.md):
// lean result-card fields plus the dynamic snippet/chunk_snippets fragments.
type vespaHitFields struct {
	DocID         string   `json:"doc_id"`
	ConnectorID   string   `json:"connector_id"`
	Type          string   `json:"type"`
	Title         string   `json:"title"`
	Snippet       string   `json:"snippet"`
	ChunkSnippets []string `json:"chunk_snippets"`
	MetadataJSON  string   `json:"metadata_json"`
	CreatedAt     int64    `json:"created_at"`
	ModifiedAt    int64    `json:"modified_at"`
}

func parseVespaResponse(raw []byte) (vespaResult, error) {
	var vr vespaSearchResponse
	if err := json.Unmarshal(raw, &vr); err != nil {
		return vespaResult{}, fmt.Errorf("vespa: decode response: %w", err)
	}
	// Errors alongside children are partial results: serve what we got.
	// Errors with nothing else are a failed query.
	if len(vr.Root.Errors) > 0 && len(vr.Root.Children) == 0 {
		e := vr.Root.Errors[0]
		return vespaResult{}, fmt.Errorf("vespa: query error %d (%s): %s", e.Code, e.Summary, e.Message)
	}

	out := vespaResult{Total: vr.Root.Fields.TotalCount}
	for _, child := range vr.Root.Children {
		f := child.Fields
		if f.DocID == "" {
			continue // not a document hit (grouping/aux item)
		}
		hit := &queryv1.Hit{
			DocId:       f.DocID,
			ConnectorId: f.ConnectorID,
			// Unknown enum names map to 0 = DOC_TYPE_UNSPECIFIED.
			Type:     askerv1.DocType(askerv1.DocType_value[f.Type]),
			Title:    f.Title,
			Snippet:  chooseSnippet(f),
			Score:    child.Relevance,
			Metadata: parseMetadataJSON(f.MetadataJSON),
		}
		if f.CreatedAt > 0 {
			hit.Created = timestamppb.New(time.Unix(f.CreatedAt, 0).UTC())
		}
		if f.ModifiedAt > 0 {
			hit.Modified = timestamppb.New(time.Unix(f.ModifiedAt, 0).UTC())
		}
		out.Hits = append(out.Hits, hit)
	}
	return out, nil
}

// chooseSnippet assembles the REST snippet: prefer fragments that actually
// carry <hi> highlights (the dynamic body snippet first, then any chunk
// fragment — per vespa/README.md, non-matching chunk elements come back as
// full unhighlighted text), then fall back to the raw snippet, the first
// chunk (truncated), and finally the title (e.g. vector-only matches where
// no query term occurs literally).
func chooseSnippet(f vespaHitFields) string {
	const hi = "<hi>"
	if f.Snippet != "" && strings.Contains(f.Snippet, hi) {
		return f.Snippet
	}
	for _, cs := range f.ChunkSnippets {
		if strings.Contains(cs, hi) {
			return cs
		}
	}
	if f.Snippet != "" {
		return truncateRunes(f.Snippet, snippetFallbackMaxRunes)
	}
	if len(f.ChunkSnippets) > 0 && f.ChunkSnippets[0] != "" {
		return truncateRunes(f.ChunkSnippets[0], snippetFallbackMaxRunes)
	}
	return f.Title
}

func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// parseMetadataJSON converts the metadata_json field (a JSON object rendered
// as a string by the index writer) into the Hit metadata map. Non-string
// values are re-encoded as compact JSON; a malformed document yields an empty
// map rather than a failed search.
func parseMetadataJSON(s string) map[string]string {
	if s == "" {
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil
	}
	m := make(map[string]string, len(raw))
	for k, v := range raw {
		if sv, ok := v.(string); ok {
			m[k] = sv
			continue
		}
		if b, err := json.Marshal(v); err == nil {
			m[k] = string(b)
		}
	}
	return m
}

func truncateForError(raw []byte) string {
	const max = 512
	s := strings.TrimSpace(string(raw))
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}
