package main

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	documentv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// searchHitJSON is the pinned REST hit shape (web/src/api.ts Hit). Field
// names and presence are contractual — every key is always emitted.
type searchHitJSON struct {
	DocID       string            `json:"doc_id"`
	ConnectorID string            `json:"connector_id"`
	Type        string            `json:"type"`
	Title       string            `json:"title"`
	Snippet     string            `json:"snippet"`
	Score       float64           `json:"score"`
	Created     string            `json:"created"`
	Modified    string            `json:"modified"`
	Metadata    map[string]string `json:"metadata"`
}

// searchResponseJSON is the pinned REST response shape (web/src/api.ts
// SearchResponse).
type searchResponseJSON struct {
	Hits     []searchHitJSON `json:"hits"`
	Total    int64           `json:"total"`
	Degraded string          `json:"degraded"`
	TookMs   int64           `json:"took_ms"`
	Cached   bool            `json:"cached"`
}

// handleSearch proxies GET /v1/search to the QueryService. The tenant is NOT
// handled here: it rides the request context into the gRPC client
// interceptor, which fails closed without one.
func (d *deps) handleSearch(w http.ResponseWriter, r *http.Request) {
	req, err := parseSearchRequest(r.URL.Query())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	resp, err := d.query.Search(r.Context(), req)
	if err != nil {
		d.upstreamError(w, r, "QueryService.Search", err)
		return
	}
	writeJSON(w, http.StatusOK, restSearchResponse(resp))
}

// parseSearchRequest validates and maps the /v1/search query parameters onto
// the QueryService request. Any malformed parameter is a 400.
func parseSearchRequest(q url.Values) (*queryv1.SearchRequest, error) {
	req := &queryv1.SearchRequest{
		Query:       q.Get("q"),
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
			DocID:       h.GetDocId(),
			ConnectorID: h.GetConnectorId(),
			Type:        h.GetType().String(),
			Title:       h.GetTitle(),
			Snippet:     h.GetSnippet(),
			Score:       h.GetScore(),
			Created:     rfc3339OrEmpty(h.GetCreated()),
			Modified:    rfc3339OrEmpty(h.GetModified()),
			Metadata:    md,
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

func rfc3339OrEmpty(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return ""
	}
	return ts.AsTime().UTC().Format(time.RFC3339)
}
