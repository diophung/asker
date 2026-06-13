package slack

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/asker/asker/connectors/sdk/connectortest"
)

// postSigned builds a signed POST request for body at the connector's fixed
// clock time and runs HandleWebhook, returning the recorder and error.
func postSigned(t *testing.T, c *Connector, secret string, body []byte) (*connectortest.EmitRecorder, error) {
	t.Helper()
	ts := strconv.FormatInt(fixedNow.Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/slack/inst-1", bytes.NewReader(body))
	req.Header.Set(timestampHeader, ts)
	req.Header.Set(signatureHeader, computeSignature(secret, ts, body))

	cfg := testConfig(t, "http://unused.invalid", testSigningSecret)
	var rec connectortest.EmitRecorder
	err := c.HandleWebhook(context.Background(), cfg, req, rec.Emit)
	return &rec, err
}

func TestWebhookMessageCreate(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	body := []byte(`{"type":"event_callback","team_id":"T1","event":{"type":"message","channel":"C100","user":"U_ALICE","text":"hello world","ts":"1700000700.000700","event_ts":"1700000700.000700","team":"T1"}}`)
	rec, err := postSigned(t, c, testSigningSecret, body)
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	docs := rec.Docs()
	if len(docs) != 1 {
		t.Fatalf("emitted %d docs, want 1", len(docs))
	}
	if docs[0].GetDocId() != docID("C100", "1700000700.000700") {
		t.Errorf("doc_id = %q", docs[0].GetDocId())
	}
	if docs[0].GetBodyText() != "hello world" {
		t.Errorf("body = %q", docs[0].GetBodyText())
	}
}

func TestWebhookMessageChangedUpsert(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	body := []byte(`{"type":"event_callback","team_id":"T1","event":{"type":"message","subtype":"message_changed","channel":"C100","event_ts":"1700000800.000800","message":{"type":"message","user":"U_ALICE","text":"edited body","ts":"1700000700.000700","edited":{"user":"U_ALICE","ts":"1700000800.000800"},"team":"T1"}}}`)
	rec, err := postSigned(t, c, testSigningSecret, body)
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	docs := rec.Docs()
	if len(docs) != 1 {
		t.Fatalf("emitted %d docs, want 1", len(docs))
	}
	// Same doc_id as the original message ts; new etag from edited.ts.
	if docs[0].GetDocId() != docID("C100", "1700000700.000700") {
		t.Errorf("changed doc_id = %q, want the original message's", docs[0].GetDocId())
	}
	if docs[0].GetVersionEtag() != "1700000800.000800" {
		t.Errorf("changed etag = %q, want edited.ts", docs[0].GetVersionEtag())
	}
}

func TestWebhookMessageDeletedTombstone(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	body := []byte(`{"type":"event_callback","team_id":"T1","event":{"type":"message","subtype":"message_deleted","channel":"C100","deleted_ts":"1700000200.000200","event_ts":"1700000900.000900","ts":"1700000900.000900"}}`)
	rec, err := postSigned(t, c, testSigningSecret, body)
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	docs := rec.Docs()
	if len(docs) != 1 {
		t.Fatalf("emitted %d docs, want 1", len(docs))
	}
	if !docs[0].GetTombstone().GetDeleted() {
		t.Error("delete must emit a tombstone")
	}
	if docs[0].GetDocId() != docID("C100", "1700000200.000200") {
		t.Errorf("tombstone doc_id = %q, want the deleted message's", docs[0].GetDocId())
	}
	if docs[0].GetBodyText() != "" {
		t.Error("tombstone must have no body")
	}
}

func TestWebhookURLVerificationEmitsNothing(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	body := []byte(`{"type":"url_verification","token":"x","challenge":"abc123"}`)
	rec, err := postSigned(t, c, testSigningSecret, body)
	if err != nil {
		t.Fatalf("HandleWebhook(url_verification): %v", err)
	}
	if n := len(rec.Docs()); n != 0 {
		t.Errorf("url_verification emitted %d docs, want 0 (hub answers the challenge)", n)
	}
}

func TestWebhookIgnoresNonMessageEvents(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	for _, body := range [][]byte{
		[]byte(`{"type":"event_callback","event":{"type":"reaction_added","user":"U","reaction":"tada"}}`),
		[]byte(`{"type":"event_callback","event":{"type":"message","subtype":"channel_join","channel":"C100","user":"U","ts":"1.0"}}`),
	} {
		rec, err := postSigned(t, c, testSigningSecret, body)
		if err != nil {
			t.Fatalf("HandleWebhook: %v", err)
		}
		if n := len(rec.Docs()); n != 0 {
			t.Errorf("non-indexable event emitted %d docs, want 0 (body %s)", n, body)
		}
	}
}

func TestWebhookSignatureRejection(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	body := []byte(`{"type":"event_callback","event":{"type":"message","channel":"C1","user":"U","text":"x","ts":"1700000700.000700"}}`)

	t.Run("wrong secret", func(t *testing.T) {
		t.Parallel()
		if _, err := postSigned(t, c, "the-wrong-secret", body); err == nil {
			t.Fatal("HandleWebhook accepted a signature from the wrong secret")
		}
	})

	t.Run("missing signature header", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodPost, "/w", bytes.NewReader(body))
		req.Header.Set(timestampHeader, strconv.FormatInt(fixedNow.Unix(), 10))
		cfg := testConfig(t, "http://unused.invalid", testSigningSecret)
		if err := c.HandleWebhook(context.Background(), cfg, req, emit); err == nil {
			t.Fatal("HandleWebhook accepted a request with no signature")
		}
	})

	t.Run("stale timestamp", func(t *testing.T) {
		t.Parallel()
		stale := strconv.FormatInt(fixedNow.Add(-time.Hour).Unix(), 10)
		req := httptest.NewRequest(http.MethodPost, "/w", bytes.NewReader(body))
		req.Header.Set(timestampHeader, stale)
		req.Header.Set(signatureHeader, computeSignature(testSigningSecret, stale, body))
		cfg := testConfig(t, "http://unused.invalid", testSigningSecret)
		if err := c.HandleWebhook(context.Background(), cfg, req, emit); err == nil {
			t.Fatal("HandleWebhook accepted a stale (replayable) timestamp")
		}
	})

	t.Run("no signing secret configured", func(t *testing.T) {
		t.Parallel()
		ts := strconv.FormatInt(fixedNow.Unix(), 10)
		req := httptest.NewRequest(http.MethodPost, "/w", bytes.NewReader(body))
		req.Header.Set(timestampHeader, ts)
		req.Header.Set(signatureHeader, computeSignature(testSigningSecret, ts, body))
		cfg := testConfig(t, "http://unused.invalid", "") // no signing_secret
		if err := c.HandleWebhook(context.Background(), cfg, req, emit); err == nil {
			t.Fatal("HandleWebhook verified a request without a configured signing secret")
		}
	})
}
