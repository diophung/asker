package connectortest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
)

// recordEnv is the environment variable that switches a [NewRecorder] /
// [NewRecordingTransport] from replay (the CI default) into record mode.
const recordEnv = "ASKER_RECORD"

// redactedValue replaces credential header values in a recorded cassette so
// fixtures committed to the repo contain NO secrets.
const redactedValue = "REDACTED"

// redactedHeaders are request headers whose values are replaced with
// redactedValue when recording (case-insensitive, lowercased keys). Add to this
// set if a source carries credentials in a bespoke header.
var redactedHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"x-api-key":           true,
	"x-goog-api-key":      true,
}

// Recording mode workflow (ADR-011):
//
//  1. Stand up the real or in-repo fake upstream (e.g. tools/fake-gmail) and
//     point NewRecorder at its URL.
//  2. Run the test ONCE with ASKER_RECORD=1: every request is proxied to the
//     upstream and the interaction (with credential headers redacted) is
//     appended to the cassette file on disk when the recorder closes.
//  3. Commit the cassette. Thereafter the same test runs in replay mode (no
//     ASKER_RECORD) with zero network calls — this is what CI executes.
//
// NewRecorder returns a recorder serving an httptest.Server; for connectors
// that need a RoundTripper, NewRecordingTransport is the transport-level twin.

// Recorder proxies requests to a real upstream and writes a cassette on Close,
// or replays an existing cassette — selected by the ASKER_RECORD env var. Use
// [NewRecorder] to obtain one wired to a test.
type Recorder struct {
	tb        testing.TB
	recording bool

	// replay path
	server *ReplayServer

	// record path
	target       *url.URL
	proxy        *httptest.Server
	cassettePath string

	mu           sync.Mutex
	interactions []*Interaction
}

// NewRecorder returns a record/replay HTTP layer for a connector contract test.
// With ASKER_RECORD unset (CI) it loads cassettePath and behaves exactly like
// [NewReplayServer]. With ASKER_RECORD=1 it proxies every request to target
// (the real or fake upstream) and writes the captured, credential-redacted
// interactions to cassettePath when the test ends.
//
// Either way the returned recorder exposes URL() to point a connector's
// base_url at, and registers all cleanup on tb.
func NewRecorder(tb testing.TB, target, cassettePath string) *Recorder {
	tb.Helper()
	rec := &Recorder{tb: tb, cassettePath: cassettePath, recording: recordingEnabled()}
	if !rec.recording {
		c, err := LoadCassette(cassettePath)
		if err != nil {
			tb.Fatalf("connectortest: NewRecorder (replay): %v", err)
			return nil
		}
		rec.server = NewReplayServer(tb, c)
		return rec
	}

	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		tb.Fatalf("connectortest: NewRecorder (record): target %q is not a valid URL: %v", target, err)
		return nil
	}
	rec.target = u
	rec.proxy = httptest.NewServer(http.HandlerFunc(rec.proxyAndCapture))
	tb.Cleanup(func() {
		rec.proxy.Close()
		if err := rec.flush(); err != nil {
			tb.Fatalf("connectortest: write cassette %q: %v", cassettePath, err)
		}
		tb.Logf("connectortest: recorded %d interaction(s) to %s", len(rec.interactions), cassettePath)
	})
	return rec
}

// URL is the base URL a connector's base_url should point at: the replay server
// in replay mode, or the recording proxy in record mode.
func (rec *Recorder) URL() string {
	if rec.recording {
		return rec.proxy.URL
	}
	return rec.server.URL()
}

