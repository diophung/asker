package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	documentv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// searchHitJSON is the pinned REST hit shape (web/src/api.ts Hit). Field
// names and presence are contractual — every key is always emitted.
type searchHitJSON struct {
	DocID       string `json:"doc_id"`
	ConnectorID string `json:"connector_id"`
	Type        string `json:"type"`
	Title       string `json:"title"`
	Snippet     string `json:"snippet"`
	// Personalized relevance results expose combined relevance or the selected
	// MMR utility, in descending order. Schedule lookups retain chronological
	// order. Earlier scoring stages are available in features when debug=1.
	Score    float64           `json:"score"`
	Created  string            `json:"created"`
	Modified string            `json:"modified"`
	Metadata map[string]string `json:"metadata"`
	// SourceURL is a browser-openable link to the original item at its source
	// (the Gmail message in Gmail, the Drive file, the Slack permalink, ...),
	// derived from the connector metadata. "" when the source has no web URL
	// (e.g. uploaded files). Always emitted (pinned shape).
	SourceURL string `json:"source_url"`
	// Media fields (M3): set for IMAGE/AUDIO/VIDEO hits; zero/empty otherwise.
	StartMs      int64  `json:"start_ms"`
	EndMs        int64  `json:"end_ms"`
	Modality     string `json:"modality"`
	ThumbnailKey string `json:"thumbnail_key"`
	// Explanation is the v3 "why this ranked" reason (empty when personalization
	// is not active). Features carries per-term contributions when ?debug=1.
	Explanation string             `json:"explanation"`
	Features    map[string]float64 `json:"features,omitempty"`
}

// searchResponseJSON is the pinned REST response shape (web/src/api.ts
// SearchResponse).
type searchResponseJSON struct {
	Hits            []searchHitJSON     `json:"hits"`
	Total           int64               `json:"total"`
	Degraded        string              `json:"degraded"`
	TookMs          int64               `json:"took_ms"`
	Cached          bool                `json:"cached"`
	RerankRequested bool                `json:"rerank_requested"`
	RerankApplied   bool                `json:"rerank_applied"`
	CandidateDebug  *candidateDebugJSON `json:"candidate_debug,omitempty"`
}

// Candidate IDs are authorized upstream evidence for explicitly requested
// diagnostics. Missing or truncated telemetry never proves candidate recall.
type candidateDebugJSON struct {
	Scope    string   `json:"scope"`
	DocIDs   []string `json:"doc_ids"`
	Count    int      `json:"count"`
	Depth    int      `json:"depth"`
	Complete bool     `json:"complete"`
}

func candidateDebug(headers metadata.MD) *candidateDebugJSON {
	values := headers.Get("x-asker-candidates-bin")
	if len(values) != 1 || len(values[0]) > 4096 {
		return nil
	}
	var value candidateDebugJSON
	if json.Unmarshal([]byte(values[0]), &value) != nil ||
		(value.Scope != "pre_rerank_head" && value.Scope != "retrieval_candidates") ||
		value.Count < 0 || value.Depth < 0 || value.Depth > value.Count ||
		len(value.DocIDs) > value.Depth || (value.Complete && len(value.DocIDs) != value.Depth) {
		return nil
	}
	seen := make(map[string]bool, len(value.DocIDs))
	for _, id := range value.DocIDs {
		if id == "" || len(id) > 1024 || seen[id] {
			return nil
		}
		seen[id] = true
	}
	return &value
}

// handleSearch proxies GET /v1/search (the "All" tab) to the QueryService. The
// tenant is NOT handled here: it rides the request context into the gRPC client
// interceptor, which fails closed without one.
func (d *deps) handleSearch(w http.ResponseWriter, r *http.Request) {
	req, err := parseSearchRequest(r.URL.Query(), d.maxQueryChars)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	resp, headers, err := d.searchRPC(r, req)
	if err != nil {
		d.upstreamError(w, r, "QueryService.Search", err)
		return
	}
	d.recordRecent(r.Context(), req.GetQuery())
	d.writeSearchResponse(w, req, resp, headers)
}

// sourceDocTypes maps a /v1/search/{source} path segment to the DocType filter
// that backs that source tab (mirrors the web SOURCE_TYPES). The "all" tab is
// the unfiltered /v1/search route; "people" is special-cased to the aggregation
// handler (people.go) and is intentionally absent here.
var sourceDocTypes = map[string][]documentv1.DocType{
	"email":    {documentv1.DocType_EMAIL},
	"files":    {documentv1.DocType_FILE, documentv1.DocType_WIKI_PAGE, documentv1.DocType_TICKET},
	"messages": {documentv1.DocType_CHAT_MESSAGE},
	"calendar": {documentv1.DocType_CALENDAR_EVENT},
	"photos":   {documentv1.DocType_IMAGE, documentv1.DocType_VIDEO, documentv1.DocType_AUDIO},
}

