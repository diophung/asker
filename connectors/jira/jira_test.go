package jira

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
	c := New()
	connectortest.RunSpecChecks(t, c)

	spec := c.Spec()
	if spec.ID != "jira" {
		t.Errorf("spec.ID = %q, want jira", spec.ID)
	}
	if spec.AuthType != sdk.AuthOAuth2 {
		t.Errorf("spec.AuthType = %v, want AuthOAuth2", spec.AuthType)
	}
	if spec.SupportsWebhook {
		t.Error("spec.SupportsWebhook = true, want false (Jira webhooks deferred)")
	}
	if !json.Valid(spec.ConfigSchema) {
		t.Error("ConfigSchema is not valid JSON")
	}
}

func TestWebhookUnsupported(t *testing.T) {
	t.Parallel()
	err := New().HandleWebhook(context.Background(), sdk.Config{}, nil, nil)
	if err != sdk.ErrWebhookUnsupported {
		t.Errorf("HandleWebhook err = %v, want sdk.ErrWebhookUnsupported", err)
	}
}

func TestParseConfig(t *testing.T) {
	t.Parallel()
	t.Run("empty is valid", func(t *testing.T) {
		t.Parallel()
		conf, err := parseConfig(nil)
		if err != nil {
			t.Fatalf("parseConfig(nil): %v", err)
		}
		if conf.baseURL() != defaultBaseURL {
			t.Errorf("baseURL() = %q, want default", conf.baseURL())
		}
	})

	t.Run("custom base_url trims slash", func(t *testing.T) {
		t.Parallel()
		conf, err := parseConfig([]byte(`{"base_url":"https://acme.atlassian.net/"}`))
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		if conf.baseURL() != "https://acme.atlassian.net" {
			t.Errorf("baseURL() = %q, want trimmed", conf.baseURL())
		}
	})

	t.Run("rejects invalid", func(t *testing.T) {
		t.Parallel()
		for name, raw := range map[string]string{
			"not json":     "{",
			"bad base_url": `{"base_url":"::nope"}`,
			"ftp base_url": `{"base_url":"ftp://host"}`,
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
	c := New()

	t.Run("ok with token", func(t *testing.T) {
		t.Parallel()
		rs := newReplayServer(t, "testdata/validate.json")
		cfg := contractConfig(t, rs.URL(), `{}`)
		if err := c.Validate(ctx, cfg); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("bad token surfaces a credential-free error", func(t *testing.T) {
		t.Parallel()
		rs := newReplayServer(t, "testdata/validate_unauthorized.json")
		cfg := contractConfig(t, rs.URL(), `{}`)
		err := c.Validate(ctx, cfg)
		if err == nil {
			t.Fatal("Validate accepted an unauthorized token")
		}
		if strings.Contains(err.Error(), "decrypted-oauth-token") {
			t.Errorf("Validate error leaked the token: %v", err)
		}
		// The error must not echo the upstream Jira response body. The
		// validate_unauthorized cassette returns this errorMessages string;
		// surfacing it to the user is an information leak.
		if strings.Contains(err.Error(), "Client must be authenticated") {
			t.Errorf("Validate error leaked the upstream Jira response body: %v", err)
		}
	})

	t.Run("no token skips round-trip", func(t *testing.T) {
		t.Parallel()
		cfg := sdk.Config{ConfigJSON: []byte(`{}`), Checkpoint: sdk.NopCheckpoint}
		if err := c.Validate(ctx, cfg); err != nil {
			t.Fatalf("Validate without token: %v", err)
		}
	})

	t.Run("bad config", func(t *testing.T) {
		t.Parallel()
		cfg := sdk.Config{ConfigJSON: []byte(`{`), Token: []byte("x"), Checkpoint: sdk.NopCheckpoint}
		if err := c.Validate(ctx, cfg); err == nil {
			t.Fatal("Validate accepted malformed config")
		}
	})
}

func TestWithProjectFilter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		jql  string
		keys []string
		want string
	}{
		{
			name: "no keys unchanged",
			jql:  "order by updated asc",
			keys: nil,
			want: "order by updated asc",
		},
		{
			name: "filter before order by, no other clause",
			jql:  "order by updated asc",
			keys: []string{"DEMO", "OPS"},
			want: "project in ('DEMO', 'OPS') order by updated asc",
		},
		{
			name: "AND into an existing where clause",
			jql:  "updated >= '2026/06/10 10:30' order by updated asc",
			keys: []string{"DEMO"},
			want: "updated >= '2026/06/10 10:30' AND project in ('DEMO') order by updated asc",
		},
		{
			name: "blank keys dropped",
			jql:  "order by updated asc",
			keys: []string{"  ", "DEMO"},
			want: "project in ('DEMO') order by updated asc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := withProjectFilter(tt.jql, tt.keys); got != tt.want {
				t.Errorf("withProjectFilter() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDeletedStatusDetection(t *testing.T) {
	t.Parallel()
	set := deletedStatusSet([]string{"Removed", "  Deleted  "})
	if !isDeletedStatus(set, &issue{Fields: issueFields{Status: &namedRef{Name: "removed"}}}) {
		t.Error("expected case-insensitive match for 'removed'")
	}
	if !isDeletedStatus(set, &issue{Fields: issueFields{Status: &namedRef{Name: "Deleted"}}}) {
		t.Error("expected match for 'Deleted'")
	}
	if isDeletedStatus(set, &issue{Fields: issueFields{Status: &namedRef{Name: "Done"}}}) {
		t.Error("did not expect 'Done' to be a deleted status")
	}
	if isDeletedStatus(nil, &issue{Fields: issueFields{Status: &namedRef{Name: "Removed"}}}) {
		t.Error("no configured deleted statuses should never match")
	}
}
