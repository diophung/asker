package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
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
	// retrieveCLIP: the text->image arm (ADR-013). nearestNeighbor over the
	// CLIP-space clip_embedding tensor with the 'clip' ranking profile; the
	// query vector comes from the clip service's text encoder, scoped to the
	// SAME tenant streaming group. It runs ALONGSIDE the text/hybrid arm and
	// is merged by doc_id (server.go), so a purely-visual match still appears.
	// Only chunks carrying a CLIP vector (image / keyframe chunks) match.
	retrieveCLIP
)

// profile maps the retrieval kind to the ranking profile (vespa/README.md,
// ADR-013).
func (k retrievalKind) profile() string {
	switch k {
	case retrieveHybrid, retrieveVector:
		return "hybrid"
	case retrieveCLIP:
		return "clip"
	default:
		return "keyword"
	}
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
	// clipNearestNeighborClause: the CLIP-space text->image arm (ADR-013).
	// Same exact-scan recall floor; matches only chunks that carry a CLIP
	// vector. The query tensor arrives as input.query(qclip).
	clipNearestNeighborClause = "({targetHits:100}nearestNeighbor(clip_embedding,qclip))"
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
	// ClipVector is the CLIP-space query vector for retrieveCLIP; passed via
	// input.query(qclip).
	ClipVector []float32

	DocTypes    []askerv1.DocType
	From, To    time.Time
	Participant string

	// EventFrom/EventTo bound event_start (occurrence time) for a schedule
	// lookup — the correct field for "what's on my calendar next week" (created_at
	// is the authoring time). Zero means unbounded. (v3.2, DECISIONS D11)
	EventFrom, EventTo time.Time

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
	case retrieveCLIP:
		// text->image: the CLIP nearestNeighbor IS the match set (no keyword
		// text in this space); the 'clip' profile ranks by closeness.
		clauses = append(clauses, clipNearestNeighborClause)
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
	// event_start (occurrence time) range for schedule lookups (v3.2). The
	// half-open [EventFrom, EventTo) window is rendered as >= From and < To.
	if !q.EventFrom.IsZero() {
		clauses = append(clauses, fmt.Sprintf("event_start >= %d", q.EventFrom.Unix()))
	}
	if !q.EventTo.IsZero() {
		clauses = append(clauses, fmt.Sprintf("event_start < %d", q.EventTo.Unix()))
	}
	if q.Participant != "" {
		lit, err := yqlStringLiteral(q.Participant)
		if err != nil {
			return "", fmt.Errorf("participant filter: %w", err)
		}
		clauses = append(clauses, "participants contains ({substring:true}"+lit+")")
	}

	yql := "select * from sources * where " + strings.Join(clauses, " and ")

	// Filter-only schedule lookups ("what's on my calendar", "upcoming meetings")
	// list events by occurrence time. Order by event_start ASCENDING in Vespa so
	// the candidate cap captures the SOONEST events — without this an unbounded
	// upcoming lookup returns an arbitrary cap-sized sample (which, once re-sorted
	// client-side, starts months out instead of today). EventFrom is set only for
	// schedule lookups, so this never reorders a generic filter-only search (e.g.
	// an empty query with a type filter on email, where event_start is 0).
	if q.Kind == retrieveFilterOnly && !q.EventFrom.IsZero() {
		yql += " order by event_start asc"
	}
	return yql, nil
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
	// The free-text query= drives userQuery(); the CLIP arm has no userQuery()
	// clause, so it must not carry it.
	if q.Text != "" && q.Kind != retrieveVector && q.Kind != retrieveCLIP {
		body["query"] = q.Text
	}
	if len(q.Vector) > 0 && (q.Kind == retrieveHybrid || q.Kind == retrieveVector) {
		body["input.query(q)"] = q.Vector
	}
	if q.Kind == retrieveCLIP && len(q.ClipVector) > 0 {
		body["input.query(qclip)"] = q.ClipVector
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
	return parseVespaResponse(raw, q.Kind)
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
// lean result-card fields plus the dynamic snippet/chunk_snippets fragments,
// plus the M3 media fields (ADR-013): the parallel chunk_starts_ms/
// chunk_ends_ms/chunk_modalities arrays (same index order as chunks) and the
// doc-level media attributes. summaryfeatures carries the matched-chunk index
// for the CLIP arm (closest(clip_embedding)).
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

	// M3 media fields (ADR-013); omitted/zero for text documents.
	ChunkStartsMs   []int64  `json:"chunk_starts_ms"`
	ChunkEndsMs     []int64  `json:"chunk_ends_ms"`
	ChunkModalities []string `json:"chunk_modalities"`
	ThumbnailKey    string   `json:"thumbnail_key"`

	// summaryfeatures exposes rank features in the result; closest(clip_embedding)
	// names the matched chunk for the CLIP arm.
	SummaryFeatures map[string]json.RawMessage `json:"summaryfeatures"`
}

func parseVespaResponse(raw []byte, kind retrievalKind) (vespaResult, error) {
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
		populateMediaFields(hit, f, kind)
		out.Hits = append(out.Hits, hit)
	}
	return out, nil
}

