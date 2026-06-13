package jira

import (
	"encoding/json"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
	"github.com/asker/asker/platform/tenancy"
)

// newReplayServer loads a cassette and starts a replay server bound to it.
func newReplayServer(t *testing.T, path string) *connectortest.ReplayServer {
	t.Helper()
	cas, err := connectortest.LoadCassette(path)
	if err != nil {
		t.Fatalf("LoadCassette(%q): %v", path, err)
	}
	return connectortest.NewReplayServer(t, cas)
}

// contractConfig builds an sdk.Config whose base_url points at baseURL, with a
// real tenancy.Context and a placeholder token.
func contractConfig(t *testing.T, baseURL, extraJSON string) sdk.Config {
	t.Helper()
	tcx, err := tenancy.FromClaims(map[string]any{"tenant_id": "tenant-jira", "sub": "user-jira"})
	if err != nil {
		t.Fatalf("tenancy.FromClaims: %v", err)
	}
	cfgJSON := mergeBaseURL(t, extraJSON, baseURL)
	return sdk.Config{
		Tenant:     tcx,
		InstanceID: "inst-jira",
		ConfigJSON: cfgJSON,
		Token:      []byte("decrypted-oauth-token"),
		Checkpoint: sdk.NopCheckpoint,
	}
}

// mergeBaseURL parses extraJSON as a JSON object and sets base_url to baseURL.
func mergeBaseURL(t *testing.T, extraJSON, baseURL string) []byte {
	t.Helper()
	obj := map[string]json.RawMessage{}
	if extraJSON != "" {
		if err := json.Unmarshal([]byte(extraJSON), &obj); err != nil {
			t.Fatalf("parse extraJSON %q: %v", extraJSON, err)
		}
	}
	urlRaw, err := json.Marshal(baseURL)
	if err != nil {
		t.Fatalf("marshal base_url: %v", err)
	}
	obj["base_url"] = urlRaw
	out, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return out
}
