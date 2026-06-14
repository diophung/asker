package connectortest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordingEnabled(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"0":     false,
		"false": false,
		"FALSE": false,
		"1":     true,
		"true":  true,
		"yes":   true,
	}
	for v, want := range cases {
		t.Setenv(recordEnv, v)
		if got := recordingEnabled(); got != want {
			t.Errorf("recordingEnabled() with %s=%q = %v, want %v", recordEnv, v, got, want)
		}
	}
}

func TestNewRecorderReplayMode(t *testing.T) {
	// Ensure record mode is off, then NewRecorder must replay an existing
	// cassette exactly like NewReplayServer.
	t.Setenv(recordEnv, "")
	rec := NewRecorder(t, "http://unused.example", "testdata/notes_contract.json")
	status, body := getBody(t, http.DefaultClient, rec.URL()+"/notes?page=1")
	if status != http.StatusOK || !contains([]byte(body), "n-1") {
		t.Errorf("replay via NewRecorder failed: status=%d body=%s", status, body)
	}
}

// TestRecorderRecordsAndRedacts records against a fake upstream with
// ASKER_RECORD=1, then asserts the on-disk cassette replays and that the
// Authorization header never reached the file (it is a request header we do not
// persist, and Set-Cookie is redacted).
func TestRecorderRecordsAndRedacts(t *testing.T) {
	t.Setenv(recordEnv, "1")

	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=supersecret; HttpOnly")
		_, _ = io.WriteString(w, `{"ok":true,"id":"`+r.URL.Query().Get("id")+`"}`)
	}))
	defer upstream.Close()

	cassettePath := filepath.Join(t.TempDir(), "recorded.json")
	inner := &capturingTB{TB: t}
	rec := NewRecorder(inner, upstream.URL, cassettePath)

	// Drive a request carrying a secret bearer token through the proxy.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, rec.URL()+"/thing?id=42", nil)
	req.Header.Set("Authorization", "Bearer super-secret-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !contains(body, `"id":"42"`) {
		t.Errorf("proxied response unexpected: %s", body)
	}
	if gotAuth != "Bearer super-secret-token" {
		t.Errorf("upstream did not receive the bearer token (got %q); the proxy must forward it", gotAuth)
	}

	// Flush the cassette by running the recorder's cleanup.
	inner.runCleanups()
	if inner.failed {
		t.Fatalf("recorder cleanup failed: %v", inner.errors)
	}

	// The on-disk cassette must contain no secret and must replay.
	raw, err := readFile(cassettePath)
	if err != nil {
		t.Fatal(err)
	}
	if contains(raw, "super-secret-token") {
		t.Error("recorded cassette leaked the bearer token")
	}
	if contains(raw, "supersecret") {
		t.Error("recorded cassette leaked the Set-Cookie session value")
	}
	if !contains(raw, "REDACTED") {
		t.Error("recorded cassette did not redact Set-Cookie")
	}

	c, err := LoadCassette(cassettePath)
	if err != nil {
		t.Fatalf("recorded cassette does not load: %v", err)
	}
	if len(c.Interactions) != 1 {
		t.Fatalf("recorded %d interactions, want 1", len(c.Interactions))
	}
	in := c.Interactions[0]
	if in.Request.Path != "/thing" || in.Request.Query != "id=42" {
		t.Errorf("recorded request = %s %s?%s, want GET /thing?id=42", in.Request.Method, in.Request.Path, in.Request.Query)
	}
}