// handleSourceSearch serves GET /v1/search/{source}: a per-tab search endpoint.
// The path segment IS the type filter, so each tab is genuinely served by its
// own endpoint rather than a client-side filter (the v2 UI navigates here on a
// full page load). The tenant still rides the request context as for /v1/search.
func (d *deps) handleSourceSearch(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("source")
	if source == "people" {
		d.handlePeopleSearch(w, r)
		return
	}
	types, ok := sourceDocTypes[source]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown source"})
		return
	}
	req, err := parseSearchRequest(r.URL.Query(), d.maxQueryChars)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// The endpoint defines the type filter; any types= param is overridden so a
	// tab's results can never be widened past its source.
	req.DocTypes = types
	resp, headers, err := d.searchRPC(r, req)
	if err != nil {
		d.upstreamError(w, r, "QueryService.Search", err)
		return
	}
	d.recordRecent(r.Context(), req.GetQuery())
	d.writeSearchResponse(w, req, resp, headers)
}

// Search cache control is separate from tenant identity. The gateway never
// copies client-supplied tenant metadata; only this harmless cache hint is
// forwarded, while tenancygrpc still derives identity from verified claims.
func (d *deps) searchRPC(r *http.Request, req *queryv1.SearchRequest) (*queryv1.SearchResponse, metadata.MD, error) {
	ctx := r.Context()
	for _, header := range r.Header.Values("Cache-Control") {
		for _, directive := range strings.Split(header, ",") {
			name := strings.TrimSpace(strings.SplitN(directive, "=", 2)[0])
			if strings.EqualFold(name, "no-cache") || strings.EqualFold(name, "no-store") {
				ctx = metadata.AppendToOutgoingContext(ctx, "x-asker-cache-bypass", "true")
			}
		}
	}
	var headers metadata.MD
	resp, err := d.query.Search(ctx, req, grpc.Header(&headers))
	return resp, headers, err
}

func (d *deps) writeSearchResponse(w http.ResponseWriter, req *queryv1.SearchRequest, resp *queryv1.SearchResponse, headers metadata.MD) {
	out := restSearchResponse(resp)
	out.RerankRequested = req.GetRerank()
	for _, value := range headers.Get("x-asker-rerank-applied") {
		out.RerankApplied = out.RerankApplied || value == "true"
	}
	if req.GetDebug() {
		out.CandidateDebug = candidateDebug(headers)
	}
	// Search responses contain private data and must not enter shared HTTP caches.
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, out)
}

// parseSearchRequest validates and maps the /v1/search query parameters onto
// the QueryService request. Any malformed parameter is a 400. The query text is
// length-capped (maxQueryChars; <= 0 disables) so an oversized q= cannot drive
// disproportionate downstream tokenization/embedding work (M6 DoS hardening).
func parseSearchRequest(q url.Values, maxQueryChars int) (*queryv1.SearchRequest, error) {
	query := q.Get("q")
	if maxQueryChars > 0 && len([]rune(query)) > maxQueryChars {
		return nil, fmt.Errorf("query too long: %d characters exceeds the %d limit", len([]rune(query)), maxQueryChars)
	}
	req := &queryv1.SearchRequest{
		Query:       query,
		Participant: q.Get("participant"),
	}

	if raw := q.Get("types"); raw != "" {
		for _, name := range strings.Split(raw, ",") {
			v, ok := documentv1.DocType_value[name]
			if !ok || v == 0 { // UNSPECIFIED is not a usable filter
				return nil, fmt.Errorf("unknown doc type %q", name)
			}
			req.DocTypes = append(req.DocTypes, documentv1.DocType(v))
		}
	}

	var err error
	if req.FromDate, err = parseRFC3339Param(q, "from"); err != nil {
		return nil, err
	}
	if req.ToDate, err = parseRFC3339Param(q, "to"); err != nil {
		return nil, err
	}
	if req.Limit, err = parseIntParam(q, "limit"); err != nil {
		return nil, err
	}
	if req.Offset, err = parseIntParam(q, "offset"); err != nil {
		return nil, err
	}

	switch q.Get("mode") {
	case "", "hybrid":
		req.Mode = queryv1.SearchMode_HYBRID
	case "keyword":
		req.Mode = queryv1.SearchMode_KEYWORD
	case "vector":
		req.Mode = queryv1.SearchMode_VECTOR
	default:
		return nil, fmt.Errorf("invalid mode %q: want hybrid, keyword or vector", q.Get("mode"))
	}

	// debug=1 asks the query service to include per-hit feature contributions
	// (Hit.features) — a ranking-reproducibility surface, never a ranking input.
	switch q.Get("debug") {
	case "", "0", "false":
	case "1", "true":
		req.Debug = true
	default:
		return nil, fmt.Errorf("invalid debug %q: want 1 or 0", q.Get("debug"))
	}

	// rerank=1 requests the cross-encoder rerank pass. Honored only for HYBRID
	// with text and when the query service has a reranker wired; otherwise it is
	// ignored, and a reranker failure degrades to the fused order.
	switch q.Get("rerank") {
	case "", "0", "false":
	case "1", "true":
		req.Rerank = true
	default:
		return nil, fmt.Errorf("invalid rerank %q: want 1 or 0", q.Get("rerank"))
	}

	return req, nil
}

