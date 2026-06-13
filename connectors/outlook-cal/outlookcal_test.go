package outlookcal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	if spec.ID != "outlook-cal" {
		t.Errorf("spec.ID = %q, want %q", spec.ID, "outlook-cal")
	}
	if spec.DisplayName == "" {
		t.Error("spec.DisplayName is empty")
	}
	if spec.AuthType != sdk.AuthOAuth2 {
		t.Errorf("spec.AuthType = %v, want AuthOAuth2", spec.AuthType)
	}
	if spec.SupportsWebhook {
		t.Error("spec.SupportsWebhook = true, want false (Graph subscriptions deferred)")
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
	for _, prop := range []string{"base_url", "user_principal_name"} {
		if p, ok := schema.Properties[prop]; !ok || p.Type != "string" {
			t.Errorf("schema property %q = %+v, want string property", prop, p)
		}
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newTestConnector()

	t.Run("ok with token via /me", func(t *testing.T) {
		t.Parallel()
		ts := meServer(t, `{"userPrincipalName":"alice@example.com","mail":"alice@example.com"}`, http.StatusOK)
		cfg := sdk.Config{
			ConfigJSON: []byte(`{"base_url":"` + ts.URL + `","user_principal_name":"alice@example.com"}`),
			Token:      []byte("tok"),
			Checkpoint: sdk.NopCheckpoint,
		}
		if err := c.Validate(ctx, cfg); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("ok with token and no user_principal_name set", func(t *testing.T) {
		t.Parallel()
		ts := meServer(t, `{"userPrincipalName":"someone@example.com"}`, http.StatusOK)
		cfg := sdk.Config{
			ConfigJSON: []byte(`{"base_url":"` + ts.URL + `"}`),
			Token:      []byte("tok"),
			Checkpoint: sdk.NopCheckpoint,
		}
		if err := c.Validate(ctx, cfg); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("principal mismatch", func(t *testing.T) {
		t.Parallel()
		ts := meServer(t, `{"userPrincipalName":"someone-else@example.com"}`, http.StatusOK)
		cfg := sdk.Config{
			ConfigJSON: []byte(`{"base_url":"` + ts.URL + `","user_principal_name":"alice@example.com"}`),
			Token:      []byte("tok"),
			Checkpoint: sdk.NopCheckpoint,
		}
		err := c.Validate(ctx, cfg)
		if err == nil || !strings.Contains(err.Error(), "user_principal_name") {
			t.Fatalf("Validate = %v, want principal mismatch error", err)
		}
	})

	t.Run("bad token surfaces credential error", func(t *testing.T) {
		t.Parallel()
		ts := meServer(t, `{"error":{"code":"InvalidAuthenticationToken"}}`, http.StatusUnauthorized)
		cfg := sdk.Config{
			ConfigJSON: []byte(`{"base_url":"` + ts.URL + `","user_principal_name":"alice@example.com"}`),
			Token:      []byte("bad"),
			Checkpoint: sdk.NopCheckpoint,
		}
		err := c.Validate(ctx, cfg)
		if err == nil || !strings.Contains(err.Error(), "credential check failed") {
			t.Fatalf("Validate = %v, want credential check error", err)
		}
		if strings.Contains(err.Error(), "bad") {
			t.Error("Validate error leaked the token")
		}
	})

	t.Run("no token skips round-trip", func(t *testing.T) {
		t.Parallel()
		cfg := sdk.Config{ConfigJSON: []byte(`{"user_principal_name":"a@b.c"}`), Checkpoint: sdk.NopCheckpoint}
		if err := c.Validate(ctx, cfg); err != nil {
			t.Fatalf("Validate without token: %v", err)
		}
	})

	t.Run("empty config is config-only ok without token", func(t *testing.T) {
		t.Parallel()
		cfg := sdk.Config{Checkpoint: sdk.NopCheckpoint}
		if err := c.Validate(ctx, cfg); err != nil {
			t.Fatalf("Validate empty config: %v", err)
		}
	})

	t.Run("config errors", func(t *testing.T) {
		t.Parallel()
		for name, raw := range map[string]string{
			"not json":     "{",
			"bad base_url": `{"base_url":"::not-a-url"}`,
			"ftp base_url": `{"base_url":"ftp://host"}`,
		} {
			cfg := sdk.Config{ConfigJSON: []byte(raw), Checkpoint: sdk.NopCheckpoint}
			if err := c.Validate(ctx, cfg); err == nil {
				t.Errorf("Validate accepted %s config %q", name, raw)
			}
		}
	})
}

func TestHandleWebhookUnsupported(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	var rec connectortest.EmitRecorder
	err := c.HandleWebhook(context.Background(), sdk.Config{Checkpoint: sdk.NopCheckpoint}, nil, rec.Emit)
	if err != sdk.ErrWebhookUnsupported {
		t.Fatalf("HandleWebhook err = %v, want sdk.ErrWebhookUnsupported", err)
	}
	if len(rec.Docs()) != 0 {
		t.Errorf("HandleWebhook emitted %d docs, want 0", len(rec.Docs()))
	}
}

// meServer is a fake Graph /me endpoint serving body with status.
func meServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts
}
