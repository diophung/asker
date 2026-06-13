package slack

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

// Test fixtures shared across the slack connector test suite.
const (
	testTenant = "tenant-a"
	testToken  = "xoxb-test-token"
	// testSigningSecret signs webhook fixtures (the same secret the connector
	// verifies against). It is a throwaway test value, never a real Slack
	// signing secret.
	testSigningSecret = "8f742231b10e8888abcd99yyyzzz85a5"
)

// fixedNow is a deterministic clock pinned near the fixture timestamps so
// tombstone deleted_at and webhook timestamp-skew checks are stable.
// 1700000040 unix == 2023-11-14T22:14:00Z.
var fixedNow = time.Date(2023, 11, 14, 22, 14, 0, 0, time.UTC)

// newTestConnector returns a *Connector with a quiet logger and a fixed clock.
func newTestConnector() *Connector {
	c := New(WithLogger(slog.New(slog.DiscardHandler))).(*Connector)
	c.now = func() time.Time { return fixedNow }
	return c
}

// testConfig builds an sdk.Config pointing the connector at baseURL.
func testConfig(t *testing.T, baseURL, signingSecret string) sdk.Config {
	t.Helper()
	tc, err := tenancy.FromClaims(map[string]any{"tenant_id": testTenant, "sub": "user-a"})
	if err != nil {
		t.Fatalf("tenancy.FromClaims: %v", err)
	}
	conf := map[string]string{"base_url": baseURL}
	if signingSecret != "" {
		conf["signing_secret"] = signingSecret
	}
	raw, err := json.Marshal(conf)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return sdk.Config{
		Tenant:     tc,
		InstanceID: "inst-1",
		ConfigJSON: raw,
		Token:      []byte(testToken),
		Checkpoint: sdk.NopCheckpoint,
	}
}

// docID is the doc_id a "<channel>:<ts>" native id maps to, for expectations.
func docID(channel, ts string) string {
	return sdk.DocID(connectorID, nativeID(channel, ts))
}

// emit is a discarding sdk.Emit for passes whose emissions are asserted via a
// recorder or must be zero.
func emit(_ context.Context, _ *askerv1.Document) error { return nil }
