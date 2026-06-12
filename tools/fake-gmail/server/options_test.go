package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWithClockAndPushClient(t *testing.T) {
	t.Parallel()

	fixed := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	pushed := make(chan *http.Request, 1)
	pushClient := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		pushed <- r
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       http.NoBody,
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})}

	srv := New(
		WithLogger(testLogger(t)),
		WithClock(func() time.Time { return fixed }),
		WithPushClient(pushClient),
	)
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		ts.Close()
		srv.Close()
	})
	f := &fixture{t: t, srv: srv, ts: ts}

	// Watch: expiration must be derived from the injected clock.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/gmail/v1/users/me/watch",
		strings.NewReader(`{"topicName":"projects/fake/topics/asker"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set(pushURLHeader, "http://push.invalid/notify")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var watch wireWatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&watch); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if want := fixed.Add(watchTTL).UnixMilli(); watch.Expiration != want {
		t.Fatalf("expiration = %d, want %d (fixed clock + TTL)", watch.Expiration, want)
	}

	// A change is pushed through the injected HTTP client.
	f.addMessage(testEmail, "clocked", "x")
	select {
	case r := <-pushed:
		if r.URL.Host != "push.invalid" {
			t.Fatalf("push went to %q", r.URL.Host)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("push not delivered through injected client")
	}

	// Messages created without an explicit date inherit the fixed clock.
	var msg wireMessage
	var list wireListMessagesResponse
	if code := f.do(http.MethodGet, "/gmail/v1/users/me/messages", testToken, nil, &list); code != http.StatusOK {
		t.Fatalf("list: status %d", code)
	}
	if code := f.do(http.MethodGet, "/gmail/v1/users/me/messages/"+list.Messages[0].ID, testToken, nil, &msg); code != http.StatusOK {
		t.Fatalf("get: status %d", code)
	}
	if msg.InternalDate != fixed.UnixMilli() {
		t.Fatalf("internalDate = %d, want fixed clock %d", msg.InternalDate, fixed.UnixMilli())
	}
}
