package connectortest

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// getBody GETs path on base and returns status + body.
func getBody(t *testing.T, client *http.Client, urlStr string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, urlStr, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestReplayServerServesMatchedResponses(t *testing.T) {
	c, err := LoadCassette("testdata/notes_contract.json")
	if err != nil {
		t.Fatal(err)
	}
	rs := NewReplayServer(t, c)
	client := rs.server.Client()

	status, body := getBody(t, client, rs.URL()+"/notes?page=1")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !contains([]byte(body), "n-1") || !contains([]byte(body), "n-2") {
		t.Errorf("page 1 body unexpected: %s", body)
	}
}

func TestReplayServerHeaders(t *testing.T) {
	c, err := LoadCassette("testdata/binary.json")
	if err != nil {
		t.Fatal(err)
	}
	rs := NewReplayServer(t, c)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, rs.URL()+"/blob", nil)
	resp, err := rs.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 11 {
		t.Errorf("binary body length = %d, want 11", len(body))
	}
}

// TestReplayServerUnmatchedFailsTest proves an unmatched request both responds
// 598 and registers a miss. We capture the miss directly (the t.Cleanup failure
// path is covered by TestReplayServerCleanupFlagsMisses below).
func TestReplayServerUnmatchedRecordsMiss(t *testing.T) {
	c := &Cassette{Name: "empty", Interactions: nil}
	// Build the server WITHOUT NewReplayServer's failing cleanup, so this test
	// itself does not fail when we deliberately miss.
	rs := newBareReplayServer(t, c)

	status, _ := getBody(t, rs.server.Client(), rs.URL()+"/anything?x=1")
	if status != statusReplayMiss {
		t.Errorf("unmatched request status = %d, want %d", status, statusReplayMiss)
	}
	misses := rs.Misses()
	if len(misses) != 1 {
		t.Fatalf("recorded %d misses, want 1", len(misses))
	}
	if misses[0].Path != "/anything" || misses[0].Query != "x=1" {
		t.Errorf("miss = %+v, want path /anything query x=1", misses[0])
	}
}

// TestReplayServerCleanupFlagsMisses proves NewReplayServer's cleanup turns a
// miss into a test failure, by running a sub-test whose failure we capture.
func TestReplayServerCleanupFlagsMisses(t *testing.T) {
	inner := &capturingTB{TB: t}
	func() {
		// A nested run so the registered cleanup fires against inner.
		rs := NewReplayServer(inner, &Cassette{Name: "empty"})
		_, _ = getBody(t, rs.server.Client(), rs.URL()+"/missing")
		inner.runCleanups()
	}()
	if !inner.failed {
		t.Error("replay server cleanup did not fail the test on an unmatched request")
	}
	if !inner.contains("unmatched request") {
		t.Errorf("failure message did not mention the miss: %v", inner.errors)
	}
}

func TestReplayTransportServesAndRecordsMisses(t *testing.T) {
	c, err := LoadCassette("testdata/sequence.json")
	if err != nil {
		t.Fatal(err)
	}
	rt := NewReplayTransport(t, c)
	client := &http.Client{Transport: rt}

	// Any host works; the transport ignores it and matches by path.
	status, body := getBody(t, client, "http://ignored.example/poll")
	if status != http.StatusOK || body != "first" {
		t.Errorf("first poll = (%d, %q), want (200, first)", status, body)
	}
	_, body2 := getBody(t, client, "http://ignored.example/poll")
	if body2 != "second" {
		t.Errorf("second poll = %q, want second", body2)
	}
}

// TestReplayTransportPostBody covers a request with a body flowing through the
// transport (the body-read + body-pin matching path).
func TestReplayTransportPostBody(t *testing.T) {
	cas := &Cassette{Name: "post", Interactions: []*Interaction{
		{Request: RecordedRequest{Method: "POST", Path: "/submit", Body: `{"v":1}`},
			Response: RecordedResponse{Status: 201, Body: "created"}},
	}}
	rt := NewReplayTransport(t, cas)
	client := &http.Client{Transport: rt}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://ignored.example/submit", strings.NewReader(`{"v":1}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "created" {
		t.Errorf("body = %q, want created", body)
	}
}

func TestReplayTransportUnmatchedRecordsMiss(t *testing.T) {
	rt := &ReplayTransport{tb: t, cassette: &Cassette{Name: "empty"}}
	client := &http.Client{Transport: rt}
	status, _ := getBody(t, client, "http://ignored.example/none")
	if status != statusReplayMiss {
		t.Errorf("status = %d, want %d", status, statusReplayMiss)
	}
	if len(rt.Misses()) != 1 {
		t.Errorf("recorded %d misses, want 1", len(rt.Misses()))
	}
}

// newBareReplayServer builds a ReplayServer without the failing cleanup, for
// tests that intend to miss the cassette and inspect rs.Misses() directly.
func newBareReplayServer(t *testing.T, c *Cassette) *ReplayServer {
	t.Helper()
	rs := &ReplayServer{tb: t, cassette: c}
	rs.server = newTestServer(rs.serve)
	t.Cleanup(rs.server.Close)
	return rs
}

func TestMissString(t *testing.T) {
	if got := (Miss{Method: "GET", Path: "/x"}).String(); got != "GET /x" {
		t.Errorf("Miss.String() = %q, want GET /x", got)
	}
	if got := (Miss{Method: "GET", Path: "/x", Query: "a=1"}).String(); got != "GET /x?a=1" {
		t.Errorf("Miss.String() with query = %q, want GET /x?a=1", got)
	}
}

func TestTruncateBody(t *testing.T) {
	if got := truncateBody(nil); got != "" {
		t.Errorf("truncateBody(nil) = %q, want empty", got)
	}
	if got := truncateBody([]byte("short")); got != "short" {
		t.Errorf("truncateBody(short) = %q", got)
	}
	long := make([]byte, 300)
	for i := range long {
		long[i] = 'a'
	}
	got := truncateBody(long)
	if len(got) <= 256 || got[len(got)-len("...(truncated)"):] != "...(truncated)" {
		t.Errorf("truncateBody(long) not truncated: len=%d", len(got))
	}
}

// TestReplayServerNilCassette proves NewReplayServer fatals on a nil cassette.
func TestReplayServerNilCassette(t *testing.T) {
	inner := &capturingTB{TB: t}
	NewReplayServer(inner, nil)
	if !inner.failed {
		t.Error("NewReplayServer(nil) did not fail")
	}
}

// TestReplayTransportNilCassette proves NewReplayTransport fatals on nil.
func TestReplayTransportNilCassette(t *testing.T) {
	inner := &capturingTB{TB: t}
	NewReplayTransport(inner, nil)
	if !inner.failed {
		t.Error("NewReplayTransport(nil) did not fail")
	}
}
