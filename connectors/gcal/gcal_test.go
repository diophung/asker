package gcal

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
)

const testToken = "ya29.fake-access-token"

// newTestConnector returns a connector with a quiet logger.
func newTestConnector() sdk.Connector {
	return New(WithLogger(slog.New(slog.DiscardHandler)))
}

func TestSpec(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	connectortest.RunSpecChecks(t, c)

	spec := c.Spec()
	if spec.ID != "gcal" {
		t.Errorf("spec.ID = %q, want %q", spec.ID, "gcal")
	}
	if spec.AuthType != sdk.AuthOAuth2 {
		t.Errorf("spec.AuthType = %v, want AuthOAuth2", spec.AuthType)
	}
	if spec.SupportsWebhook {
		t.Error("spec.SupportsWebhook = true, want false (push channels need a public URL)")
	}

	var schema struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(spec.ConfigSchema, &schema); err != nil {
		t.Fatalf("ConfigSchema does not parse: %v", err)
	}
	if schema.Type != "object" {
		t.Errorf("schema type = %q, want object", schema.Type)
	}
	for _, prop := range []string{"base_url", "calendar_id"} {
		if p, ok := schema.Properties[prop]; !ok || p.Type != "string" {
			t.Errorf("schema property %q = %+v, want string property", prop, p)
		}
	}
}

func TestParseConfig(t *testing.T) {
	t.Parallel()

	t.Run("defaults", func(t *testing.T) {
		t.Parallel()
		conf, err := parseConfig([]byte(`{}`))
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		if conf.CalendarID != defaultCalendarID {
			t.Errorf("calendar_id = %q, want %q", conf.CalendarID, defaultCalendarID)
		}
		if conf.BaseURL != defaultBaseURL {
			t.Errorf("base_url = %q, want %q", conf.BaseURL, defaultBaseURL)
		}
	})

	t.Run("empty config uses defaults", func(t *testing.T) {
		t.Parallel()
		conf, err := parseConfig(nil)
		if err != nil {
			t.Fatalf("parseConfig(nil): %v", err)
		}
		if conf.CalendarID != defaultCalendarID || conf.BaseURL != defaultBaseURL {
			t.Errorf("parseConfig(nil) = %+v, want defaults", conf)
		}
	})

	t.Run("explicit calendar and base", func(t *testing.T) {
		t.Parallel()
		conf, err := parseConfig([]byte(`{"calendar_id":"team@group.calendar.google.com","base_url":"https://example.test/calendar/v3"}`))
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		if conf.CalendarID != "team@group.calendar.google.com" {
			t.Errorf("calendar_id = %q", conf.CalendarID)
		}
		if conf.BaseURL != "https://example.test/calendar/v3" {
			t.Errorf("base_url = %q", conf.BaseURL)
		}
	})

	t.Run("errors", func(t *testing.T) {
		t.Parallel()
		for name, raw := range map[string]string{
			"not json":     "{",
			"bad base_url": `{"base_url":"::not-a-url"}`,
			"ftp base_url": `{"base_url":"ftp://host/x"}`,
		} {
			if _, err := parseConfig([]byte(raw)); err == nil {
				t.Errorf("parseConfig accepted %s config %q", name, raw)
			}
		}
	})
}

