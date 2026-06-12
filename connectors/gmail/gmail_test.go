package gmail

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
)

func TestSpec(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	connectortest.RunSpecChecks(t, c)

	spec := c.Spec()
	if spec.ID != "gmail" {
		t.Errorf("spec.ID = %q, want %q", spec.ID, "gmail")
	}
	if spec.AuthType != sdk.AuthOAuth2 {
		t.Errorf("spec.AuthType = %v, want AuthOAuth2", spec.AuthType)
	}
	if !spec.SupportsWebhook {
		t.Error("spec.SupportsWebhook = false, want true")
	}

	var schema struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(spec.ConfigSchema, &schema); err != nil {
		t.Fatalf("ConfigSchema does not parse: %v", err)
	}
	if schema.Type != "object" {
		t.Errorf("schema type = %q, want object", schema.Type)
	}
	for _, prop := range []string{"base_url", "user_email"} {
		if p, ok := schema.Properties[prop]; !ok || p.Type != "string" {
			t.Errorf("schema property %q = %+v, want string property", prop, p)
		}
	}
	if len(schema.Required) != 1 || schema.Required[0] != "user_email" {
		t.Errorf("schema required = %v, want [user_email]", schema.Required)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newTestConnector()

	t.Run("ok with token against fake (getProfile fallback)", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		if err := c.Validate(ctx, f.connectorConfig("", nil)); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("ok with token via getProfile", func(t *testing.T) {
		t.Parallel()
		f := newFixtureWithProfile(t)
		if err := c.Validate(ctx, f.connectorConfig("", nil)); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("profile email mismatch", func(t *testing.T) {
		t.Parallel()
		f := newFixtureWithProfile(t)
		cfg := f.connectorConfig("", nil)
		cfg.ConfigJSON = []byte(`{"base_url":"` + f.ts.URL + `","user_email":"someone-else@example.com"}`)
		err := c.Validate(ctx, cfg)
		if err == nil || !strings.Contains(err.Error(), "user_email") {
			t.Fatalf("Validate = %v, want user_email mismatch error", err)
		}
	})

	t.Run("bad token", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		cfg := f.connectorConfig("", nil)
		cfg.Token = []byte("not-a-shim-token")
		if err := c.Validate(ctx, cfg); err == nil {
			t.Fatal("Validate accepted an invalid token")
		}
	})

	t.Run("no token skips round-trip", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		cfg := f.connectorConfig("", nil)
		cfg.Token = nil
		if err := c.Validate(ctx, cfg); err != nil {
			t.Fatalf("Validate without token: %v", err)
		}
	})

	t.Run("config errors", func(t *testing.T) {
		t.Parallel()
		for name, raw := range map[string]string{
			"empty":              "",
			"not json":           "{",
			"missing user_email": `{"base_url":"http://x"}`,
			"blank user_email":   `{"user_email":"  "}`,
			"bad base_url":       `{"user_email":"a@b.c","base_url":"::not-a-url"}`,
			"ftp base_url":       `{"user_email":"a@b.c","base_url":"ftp://host"}`,
		} {
			cfg := sdk.Config{ConfigJSON: []byte(raw), Checkpoint: sdk.NopCheckpoint}
			if err := c.Validate(ctx, cfg); err == nil {
				t.Errorf("Validate accepted %s config %q", name, raw)
			}
		}
	})
}