// TestRecorderCapturesBinary records a non-UTF-8 upstream body and asserts it is
// stored as body_base64 and round-trips byte-for-byte.
func TestRecorderCapturesBinary(t *testing.T) {
	t.Setenv(recordEnv, "1")
	want := []byte{0x00, 0x01, 0xff, 0xfe, 0x80}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(want)
	}))
	defer upstream.Close()

	cassettePath := filepath.Join(t.TempDir(), "bin.json")
	inner := &capturingTB{TB: t}
	rec := NewRecorder(inner, upstream.URL, cassettePath)
	_, _ = getBody(t, http.DefaultClient, rec.URL()+"/blob")
	inner.runCleanups()

	c, err := LoadCassette(cassettePath)
	if err != nil {
		t.Fatal(err)
	}
	if c.Interactions[0].Response.BodyBase64 == "" {
		t.Error("binary body was not stored as body_base64")
	}
	got, err := c.Interactions[0].Response.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("binary body round-trip = % x, want % x", got, want)
	}
}

// TestRecordingTransportReplayMode covers the RoundTripper twin in replay mode.
func TestRecordingTransportReplayMode(t *testing.T) {
	t.Setenv(recordEnv, "")
	rt := NewRecordingTransport(t, nil, "testdata/sequence.json")
	client := &http.Client{Transport: rt}
	_, body := getBody(t, client, "http://ignored.example/poll")
	if body != "first" {
		t.Errorf("replay transport body = %q, want first", body)
	}
}

// TestRecordingTransportRecordMode records through a base transport pointed at a
// fake upstream and asserts the cassette is written.
func TestRecordingTransportRecordMode(t *testing.T) {
	t.Setenv(recordEnv, "1")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "recorded-body")
	}))
	defer upstream.Close()

	cassettePath := filepath.Join(t.TempDir(), "rt.json")
	inner := &capturingTB{TB: t}
	rt := NewRecordingTransport(inner, http.DefaultTransport, cassettePath)
	client := &http.Client{Transport: rt}
	_, body := getBody(t, client, upstream.URL+"/path")
	if body != "recorded-body" {
		t.Errorf("record-mode body = %q, want recorded-body", body)
	}
	inner.runCleanups()

	c, err := LoadCassette(cassettePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Interactions) != 1 || c.Interactions[0].Request.Path != "/path" {
		t.Errorf("unexpected recorded interactions: %+v", c.Interactions)
	}
}

// TestRecordingTransportRecordModePostBody covers the record-mode RoundTrip
// path where the request carries a body (it must be buffered, forwarded, and
// restored).
func TestRecordingTransportRecordModePostBody(t *testing.T) {
	t.Setenv(recordEnv, "1")
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, "ack")
	}))
	defer upstream.Close()

	cassettePath := filepath.Join(t.TempDir(), "post.json")
	inner := &capturingTB{TB: t}
	rt := NewRecordingTransport(inner, http.DefaultTransport, cassettePath)
	client := &http.Client{Transport: rt}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		upstream.URL+"/submit", strings.NewReader(`{"v":7}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if gotBody != `{"v":7}` {
		t.Errorf("upstream got body %q, want {\"v\":7}", gotBody)
	}
	inner.runCleanups()
	if _, err := LoadCassette(cassettePath); err != nil {
		t.Errorf("recorded cassette does not load: %v", err)
	}
}

func TestRedactRequestHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer secret")
	h.Set("X-Api-Key", "key123")
	h.Set("Accept", "application/json")
	got := redactRequestHeaders(h)
	if got["Authorization"][0] != redactedValue {
		t.Errorf("Authorization not redacted: %v", got["Authorization"])
	}
	if got["X-Api-Key"][0] != redactedValue {
		t.Errorf("X-Api-Key not redacted: %v", got["X-Api-Key"])
	}
	if got["Accept"][0] != "application/json" {
		t.Errorf("Accept wrongly redacted: %v", got["Accept"])
	}
}

func TestSingleJoin(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"", "/x", "/x"},
		{"/", "/x", "/x"},
		{"/base", "/x", "/base/x"},
		{"/base/", "/x", "/base/x"},
		{"/base", "x", "/base/x"},
	}
	for _, c := range cases {
		if got := singleJoin(c.a, c.b); got != c.want {
			t.Errorf("singleJoin(%q,%q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}