func TestValidate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newTestConnector()

	t.Run("no token skips round-trip", func(t *testing.T) {
		t.Parallel()
		cfg := sdk.Config{ConfigJSON: []byte(`{}`), Checkpoint: sdk.NopCheckpoint}
		if err := c.Validate(ctx, cfg); err != nil {
			t.Fatalf("Validate without token: %v", err)
		}
	})

	t.Run("config error surfaces", func(t *testing.T) {
		t.Parallel()
		cfg := sdk.Config{ConfigJSON: []byte(`{"base_url":"::bad"}`), Token: []byte(testToken), Checkpoint: sdk.NopCheckpoint}
		if err := c.Validate(ctx, cfg); err == nil {
			t.Fatal("Validate accepted a bad base_url")
		}
	})

	t.Run("ok with token", func(t *testing.T) {
		t.Parallel()
		cas, err := connectortest.LoadCassette("testdata/validate.json")
		if err != nil {
			t.Fatalf("LoadCassette: %v", err)
		}
		rs := connectortest.NewReplayServer(t, cas)
		cfg := sdk.Config{
			ConfigJSON: []byte(`{"base_url":"` + rs.URL() + `"}`),
			Token:      []byte(testToken),
			Checkpoint: sdk.NopCheckpoint,
		}
		if err := c.Validate(ctx, cfg); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("bad credential", func(t *testing.T) {
		t.Parallel()
		// Reuse the same cassette: the first probe is consumed by the "ok"
		// subtest's server, so this subtest spins up its own server and hits
		// the 401 interaction by consuming the 200 first.
		cas, err := connectortest.LoadCassette("testdata/validate.json")
		if err != nil {
			t.Fatalf("LoadCassette: %v", err)
		}
		rs := connectortest.NewReplayServer(t, cas)
		cfg := sdk.Config{
			ConfigJSON: []byte(`{"base_url":"` + rs.URL() + `"}`),
			Token:      []byte("bad"),
			Checkpoint: sdk.NopCheckpoint,
		}
		// First call consumes the 200 (the cassette's ok interaction).
		if err := c.Validate(ctx, cfg); err != nil {
			t.Fatalf("first Validate: %v", err)
		}
		// Second call hits the 401 interaction and must fail with a
		// credential-free message.
		err = c.Validate(ctx, cfg)
		if err == nil {
			t.Fatal("Validate accepted an invalid credential")
		}
		if strings.Contains(err.Error(), "bad") {
			t.Errorf("Validate error leaks token material: %v", err)
		}
	})
}

func TestContract(t *testing.T) {
	t.Parallel()

	docID := func(eventID string) string {
		return sdk.DocID(connectorID, nativeID(defaultCalendarID, eventID))
	}

	t.Run("fullsync", func(t *testing.T) {
		t.Parallel()
		afterBackfill := syncCursor("SYNCTOKEN-AFTER-BACKFILL")
		connectortest.RunConnectorContract(t, newTestConnector(), connectortest.ContractCase{
			Name:       "fullsync",
			Cassette:   "testdata/fullsync.json",
			ConfigJSON: json.RawMessage(`{}`),
			Token:      []byte(testToken),
			FullSync: &connectortest.SyncExpectation{
				WantDocIDs: []string{docID("evt-standup"), docID("evt-launch"), docID("evt-1on1")},
				WantCursor: &afterBackfill,
			},
		})
	})

	t.Run("incremental change and delete", func(t *testing.T) {
		t.Parallel()
		afterInc := syncCursor("SYNCTOKEN-AFTER-INCREMENTAL")
		connectortest.RunConnectorContract(t, newTestConnector(), connectortest.ContractCase{
			Name:       "incremental",
			Cassette:   "testdata/incremental.json",
			ConfigJSON: json.RawMessage(`{}`),
			Token:      []byte(testToken),
			Incremental: &connectortest.IncrementalExpectation{
				FromCursor: syncCursor("SYNCTOKEN-AFTER-BACKFILL"),
				SyncExpectation: connectortest.SyncExpectation{
					WantDocIDs:          []string{docID("evt-launch")},
					WantTombstoneDocIDs: []string{docID("evt-standup")},
					WantCursor:          &afterInc,
				},
			},
		})
	})

	t.Run("stale sync token", func(t *testing.T) {
		t.Parallel()
		wantErr := sdk.ErrCursorExpired
		connectortest.RunConnectorContract(t, newTestConnector(), connectortest.ContractCase{
			Name:       "stale-cursor",
			Cassette:   "testdata/stale_cursor.json",
			ConfigJSON: json.RawMessage(`{}`),
			Token:      []byte(testToken),
			Incremental: &connectortest.IncrementalExpectation{
				FromCursor:      syncCursor("EXPIRED-SYNC-TOKEN"),
				SyncExpectation: connectortest.SyncExpectation{WantErr: &wantErr},
			},
		})
	})

	t.Run("unparseable cursor is expired", func(t *testing.T) {
		t.Parallel()
		wantErr := sdk.ErrCursorExpired
		connectortest.RunConnectorContract(t, newTestConnector(), connectortest.ContractCase{
			Name:       "garbage-cursor",
			Cassette:   "testdata/stale_cursor.json", // no HTTP call is made: cursor rejected first
			ConfigJSON: json.RawMessage(`{}`),
			Token:      []byte(testToken),
			Incremental: &connectortest.IncrementalExpectation{
				FromCursor:      sdk.Cursor("not-a-gcal-cursor"),
				SyncExpectation: connectortest.SyncExpectation{WantErr: &wantErr},
			},
		})
	})

	t.Run("webhook unsupported", func(t *testing.T) {
		t.Parallel()
		c := newTestConnector()
		var rec connectortest.EmitRecorder
		err := c.HandleWebhook(context.Background(), sdk.Config{
			ConfigJSON: []byte(`{}`),
			Checkpoint: sdk.NopCheckpoint,
		}, nil, rec.Emit)
		if !errors.Is(err, sdk.ErrWebhookUnsupported) {
			t.Errorf("HandleWebhook error = %v, want sdk.ErrWebhookUnsupported", err)
		}
		if n := len(rec.Docs()); n != 0 {
			t.Errorf("HandleWebhook emitted %d docs, want 0", n)
		}
	})
}