// populateMediaFields fills the M3 deep-link fields (ADR-013) from the matched
// chunk's parallel arrays. thumbnail_key is doc-level and always set when
// present. The matched chunk index source depends on the arm:
//
//   - CLIP arm: the closest(clip_embedding) summary feature names the chunk
//     whose visual vector matched.
//   - text/ASR arm (keyword/hybrid/vector): the first chunk_snippets element
//     carrying a <hi> highlight is the matched chunk; absent any highlight
//     (e.g. a pure vector match), no chunk offset is attributed.
//
// The index writer labels every chunk's modality, including plain text chunks
// of EMAIL/FILE docs ("text"). The Hit contract (query.proto) reserves the
// start_ms/end_ms/modality fields for IMAGE/AUDIO/VIDEO hits and pins them to
// zero "for the whole-document / non-media case". So a matched chunk whose
// modality is "text" (or unlabeled) is treated as that non-media case:
// modality and offsets stay at their zero values. Only genuine media chunks
// (ocr / asr / caption) populate them. thumbnail_key is still propagated
// regardless — an image surfaced only by its OCR text can legitimately carry a
// poster, and a pure text doc has none anyway.
//
// All reads are defensive: a doc that is not media (no parallel arrays) or a
// matched index out of range simply leaves the offset/modality at zero — the
// whole-document / text case the proto documents.
func populateMediaFields(hit *queryv1.Hit, f vespaHitFields, kind retrievalKind) {
	hit.ThumbnailKey = f.ThumbnailKey

	var idx int
	if kind == retrieveCLIP {
		idx = matchedClipChunkIndex(f.SummaryFeatures)
	} else {
		idx = matchedTextChunkIndex(f.ChunkSnippets)
		// A media hit matched by the text arm should anchor to a transcript
		// segment even when Vespa's dynamic summary did not highlight the term
		// in a returned snippet, so the timestamp deep-link is reliable rather
		// than dependent on snippet highlighting. Fall back to the first
		// timestamped media chunk (asr/ocr); a pure-text doc has none and stays
		// at the whole-document/non-media case.
		if idx < 0 {
			idx = firstTimedMediaChunkIndex(f.ChunkModalities)
		}
	}
	if idx < 0 || idx >= len(f.ChunkModalities) {
		return
	}
	// "text" (or empty) marks a non-media chunk: leave the media fields zero.
	modality := f.ChunkModalities[idx]
	if modality == "" || modality == "text" {
		return
	}
	hit.Modality = modality
	if idx < len(f.ChunkStartsMs) {
		hit.StartMs = f.ChunkStartsMs[idx]
	}
	if idx < len(f.ChunkEndsMs) {
		hit.EndMs = f.ChunkEndsMs[idx]
	}
}

// matchedTextChunkIndex returns the index of the first chunk_snippets element
// carrying a <hi> highlight (the matched chunk for the text/ASR arm), or -1.
func matchedTextChunkIndex(chunkSnippets []string) int {
	const hi = "<hi>"
	for i, cs := range chunkSnippets {
		if strings.Contains(cs, hi) {
			return i
		}
	}
	return -1
}

// firstTimedMediaChunkIndex returns the index of the first asr/ocr chunk (a
// timestamped media segment), or -1 when there is none. It anchors a media
// hit's deep-link to its first transcript segment when the exact matched chunk
// could not be pinpointed from the snippet highlights.
func firstTimedMediaChunkIndex(modalities []string) int {
	for i, m := range modalities {
		if m == "asr" || m == "ocr" {
			return i
		}
	}
	return -1
}

// matchedClipChunkIndex reads the matched chunk index for the CLIP arm from
// the closest(clip_embedding) summary feature. Vespa renders closest() as a
// mapped tensor with a single cell whose label is the chunk index, e.g.
// {"type":"tensor(chunk{})","cells":{"3":1.0}}. Older/!verbose renderings may
// emit a plain {"3":1.0} map. Both shapes are handled; anything unexpected
// yields -1 (no offset attributed).
func matchedClipChunkIndex(features map[string]json.RawMessage) int {
	raw, ok := features["closest(clip_embedding)"]
	if !ok {
		return -1
	}
	// Shape 1: {"type":...,"cells":{"<idx>":<v>}}.
	var tensor struct {
		Cells map[string]float64 `json:"cells"`
	}
	if err := json.Unmarshal(raw, &tensor); err == nil && len(tensor.Cells) > 0 {
		return firstTensorLabel(tensor.Cells)
	}
	// Shape 2: a bare {"<idx>":<v>} map.
	var cells map[string]float64
	if err := json.Unmarshal(raw, &cells); err == nil && len(cells) > 0 {
		return firstTensorLabel(cells)
	}
	return -1
}

// firstTensorLabel parses the (single) cell label of a closest() tensor as the
// chunk index. closest() returns exactly one cell; if more ever appear, the
// numerically smallest label is chosen for determinism.
func firstTensorLabel(cells map[string]float64) int {
	best := -1
	for label := range cells {
		idx, err := strconv.Atoi(label)
		if err != nil || idx < 0 {
			continue
		}
		if best < 0 || idx < best {
			best = idx
		}
	}
	return best
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
