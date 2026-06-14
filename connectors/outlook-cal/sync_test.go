package outlookcal

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

// syncCfg builds an sdk.Config pointing base_url at the replay server URL.
func syncCfg(baseURL string) sdk.Config {
	tcx, err := tenancy.FromClaims(map[string]any{"tenant_id": "tenant-sync", "sub": "user-sync"})
	if err != nil {
		panic(err)
	}
	return sdk.Config{
		Tenant:     tcx,
		InstanceID: "inst-sync",
		ConfigJSON: []byte(`{"base_url":"` + baseURL + `"}`),
		Token:      []byte("tok"),
		Checkpoint: sdk.NopCheckpoint,
	}
}

func TestFullSyncBadConfig(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	cfg := sdk.Config{ConfigJSON: []byte(`{`), Checkpoint: sdk.NopCheckpoint}
	if _, err := c.FullSync(context.Background(), cfg, discardEmit); err == nil {
		t.Fatal("FullSync accepted a malformed config")
	}
}

func TestIncrementalSyncBadCursorIsExpired(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	cfg := syncCfg("http://unused.invalid")
	_, err := c.IncrementalSync(context.Background(), cfg, sdk.Cursor("garbage-cursor"), discardEmit)
	if !errors.Is(err, sdk.ErrCursorExpired) {
		t.Fatalf("err = %v, want sdk.ErrCursorExpired for an unparseable cursor", err)
	}
}

func TestIncrementalSyncBadConfig(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	cfg := sdk.Config{ConfigJSON: []byte(`{`), Checkpoint: sdk.NopCheckpoint}
	if _, err := c.IncrementalSync(context.Background(), cfg, deltaCursor("x"), discardEmit); err == nil {
		t.Fatal("IncrementalSync accepted a malformed config")
	}
}

func TestPrimeDeltaMalformedPage(t *testing.T) {
	t.Parallel()
	// A delta page with neither nextLink nor deltaLink is a malformed source
	// response; FullSync must error rather than loop forever.
	cas := &connectortest.Cassette{Interactions: []*connectortest.Interaction{
		{
			Request:  connectortest.RecordedRequest{Method: "GET", Path: "/me/calendarView/delta", Query: deltaQuery()},
			Response: connectortest.RecordedResponse{Status: 200, Body: `{"value":[]}`},
		},
	}}
	rs := connectortest.NewReplayServer(t, cas)
	c := newTestConnector()
	_, err := c.FullSync(context.Background(), syncCfg(rs.URL()), discardEmit)
	if err == nil || !strings.Contains(err.Error(), "neither nextLink nor deltaLink") {
		t.Fatalf("err = %v, want malformed-delta-page error", err)
	}
}

func TestReplayDeltaMalformedPage(t *testing.T) {
	t.Parallel()
	cas := &connectortest.Cassette{Interactions: []*connectortest.Interaction{
		{
			Request:  connectortest.RecordedRequest{Method: "GET", Path: "/me/calendarView/delta", Query: "$deltatoken=T"},
			Response: connectortest.RecordedResponse{Status: 200, Body: `{"value":[{"id":"E1","subject":"x"}]}`},
		},
	}}
	rs := connectortest.NewReplayServer(t, cas)
	c := newTestConnector()
	cur := deltaCursor("https://graph.microsoft.com/v1.0/me/calendarView/delta?$deltatoken=T")
	_, err := c.IncrementalSync(context.Background(), syncCfg(rs.URL()), cur, discardEmit)
	if err == nil || !strings.Contains(err.Error(), "neither nextLink nor deltaLink") {
		t.Fatalf("err = %v, want malformed-delta-page error", err)
	}
}

func TestFullSyncContextCanceled(t *testing.T) {
	t.Parallel()
	// A primed delta then a canceled context: backfill must stop promptly.
	cas := &connectortest.Cassette{Interactions: []*connectortest.Interaction{
		{
			Request:  connectortest.RecordedRequest{Method: "GET", Path: "/me/calendarView/delta"},
			Response: connectortest.RecordedResponse{Status: 200, Body: `{"value":[],"@odata.deltaLink":"https://graph.microsoft.com/v1.0/me/calendarView/delta?$deltatoken=D"}`},
		},
	}}
	rs := connectortest.NewReplayServer(t, cas)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := newTestConnector()
	if _, err := c.FullSync(ctx, syncCfg(rs.URL()), discardEmit); err == nil {
		t.Fatal("FullSync ignored a canceled context")
	}
}

// discardEmit is an Emit that drops documents (for error-path tests).
func discardEmit(_ context.Context, _ *askerv1.Document) error { return nil }