// proxyAndCapture forwards a request to the upstream target, captures the
// interaction (redacting credentials), and copies the response back to the
// caller.
func (rec *Recorder) proxyAndCapture(w http.ResponseWriter, r *http.Request) {
	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "connectortest: read request body: "+err.Error(), http.StatusInternalServerError)
		return
	}

	outURL := *rec.target
	outURL.Path = singleJoin(rec.target.Path, r.URL.Path)
	outURL.RawQuery = r.URL.RawQuery
	out, err := http.NewRequestWithContext(r.Context(), r.Method, outURL.String(), bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, "connectortest: build upstream request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	copyHeader(out.Header, r.Header)
	out.Header.Del("Accept-Encoding") // capture decoded bodies, not gzip

	resp, err := http.DefaultClient.Do(out)
	if err != nil {
		http.Error(w, "connectortest: upstream request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "connectortest: read upstream response: "+err.Error(), http.StatusBadGateway)
		return
	}

	rec.capture(r, reqBody, resp, respBody)

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

// capture appends one interaction to the in-memory recording with credential
// headers redacted. Request bodies are not stored (they are rarely needed to
// disambiguate, and may carry secrets); add a body pin by hand if a test needs
// it.
func (rec *Recorder) capture(r *http.Request, _ []byte, resp *http.Response, respBody []byte) {
	in := &Interaction{
		Request: RecordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
		},
		Response: RecordedResponse{
			Status:  resp.StatusCode,
			Headers: redactResponseHeaders(resp.Header),
		},
	}
	in.Response.setBody(respBody)

	rec.mu.Lock()
	rec.interactions = append(rec.interactions, in)
	rec.mu.Unlock()
}

// flush writes the recorded interactions to the cassette file as indented JSON.
func (rec *Recorder) flush() error {
	rec.mu.Lock()
	file := cassetteFile{Interactions: rec.interactions}
	rec.mu.Unlock()
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(rec.cassettePath, raw, 0o600)
}

// RecordingTransport is the http.RoundTripper twin of [Recorder] for connectors
// that build their own http.Client. In replay mode it is a [ReplayTransport];
// in record mode it proxies to the connector's real client transport and
// captures interactions.
type RecordingTransport struct {
	tb           testing.TB
	recording    bool
	cassettePath string

	replay *ReplayTransport // replay mode

	base         http.RoundTripper // record mode: the real transport
	mu           sync.Mutex
	interactions []*Interaction
}

// NewRecordingTransport returns a record/replay http.RoundTripper. In replay
// mode (ASKER_RECORD unset) it serves cassettePath; in record mode it sends
// requests through base (default http.DefaultTransport) and writes the
// credential-redacted cassette on test cleanup. Inject the returned transport
// into the connector's http.Client.
func NewRecordingTransport(tb testing.TB, base http.RoundTripper, cassettePath string) *RecordingTransport {
	tb.Helper()
	rt := &RecordingTransport{tb: tb, cassettePath: cassettePath, recording: recordingEnabled(), base: base}
	if rt.base == nil {
		rt.base = http.DefaultTransport
	}
	if !rt.recording {
		c, err := LoadCassette(cassettePath)
		if err != nil {
			tb.Fatalf("connectortest: NewRecordingTransport (replay): %v", err)
			return nil
		}
		rt.replay = NewReplayTransport(tb, c)
		return rt
	}
	tb.Cleanup(func() {
		if err := rt.flush(); err != nil {
			tb.Fatalf("connectortest: write cassette %q: %v", cassettePath, err)
		}
		tb.Logf("connectortest: recorded %d interaction(s) to %s", len(rt.interactions), cassettePath)
	})
	return rt
}

// RoundTrip implements http.RoundTripper.
func (rt *RecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !rt.recording {
		return rt.replay.RoundTrip(req)
	}
	var reqBody []byte
	if req.Body != nil {
		var err error
		reqBody, err = io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}
	resp, err := rt.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	respBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(respBody))

	in := &Interaction{
		Request:  RecordedRequest{Method: req.Method, Path: req.URL.Path, Query: req.URL.RawQuery},
		Response: RecordedResponse{Status: resp.StatusCode, Headers: redactResponseHeaders(resp.Header)},
	}
	in.Response.setBody(respBody)
	rt.mu.Lock()
	rt.interactions = append(rt.interactions, in)
	rt.mu.Unlock()
	return resp, nil
}

func (rt *RecordingTransport) flush() error {
	rt.mu.Lock()
	file := cassetteFile{Interactions: rt.interactions}
	rt.mu.Unlock()
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(rt.cassettePath, raw, 0o600)
}

// recordingEnabled reports whether ASKER_RECORD requests record mode.
func recordingEnabled() bool {
	v := strings.TrimSpace(os.Getenv(recordEnv))
	return v != "" && v != "0" && !strings.EqualFold(v, "false")
}

// redactResponseHeaders copies response headers and strips Set-Cookie (the only
// credential-bearing response header) so a recorded fixture leaks no session.
func redactResponseHeaders(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for _, k := range sortedHeaderKeys(h) {
		if strings.EqualFold(k, "Set-Cookie") {
			out[k] = []string{redactedValue}
			continue
		}
		out[k] = append([]string(nil), h[k]...)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// redactRequestHeaders returns a copy of h with credential header values
// replaced by REDACTED. The default capture does not persist request headers,
// but a custom recorder that wants to store them can call this so fixtures
// never contain a bearer token, API key, or cookie.
func redactRequestHeaders(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for _, k := range sortedHeaderKeys(h) {
		if redactedHeaders[strings.ToLower(k)] {
			out[k] = []string{redactedValue}
			continue
		}
		out[k] = append([]string(nil), h[k]...)
	}
	return out
}

// copyHeader copies all header values from src into dst.
func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// singleJoin joins two URL path segments with exactly one slash between them.
func singleJoin(a, b string) string {
	switch {
	case a == "" || a == "/":
		return b
	case strings.HasSuffix(a, "/") && strings.HasPrefix(b, "/"):
		return a + b[1:]
	case !strings.HasSuffix(a, "/") && !strings.HasPrefix(b, "/"):
		return a + "/" + b
	default:
		return a + b
	}
}
