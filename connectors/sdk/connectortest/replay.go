package connectortest

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// statusReplayMiss is the synthetic status served (and recorded as a miss) when
// a request matches no cassette interaction. 598 is outside the IANA-assigned
// range, so a connector that surfaces it cannot be confused with a real source
// status.
const statusReplayMiss = 598

// ReplayServer is an httptest.Server backed by a [Cassette]. It serves the
// recorded response for each matched request and records a miss (responding
// 598) for any unmatched request. Point a connector's base_url at URL() and its
// contract test runs with zero live network calls.
//
// A ReplayServer is safe for concurrent requests. Construct it with
// [NewReplayServer], which registers cleanup on the test.
type ReplayServer struct {
	tb       testing.TB
	cassette *Cassette
	server   *httptest.Server

	mu     sync.Mutex
	misses []Miss
}

// Miss describes an incoming request that matched no cassette interaction.
type Miss struct {
	Method string
	Path   string
	Query  string
	Body   string
}

func (m Miss) String() string {
	if m.Query != "" {
		return fmt.Sprintf("%s %s?%s", m.Method, m.Path, m.Query)
	}
	return fmt.Sprintf("%s %s", m.Method, m.Path)
}

// NewReplayServer starts an httptest.Server that replays c and registers
// t.Cleanup to close it and fail the test if any request missed the cassette.
// The returned server's URL() is what a connector's base_url should point at.
func NewReplayServer(tb testing.TB, c *Cassette) *ReplayServer {
	tb.Helper()
	if c == nil {
		tb.Fatalf("connectortest: NewReplayServer called with nil cassette")
		return nil
	}
	rs := &ReplayServer{tb: tb, cassette: c}
	rs.server = httptest.NewServer(http.HandlerFunc(rs.serve))
	tb.Cleanup(func() {
		rs.server.Close()
		if misses := rs.Misses(); len(misses) > 0 {
			tb.Errorf("connectortest: replay server for cassette %q had %d unmatched request(s):\n%s",
				c.Name, len(misses), formatMisses(misses))
		}
	})
	return rs
}

// URL is the base URL of the running replay server (no trailing slash).
func (rs *ReplayServer) URL() string { return rs.server.URL }

// Misses returns the requests that matched no interaction, in arrival order.
func (rs *ReplayServer) Misses() []Miss {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make([]Miss, len(rs.misses))
	copy(out, rs.misses)
	return out
}

// serve handles one request: read+buffer the body, match it against the
// cassette, and either replay the recorded response or record a miss (598).
func (rs *ReplayServer) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "connectortest: read request body: "+err.Error(), http.StatusInternalServerError)
		return
	}
	in := rs.cassette.match(r.Method, r.URL.Path, r.URL.RawQuery, body)
	if in == nil {
		rs.recordMiss(r, body)
		w.WriteHeader(statusReplayMiss)
		_, _ = io.WriteString(w, "connectortest: no cassette interaction matched this request")
		return
	}
	writeResponse(w, in.Response)
}

// recordMiss appends a miss for later reporting in cleanup.
func (rs *ReplayServer) recordMiss(r *http.Request, body []byte) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.misses = append(rs.misses, Miss{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.RawQuery,
		Body:   truncateBody(body),
	})
}

// writeResponse writes a recorded response to w.
func writeResponse(w http.ResponseWriter, resp RecordedResponse) {
	for k, vs := range resp.Headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	body, err := resp.Bytes()
	if err != nil {
		// validate() rejects this at load time; defend anyway.
		http.Error(w, "connectortest: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// ReplayTransport is an http.RoundTripper backed by a [Cassette], for
// connectors that build their own http.Client and have no base_url override to
// point at a [ReplayServer]. Inject it as the client's Transport:
//
//	rt := connectortest.NewReplayTransport(t, cassette)
//	client := &http.Client{Transport: rt}
//
// Like ReplayServer it records misses and the registered cleanup fails the test
// if any request went unmatched. It is safe for concurrent use.
type ReplayTransport struct {
	tb       testing.TB
	cassette *Cassette

	mu     sync.Mutex
	misses []Miss
}

// NewReplayTransport returns a ReplayTransport replaying c and registers
// t.Cleanup to fail the test on any unmatched request.
func NewReplayTransport(tb testing.TB, c *Cassette) *ReplayTransport {
	tb.Helper()
	if c == nil {
		tb.Fatalf("connectortest: NewReplayTransport called with nil cassette")
		return nil
	}
	rt := &ReplayTransport{tb: tb, cassette: c}
	tb.Cleanup(func() {
		if misses := rt.Misses(); len(misses) > 0 {
			tb.Errorf("connectortest: replay transport for cassette %q had %d unmatched request(s):\n%s",
				c.Name, len(misses), formatMisses(misses))
		}
	})
	return rt
}

// Misses returns the requests that matched no interaction, in arrival order.
func (rt *ReplayTransport) Misses() []Miss {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]Miss, len(rt.misses))
	copy(out, rt.misses)
	return out
}

// RoundTrip implements http.RoundTripper. It buffers the request body, matches
// the cassette, and synthesizes an *http.Response from the recorded
// interaction, or a 598 response recorded as a miss.
func (rt *ReplayTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("connectortest: read request body: %w", err)
		}
	}
	in := rt.cassette.match(req.Method, req.URL.Path, req.URL.RawQuery, body)
	if in == nil {
		rt.recordMiss(req, body)
		return synthResponse(req, RecordedResponse{
			Status: statusReplayMiss,
			Body:   "connectortest: no cassette interaction matched this request",
		})
	}
	return synthResponse(req, in.Response)
}

func (rt *ReplayTransport) recordMiss(req *http.Request, body []byte) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.misses = append(rt.misses, Miss{
		Method: req.Method,
		Path:   req.URL.Path,
		Query:  req.URL.RawQuery,
		Body:   truncateBody(body),
	})
}

// synthResponse builds an *http.Response from a recorded response.
func synthResponse(req *http.Request, resp RecordedResponse) (*http.Response, error) {
	body, err := resp.Bytes()
	if err != nil {
		return nil, err
	}
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	header := make(http.Header, len(resp.Headers))
	for k, vs := range resp.Headers {
		for _, v := range vs {
			header.Add(k, v)
		}
	}
	return &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

// formatMisses renders misses one per line for a readable test failure.
func formatMisses(misses []Miss) string {
	lines := make([]string, len(misses))
	for i, m := range misses {
		lines[i] = "  - " + m.String()
	}
	return strings.Join(lines, "\n")
}

// truncateBody renders a request body for a miss message, bounding its length
// so a large upload does not flood the failure output.
func truncateBody(body []byte) string {
	const max = 256
	if len(body) == 0 {
		return ""
	}
	s := string(body)
	if len(s) > max {
		return s[:max] + "...(truncated)"
	}
	return s
}

// sortedHeaderKeys returns header keys sorted for deterministic recording.
func sortedHeaderKeys(h http.Header) []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
