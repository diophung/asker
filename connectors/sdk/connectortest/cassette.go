package connectortest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"unicode/utf8"
)

// A Cassette is an ordered, indexed set of recorded HTTP interactions used to
// replay a source API without any live network call (the M2 "no live API calls
// in CI" rule, ADR-011). Connector contract tests point a connector's base_url
// (or its http.Client transport) at a [ReplayServer] backed by a Cassette and
// assert on the documents the connector emits.
//
// # On-disk format
//
// A cassette is a single human-readable+editable JSON file committed under a
// connector's testdata/ directory:
//
//	{
//	  "interactions": [
//	    {
//	      "request":  {"method": "GET", "path": "/gmail/v1/users/me/messages",
//	                   "query": "maxResults=100"},
//	      "response": {"status": 200,
//	                   "headers": {"Content-Type": ["application/json"]},
//	                   "body": "{\"messages\":[...]}"}
//	    },
//	    ...
//	  ]
//	}
//
// A response body is stored verbatim in "body" when it is valid UTF-8 (so it
// stays diff-able), or base64 in "body_base64" when it is binary. A request may
// pin a body for disambiguation either inline ("body") or by digest
// ("body_sha256"); see the matching rules below.
//
// # Matching and sequencing (precise rule)
//
// An incoming request matches an interaction when ALL of:
//
//   - method is equal (case-insensitive),
//   - path is equal (exact, after cleaning "//" and "."), and
//   - query keys+values are equal as a SET (order-independent; a recorded empty
//     query matches only an empty incoming query), and
//   - if the interaction pins a request body (body or body_sha256), the
//     incoming body matches it; interactions with no body pin ignore the body.
//
// When several interactions match the same request, they are served in the
// order they appear in the file, advancing a per-interaction cursor on each
// hit. This makes a cassette express call sequences directly: list page 1 then
// page 2 (different query, two interactions), or the SAME request returning a
// changed response across calls (two interactions, identical matcher, served in
// file order). Once every matching interaction for a request has been consumed,
// a further identical request is an unmatched request (a replay miss).
//
// Pin a POST/PUT body only when you need to tell two otherwise-identical
// requests apart; leaving it unpinned keeps fixtures resilient to harmless body
// reformatting.
type Cassette struct {
	// Name is a label for diagnostics (the file's base name when loaded).
	Name string
	// Interactions are the recorded request/response pairs in file order.
	Interactions []*Interaction
}

// Interaction is one recorded request/response pair plus replay bookkeeping.
type Interaction struct {
	Request  RecordedRequest  `json:"request"`
	Response RecordedResponse `json:"response"`

	// played counts how many times this interaction has already been served
	// during replay; the matcher serves duplicate-matcher interactions in file
	// order by preferring the one with the lowest played count.
	played int
}

// RecordedRequest is the matcher side of an interaction.
type RecordedRequest struct {
	// Method is the HTTP method; matched case-insensitively. Empty matches any
	// method (rare; prefer being explicit).
	Method string `json:"method"`
	// Path is the URL path, matched exactly after cleaning.
	Path string `json:"path"`
	// Query is the raw query string (e.g. "a=1&b=2"); matched as a set of
	// key/value pairs, so ordering in the file is irrelevant.
	Query string `json:"query,omitempty"`
	// Body, when set, pins the request body verbatim (used to disambiguate two
	// POSTs to the same path). Mutually informative with BodySHA256.
	Body string `json:"body,omitempty"`
	// BodySHA256, when set, pins the request body by lowercase-hex SHA-256
	// digest — handy for large or binary bodies you do not want inline.
	BodySHA256 string `json:"body_sha256,omitempty"`
}

// RecordedResponse is the served side of an interaction.
type RecordedResponse struct {
	// Status is the HTTP status code (defaults to 200 when zero).
	Status int `json:"status"`
	// Headers are response headers; canonical keys, multi-valued.
	Headers map[string][]string `json:"headers,omitempty"`
	// Body holds a UTF-8 response body verbatim. Exactly one of Body /
	// BodyBase64 is set when the body is non-empty.
	Body string `json:"body,omitempty"`
	// BodyBase64 holds a non-UTF-8 (binary) response body, base64-encoded.
	BodyBase64 string `json:"body_base64,omitempty"`
}

// Bytes returns the decoded response body. It prefers BodyBase64 (binary) and
// falls back to Body (UTF-8). An invalid base64 BodyBase64 yields an error so a
// corrupt fixture fails loudly rather than serving garbage.
func (r RecordedResponse) Bytes() ([]byte, error) {
	if r.BodyBase64 != "" {
		raw, err := base64.StdEncoding.DecodeString(r.BodyBase64)
		if err != nil {
			return nil, fmt.Errorf("connectortest: response body_base64 is not valid base64: %w", err)
		}
		return raw, nil
	}
	return []byte(r.Body), nil
}

