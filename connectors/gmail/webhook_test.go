package gmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asker/asker/connectors/sdk/connectortest"
)

// pushReceiver captures webhook deliveries from the fake's push shim.
type pushReceiver struct {
	ts     *httptest.Server
	bodies chan []byte
}

func newPushReceiver(t *testing.T) *pushReceiver {
	t.Helper()
	r := &pushReceiver{bodies: make(chan []byte, 10)}
	r.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(req.Body); err != nil {
			t.Errorf("read push body: %v", err)
		}
		r.bodies <- buf.Bytes()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(r.ts.Close)
	return r
}

func (r *pushReceiver) wait(t *testing.T) []byte {
	t.Helper()
	select {
	case body := <-r.bodies:
		return body
	case <-time.After(5 * time.Second):
		t.Fatal("push notification not delivered within 5s")
		return nil
	}
}

// TestWatchRegistrationAndWebhook proves FullSync registers users.watch with
// the hub webhook URL as the push target (X-Asker-Push-Url shim), and that
// the resulting real push envelope passes HandleWebhook.
func TestWatchRegistrationAndWebhook(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	recv := newPushReceiver(t)
	webhookURL := recv.ts.URL + "/webhooks/gmail/inst-1"
	cfg := f.connectorConfig(webhookURL, nil)
	c := newTestConnector()

	if _, err := c.FullSync(context.Background(), cfg, sdkNopEmit); err != nil {
		t.Fatalf("FullSync: %v", err)
	}

	// A mailbox change must now be pushed to the webhook URL.
	f.addMessage("ava@example.com", testEmail, "watched", "a change")
	body := recv.wait(t)

	// The hub routes that request to HandleWebhook; a nil return means
	// "verified — trigger the incremental sync".
	req := httptest.NewRequest(http.MethodPost, webhookURL, bytes.NewReader(body))
	var rec connectortest.EmitRecorder
	if err := c.HandleWebhook(context.Background(), cfg, req, rec.Emit); err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if n := len(rec.Docs()); n != 0 {
		t.Errorf("HandleWebhook emitted %d documents; Gmail pushes are notification-only", n)
	}
}

// TestNoWatchWithoutWebhookURL: a poll-only instance must not register a
// push target.
func TestNoWatchWithoutWebhookURL(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	recv := newPushReceiver(t)
	cfg := f.connectorConfig("", nil) // no webhook_url in config
	c := newTestConnector()

	if _, err := c.FullSync(context.Background(), cfg, sdkNopEmit); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	f.addMessage("ava@example.com", testEmail, "unwatched", "a change")

	select {
	case <-recv.bodies:
		t.Fatal("push delivered although no webhook_url was configured")
	case <-time.After(300 * time.Millisecond):
	}
}

func validEnvelope(t *testing.T, email string, historyID uint64) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"emailAddress": email, "historyId": historyID})
	if err != nil {
		t.Fatal(err)
	}
	env, err := json.Marshal(map[string]any{
		"message": map[string]any{
			"data":        base64.StdEncoding.EncodeToString(data),
			"messageId":   "push-1",
			"publishTime": "2026-06-12T00:00:00Z",
		},
		"subscription": "projects/fake-gmail/subscriptions/asker-dev",
	})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// TestHandleWebhookValidation covers envelope-shape and identity checks.
func TestHandleWebhookValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := f.connectorConfig("", nil)
	c := newTestConnector()
	ctx := context.Background()

	post := func(body []byte) error {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/gmail/inst-1", bytes.NewReader(body))
		return c.HandleWebhook(ctx, cfg, req, sdkNopEmit)
	}

	if err := post(validEnvelope(t, testEmail, 42)); err != nil {
		t.Errorf("valid envelope rejected: %v", err)
	}
	if err := post(validEnvelope(t, strings.ToUpper(testEmail), 42)); err != nil {
		t.Errorf("email comparison must be case-insensitive: %v", err)
	}

	cases := map[string][]byte{
		"not JSON":        []byte("pub/sub? never heard of it"),
		"no message.data": []byte(`{"message":{"messageId":"1"},"subscription":"s"}`),
		"data not base64": []byte(`{"message":{"data":"!!!not-base64!!!"},"subscription":"s"}`),
		"data not JSON":   []byte(`{"message":{"data":"` + base64.StdEncoding.EncodeToString([]byte("plain text")) + `"},"subscription":"s"}`),
		"no emailAddress": []byte(`{"message":{"data":"` + base64.StdEncoding.EncodeToString([]byte(`{"historyId":7}`)) + `"},"subscription":"s"}`),
		"wrong mailbox":   validEnvelope(t, "mallory@example.com", 42),
	}
	for name, body := range cases {
		if err := post(body); err == nil {
			t.Errorf("%s: HandleWebhook accepted invalid payload %q", name, body)
		}
	}

	// A broken instance config is also a rejection.
	badCfg := cfg
	badCfg.ConfigJSON = []byte("{")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/gmail/inst-1", bytes.NewReader(validEnvelope(t, testEmail, 1)))
	if err := c.HandleWebhook(ctx, badCfg, req, sdkNopEmit); err == nil {
		t.Error("HandleWebhook accepted an unparseable instance config")
	}
}