func parseRFC3339Param(q url.Values, name string) (*timestamppb.Timestamp, error) {
	raw := q.Get(name)
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, fmt.Errorf("invalid %s timestamp %q: must be RFC3339", name, raw)
	}
	return timestamppb.New(t), nil
}

func parseIntParam(q url.Values, name string) (int32, error) {
	raw := q.Get(name)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > math.MaxInt32 {
		return 0, fmt.Errorf("invalid %s %q: must be a non-negative integer", name, raw)
	}
	return int32(n), nil
}

func restSearchResponse(resp *queryv1.SearchResponse) searchResponseJSON {
	hits := make([]searchHitJSON, 0, len(resp.GetHits()))
	for _, h := range resp.GetHits() {
		md := h.GetMetadata()
		if md == nil {
			md = map[string]string{} // pinned shape: {} not null
		}
		hits = append(hits, searchHitJSON{
			DocID:        h.GetDocId(),
			ConnectorID:  h.GetConnectorId(),
			Type:         h.GetType().String(),
			Title:        h.GetTitle(),
			Snippet:      h.GetSnippet(),
			Score:        h.GetScore(),
			Created:      rfc3339OrEmpty(h.GetCreated()),
			Modified:     rfc3339OrEmpty(h.GetModified()),
			Metadata:     md,
			SourceURL:    sourceURLFromMetadata(md),
			StartMs:      h.GetStartMs(),
			EndMs:        h.GetEndMs(),
			Modality:     h.GetModality(),
			ThumbnailKey: h.GetThumbnailKey(),
			Explanation:  h.GetExplanation(),
			Features:     h.GetFeatures(),
		})
	}
	return searchResponseJSON{
		Hits:     hits,
		Total:    resp.GetTotal(),
		Degraded: resp.GetDegraded(),
		TookMs:   resp.GetTookMs(),
		Cached:   resp.GetCached(),
	}
}

// sourceLinkKeys are the connector metadata keys that may hold a browser-
// openable link to the original item, in priority order. Connectors are not
// consistent (Outlook/Outlook-cal web_link, Drive web_view_link, Slack
// permalink, GCal html_link, Teams/Confluence/Jira web_url, iCal url), so the
// gateway picks the first present, http(s) value and exposes it as the single
// source_url field the web renders. A "source_url" key wins if a connector
// ever sets one directly.
var sourceLinkKeys = []string{
	"source_url", "web_link", "web_view_link", "permalink", "html_link", "web_url", "url",
}

// sourceURLFromMetadata returns the first metadata value (by sourceLinkKeys
// priority) that is an absolute http(s) URL with a host, or "" when none
// qualifies. The scheme check is a safety gate: the value originates from an
// external provider and the web renders it as an href, so only http/https may
// become a link — never javascript:/data: or a scheme-relative target. The host
// check additionally rejects opaque/hostless forms ("https:evil") that would
// otherwise render as a broken relative link.
func sourceURLFromMetadata(md map[string]string) string {
	for _, k := range sourceLinkKeys {
		v := strings.TrimSpace(md[k])
		if v == "" {
			continue
		}
		u, err := url.Parse(v)
		if err != nil {
			continue
		}
		if (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
			return v
		}
	}
	return ""
}

func rfc3339OrEmpty(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return ""
	}
	return ts.AsTime().UTC().Format(time.RFC3339)
}