// setBody stores body in the response, choosing the verbatim or base64 field by
// whether body is valid UTF-8. Empty bodies leave both fields empty.
func (r *RecordedResponse) setBody(body []byte) {
	r.Body = ""
	r.BodyBase64 = ""
	if len(body) == 0 {
		return
	}
	if utf8.Valid(body) {
		r.Body = string(body)
		return
	}
	r.BodyBase64 = base64.StdEncoding.EncodeToString(body)
}

// cassetteFile is the on-disk JSON envelope.
type cassetteFile struct {
	Interactions []*Interaction `json:"interactions"`
}

// LoadCassette reads and parses a cassette JSON file. The returned Cassette's
// Name is the file's base name. An empty file path, missing file, or malformed
// JSON returns an error.
func LoadCassette(path string) (*Cassette, error) {
	if path == "" {
		return nil, fmt.Errorf("connectortest: LoadCassette called with empty path")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // test fixture path supplied by the connector author
	if err != nil {
		return nil, fmt.Errorf("connectortest: read cassette %q: %w", path, err)
	}
	var file cassetteFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("connectortest: parse cassette %q: %w", path, err)
	}
	c := &Cassette{Name: baseName(path), Interactions: file.Interactions}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// validate checks that decoded bodies are well-formed so a corrupt fixture
// fails at load time, not mid-test.
func (c *Cassette) validate() error {
	for i, in := range c.Interactions {
		if in == nil {
			return fmt.Errorf("connectortest: cassette %q interaction %d is null", c.Name, i)
		}
		if _, err := in.Response.Bytes(); err != nil {
			return fmt.Errorf("connectortest: cassette %q interaction %d: %w", c.Name, i, err)
		}
		if in.Request.Query != "" {
			if _, err := url.ParseQuery(in.Request.Query); err != nil {
				return fmt.Errorf("connectortest: cassette %q interaction %d: bad query %q: %w", c.Name, i, in.Request.Query, err)
			}
		}
	}
	return nil
}

// match finds the next unplayed interaction whose matcher accepts (method,
// path, rawQuery, body) and marks it played. It returns nil when no unplayed
// interaction matches (a replay miss). Interactions sharing a matcher are
// served in file order because the search runs front-to-back and stops at the
// first unplayed match.
func (c *Cassette) match(method, path, rawQuery string, body []byte) *Interaction {
	for _, in := range c.Interactions {
		if in.played > 0 {
			continue
		}
		if in.Request.matches(method, path, rawQuery, body) {
			in.played++
			return in
		}
	}
	return nil
}

// matches reports whether req accepts an incoming request.
func (req RecordedRequest) matches(method, path, rawQuery string, body []byte) bool {
	if req.Method != "" && !strings.EqualFold(req.Method, method) {
		return false
	}
	if cleanPath(req.Path) != cleanPath(path) {
		return false
	}
	if !queryEqual(req.Query, rawQuery) {
		return false
	}
	return req.bodyMatches(body)
}

// bodyMatches enforces a request body pin, if any. With no pin the body is
// ignored (the common case).
func (req RecordedRequest) bodyMatches(body []byte) bool {
	switch {
	case req.BodySHA256 != "":
		sum := sha256.Sum256(body)
		return strings.EqualFold(req.BodySHA256, hex.EncodeToString(sum[:]))
	case req.Body != "":
		return req.Body == string(body)
	default:
		return true
	}
}

// queryEqual compares two raw query strings as multisets of key/value pairs, so
// "a=1&b=2" and "b=2&a=1" are equal and key order in the cassette is irrelevant.
func queryEqual(a, b string) bool {
	pa, errA := url.ParseQuery(a)
	pb, errB := url.ParseQuery(b)
	if errA != nil || errB != nil {
		// Fall back to a literal compare when either side is unparseable; a
		// load-time validate already rejected unparseable cassette queries.
		return a == b
	}
	if len(pa) != len(pb) {
		return false
	}
	for k, va := range pa {
		vb, ok := pb[k]
		if !ok || len(va) != len(vb) {
			return false
		}
		sa := append([]string(nil), va...)
		sb := append([]string(nil), vb...)
		sort.Strings(sa)
		sort.Strings(sb)
		for i := range sa {
			if sa[i] != sb[i] {
				return false
			}
		}
	}
	return true
}

// cleanPath normalizes a URL path for comparison: it collapses "//" and "/./"
// and trims a trailing slash (except for the root "/"), so trivial formatting
// differences between a recorded and a live path do not cause spurious misses.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	// Replace repeated slashes.
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	p = strings.ReplaceAll(p, "/./", "/")
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

// baseName returns the final path element of p (a tiny filepath.Base that does
// not pull in path/filepath just for diagnostics).
func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
