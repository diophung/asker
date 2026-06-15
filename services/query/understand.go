package main

import (
	"strings"
	"time"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// Query understanding (spec §2.6): extract inline filters from the raw query
// text, merge them with the structured request fields (request fields win),
// and keep what remains as the residual search text.
//
// Inline filter syntax (prefixes matched case-insensitively, one filter per
// whitespace-separated token):
//
//	from:<token>      -> participant filter
//	type:<name>       -> DocType filter (case-insensitive enum name; a
//	                     trailing plural 's' is tolerated, e.g. type:emails)
//	after:YYYY-MM-DD  -> created_at lower bound, inclusive (>= midnight UTC)
//	before:YYYY-MM-DD -> created_at upper bound, inclusive of the whole named
//	                     day (<= 23:59:59 UTC that day)
//
// A token that looks like a filter but does not parse (unknown type, bad
// date, empty value) is left in the residual text rather than dropped: a
// query like "type:writing tips" must not silently lose words.

// parsedQuery is the output of query understanding: the residual free-text
// query plus the merged filter set.
type parsedQuery struct {
	// Text is the residual search text after inline filters were removed,
	// whitespace-normalized. Empty with filters present means filter-only
	// search (Vespa 'true' clause, keyword ranking).
	Text string
	// DocTypes filters by document type; deduplicated, never UNSPECIFIED.
	DocTypes []askerv1.DocType
	// From and To bound created_at (inclusive). Zero values mean unbounded.
	From time.Time
	To   time.Time
	// Participant filters by participant token (sender/attendee).
	Participant string

	// --- v3 query understanding (set only on the personalized path, scope.go) ---
	// Intent is the classified query intent (schedule_lookup / needs_attention /
	// find_item / freeform); intentFreeform (zero) on the non-personalized path.
	Intent intentClass
	// EventFrom/EventTo bound event_start (occurrence time) for a schedule
	// lookup — the correct date field for "what's on my calendar next week"
	// (created_at is the authoring time). Zero means unbounded.
	EventFrom time.Time
	EventTo   time.Time
	// WinFrom/WinTo is the resolved temporal window the attention scorer uses for
	// the needs-attention intent (upcoming-event proximity). Zero means none.
	WinFrom time.Time
	WinTo   time.Time
}

// hasFilters reports whether any filter dimension is set.
func (p parsedQuery) hasFilters() bool {
	return len(p.DocTypes) > 0 || !p.From.IsZero() || !p.To.IsZero() || p.Participant != ""
}

// understand runs filter extraction over the normalized request and applies
// the merge precedence: structured request fields override inline filters.
func understand(req *queryv1.SearchRequest) parsedQuery {
	p := extractInlineFilters(req.GetQuery())
	if types := dedupeDocTypes(req.GetDocTypes()); len(types) > 0 {
		p.DocTypes = types
	}
	if req.GetFromDate() != nil {
		p.From = req.GetFromDate().AsTime()
	}
	if req.GetToDate() != nil {
		p.To = req.GetToDate().AsTime()
	}
	if req.GetParticipant() != "" {
		p.Participant = req.GetParticipant()
	}
	return p
}

// extractInlineFilters pulls from:/type:/before:/after: tokens out of raw.
// Repeated from:/before:/after: filters: the last one wins. Repeated type:
// filters accumulate (OR semantics downstream).
func extractInlineFilters(raw string) parsedQuery {
	var p parsedQuery
	var residual []string
	seenTypes := make(map[askerv1.DocType]bool)

	for _, tok := range strings.Fields(raw) {
		lower := strings.ToLower(tok)
		switch {
		case strings.HasPrefix(lower, "from:"):
			v := tok[len("from:"):]
			if v == "" {
				residual = append(residual, tok)
				continue
			}
			p.Participant = v
		case strings.HasPrefix(lower, "type:"):
			dt, ok := parseDocType(tok[len("type:"):])
			if !ok {
				residual = append(residual, tok)
				continue
			}
			if !seenTypes[dt] {
				seenTypes[dt] = true
				p.DocTypes = append(p.DocTypes, dt)
			}
		case strings.HasPrefix(lower, "before:"):
			day, ok := parseDay(tok[len("before:"):])
			if !ok {
				residual = append(residual, tok)
				continue
			}
			// Inclusive of the named day: the upper bound is the last second
			// of that day, so created_at <= bound keeps same-day documents
			// (midnight alone would exclude almost the entire day).
			p.To = day.Add(24*time.Hour - time.Second)
		case strings.HasPrefix(lower, "after:"):
			day, ok := parseDay(tok[len("after:"):])
			if !ok {
				residual = append(residual, tok)
				continue
			}
			p.From = day
		default:
			residual = append(residual, tok)
		}
	}
	p.Text = strings.Join(residual, " ")
	return p
}

// parseDocType resolves a case-insensitive DocType enum name, tolerating a
// plural trailing 's' (emails -> EMAIL, chat_messages -> CHAT_MESSAGE).
// DOC_TYPE_UNSPECIFIED is not addressable as a filter.
func parseDocType(name string) (askerv1.DocType, bool) {
	upper := strings.ToUpper(name)
	if v, ok := askerv1.DocType_value[upper]; ok && v != int32(askerv1.DocType_DOC_TYPE_UNSPECIFIED) {
		return askerv1.DocType(v), true
	}
	if singular, ok := strings.CutSuffix(upper, "S"); ok {
		if v, ok := askerv1.DocType_value[singular]; ok && v != int32(askerv1.DocType_DOC_TYPE_UNSPECIFIED) {
			return askerv1.DocType(v), true
		}
	}
	return askerv1.DocType_DOC_TYPE_UNSPECIFIED, false
}

// parseDay parses a strict YYYY-MM-DD date as midnight UTC.
func parseDay(s string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// dedupeDocTypes drops UNSPECIFIED and duplicate entries, preserving order.
func dedupeDocTypes(types []askerv1.DocType) []askerv1.DocType {
	if len(types) == 0 {
		return nil
	}
	seen := make(map[askerv1.DocType]bool, len(types))
	out := make([]askerv1.DocType, 0, len(types))
	for _, t := range types {
		if t == askerv1.DocType_DOC_TYPE_UNSPECIFIED || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// Pagination and mode bounds (step 1 of the pipeline).
const (
	defaultLimit = 20
	maxLimit     = 100
	maxOffset    = 1000
)

// normalizeRequest returns a normalized copy of req: limit defaulted/capped,
// offset clamped, mode defaulted to HYBRID, text fields trimmed. The
// normalized request is also the cache-key input, so equivalent requests
// share a cache entry.
func normalizeRequest(req *queryv1.SearchRequest) *queryv1.SearchRequest {
	limit := req.GetLimit()
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	offset := req.GetOffset()
	if offset < 0 {
		offset = 0
	}
	if offset > maxOffset {
		offset = maxOffset
	}
	mode := req.GetMode()
	if mode == queryv1.SearchMode_SEARCH_MODE_UNSPECIFIED {
		mode = queryv1.SearchMode_HYBRID
	}
	return &queryv1.SearchRequest{
		Query:       strings.TrimSpace(req.GetQuery()),
		DocTypes:    dedupeDocTypes(req.GetDocTypes()),
		FromDate:    req.GetFromDate(),
		ToDate:      req.GetToDate(),
		Participant: strings.TrimSpace(req.GetParticipant()),
		Limit:       limit,
		Offset:      offset,
		Mode:        mode,
		// Debug is a pass-through observability flag (per-hit feature
		// contributions); it never affects ranking but must survive normalization
		// so the personalized re-rank can honor it.
		Debug: req.GetDebug(),
	}
}
