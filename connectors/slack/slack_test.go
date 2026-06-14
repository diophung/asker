package slack

import (
	"context"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
)

func TestSpecShape(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	spec := c.Spec()
	if spec.ID != "slack" {
		t.Errorf("spec.ID = %q, want slack", spec.ID)
	}
	if spec.AuthType != sdk.AuthOAuth2 {
		t.Errorf("spec.AuthType = %v, want AuthOAuth2", spec.AuthType)
	}
	if !spec.SupportsWebhook {
		t.Error("spec.SupportsWebhook = false, want true")
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("ok with good token (auth.test)", func(t *testing.T) {
		t.Parallel()
		cas, err := connectortest.LoadCassette("testdata/validate.json")
		if err != nil {
			t.Fatal(err)
		}
		rs := connectortest.NewReplayServer(t, cas)
		c := newTestConnector()
		if err := c.Validate(ctx, testConfig(t, rs.URL(), "")); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("bad token (auth.test ok:false)", func(t *testing.T) {
		t.Parallel()
		// The second interaction in validate.json returns invalid_auth; serve
		// only that one by playing past the first with a fresh cassette slice.
		cas, err := connectortest.LoadCassette("testdata/validate.json")
		if err != nil {
			t.Fatal(err)
		}
		cas.Interactions = cas.Interactions[1:]
		rs := connectortest.NewReplayServer(t, cas)
		c := newTestConnector()
		if err := c.Validate(ctx, testConfig(t, rs.URL(), "")); err == nil {
			t.Fatal("Validate accepted an invalid_auth response")
		}
	})

	t.Run("no token skips round-trip", func(t *testing.T) {
		t.Parallel()
		c := newTestConnector()
		cfg := sdk.Config{ConfigJSON: []byte(`{}`), Checkpoint: sdk.NopCheckpoint}
		if err := c.Validate(ctx, cfg); err != nil {
			t.Fatalf("Validate without token: %v", err)
		}
	})

	t.Run("config errors", func(t *testing.T) {
		t.Parallel()
		c := newTestConnector()
		for name, raw := range map[string]string{
			"not json":     "{",
			"bad base_url": `{"base_url":"::nope"}`,
			"ftp base_url": `{"base_url":"ftp://host"}`,
		} {
			cfg := sdk.Config{ConfigJSON: []byte(raw), Checkpoint: sdk.NopCheckpoint, Token: []byte(testToken)}
			if err := c.Validate(ctx, cfg); err == nil {
				t.Errorf("Validate accepted %s config %q", name, raw)
			}
		}
	})
}

func TestParseConfigBaseURLDefault(t *testing.T) {
	t.Parallel()
	conf, err := parseConfig(nil)
	if err != nil {
		t.Fatalf("parseConfig(nil): %v", err)
	}
	if conf.baseURL() != defaultBaseURL {
		t.Errorf("default baseURL = %q, want %q", conf.baseURL(), defaultBaseURL)
	}
}
