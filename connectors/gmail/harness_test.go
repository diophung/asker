package gmail

// Contract-test harness: runs the connector against the in-repo fake Gmail
// (tools/fake-gmail/server) in-process, satisfying the no-live-API rule
// (ADR-008). The fake is driven through its admin HTTP surface exactly like
// the e2e stack drives the compose service.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/platform/tenancy"
	"github.com/asker/asker/tools/fake-gmail/server"
)

const (
	testEmail  = "alice@example.com"
	testToken  = "fake-gmail-token:" + testEmail
	testTenant = "tenant-a"
)

// adminMessage mirrors the fake's wire message shape (historyId and
// internalDate are JSON strings, like the real API).
type adminMessage struct {
	ID           string `json:"id"`
	ThreadID     string `json:"threadId"`
	HistoryID    uint64 `json:"historyId,string"`
	InternalDate int64  `json:"internalDate,string"`
}

// seedResult mirrors the fake's admin seed response.
type seedResult struct {
	Seeded    int    `json:"seeded"`
	HistoryID uint64 `json:"historyId"`
}

// withoutProfile wraps the fake and 404s users.getProfile in Google's error
// shape. The in-repo fake now implements users.getProfile natively, so this
// shim recreates a source WITHOUT the endpoint to keep the connector's
// history.list fallback path under test.
func withoutProfile(next http.Handler) http.Handler {
	const notFound = `{"error":{"code":404,"message":"users.getProfile is disabled in this fixture",` +
		`"errors":[{"message":"users.getProfile is disabled in this fixture","reason":"notFound"}],"status":"NOT_FOUND"}}`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet &&
			strings.HasPrefix(r.URL.Path, "/gmail/v1/users/") &&
			strings.HasSuffix(r.URL.Path, "/profile") {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, notFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// fixture is one running fake-Gmail instance plus admin helpers.
type fixture struct {
	t  *testing.T
	ts *httptest.Server
}

// newFixture serves the raw fake, which implements users.getProfile (the
// connector's canonical current-history-id path).
func newFixture(t *testing.T) *fixture { return newFixtureHandler(t, false) }

// newFixtureWithoutProfile serves the fake behind a shim that 404s
// users.getProfile, for tests of the connector's fallback path.
func newFixtureWithoutProfile(t *testing.T) *fixture { return newFixtureHandler(t, true) }

func newFixtureHandler(t *testing.T, disableProfile bool) *fixture {
	t.Helper()
	fake := server.New(server.WithLogger(slog.New(slog.DiscardHandler)))
	var handler http.Handler = fake
	if disableProfile {
		handler = withoutProfile(fake)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(func() {
		ts.Close()
		fake.Close()
	})
	return &fixture{t: t, ts: ts}
}

// do issues one HTTP request against the fixture and decodes the JSON
// response into out (when non-nil), returning the status code.
func (f *fixture) do(method, path, token string, body, out any) int {
	f.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			f.t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, f.ts.URL+path, reader)
	if err != nil {
		f.t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatalf("read response: %v", err)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			f.t.Fatalf("decode %s %s response %q: %v", method, path, raw, err)
		}
	}
	return resp.StatusCode
}

func (f *fixture) seed(count int, seed int64) seedResult {
	f.t.Helper()
	var res seedResult
	if code := f.do(http.MethodPost, "/admin/users/"+testEmail+"/seed", "",
		map[string]any{"count": count, "seed": seed}, &res); code != http.StatusOK {
		f.t.Fatalf("seed: status %d", code)
	}
	return res
}

func (f *fixture) addMessage(from, to, subject, body string) adminMessage {
	f.t.Helper()
	var msg adminMessage
	if code := f.do(http.MethodPost, "/admin/users/"+testEmail+"/messages", "",
		map[string]any{"from": from, "to": to, "subject": subject, "body": body}, &msg); code != http.StatusCreated {
		f.t.Fatalf("addMessage: status %d", code)
	}
	return msg
}

func (f *fixture) editMessage(id, subject, body string) adminMessage {
	f.t.Helper()
	var msg adminMessage
	if code := f.do(http.MethodPut, "/admin/users/"+testEmail+"/messages/"+id, "",
		map[string]any{"subject": subject, "body": body}, &msg); code != http.StatusOK {
		f.t.Fatalf("editMessage: status %d", code)
	}
	return msg
}

func (f *fixture) deleteMessage(id string) {
	f.t.Helper()
	if code := f.do(http.MethodDelete, "/admin/users/"+testEmail+"/messages/"+id, "", nil, nil); code != http.StatusNoContent {
		f.t.Fatalf("deleteMessage: status %d", code)
	}
}

// listAllIDs returns every message id in the mailbox via the (authenticated)
// Gmail list API — ground truth for no-duplicate/no-missing assertions.
func (f *fixture) listAllIDs() []string {
	f.t.Helper()
	var ids []string
	pageToken := ""
	for {
		path := "/gmail/v1/users/me/messages?maxResults=500"
		if pageToken != "" {
			path += "&pageToken=" + pageToken
		}
		var resp struct {
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
			NextPageToken string `json:"nextPageToken"`
		}
		if code := f.do(http.MethodGet, path, testToken, nil, &resp); code != http.StatusOK {
			f.t.Fatalf("list messages: status %d", code)
		}
		for _, m := range resp.Messages {
			ids = append(ids, m.ID)
		}
		if resp.NextPageToken == "" {
			return ids
		}
		pageToken = resp.NextPageToken
	}
}

// connectorConfig builds an sdk.Config pointing the connector at the
// fixture. webhookURL "" omits the webhook_url key (poll-only instance).
func (f *fixture) connectorConfig(webhookURL string, checkpoint sdk.Checkpoint) sdk.Config {
	f.t.Helper()
	tc, err := tenancy.FromClaims(map[string]any{"tenant_id": testTenant, "sub": "user-a"})
	if err != nil {
		f.t.Fatalf("tenancy.FromClaims: %v", err)
	}
	conf := map[string]string{
		"base_url":   f.ts.URL,
		"user_email": testEmail,
	}
	if webhookURL != "" {
		conf["webhook_url"] = webhookURL
	}
	raw, err := json.Marshal(conf)
	if err != nil {
		f.t.Fatalf("marshal config: %v", err)
	}
	if checkpoint == nil {
		checkpoint = sdk.NopCheckpoint
	}
	return sdk.Config{
		Tenant:     tc,
		InstanceID: "inst-1",
		ConfigJSON: raw,
		Token:      []byte(testToken),
		Checkpoint: checkpoint,
	}
}

// newTestConnector returns a connector with a quiet logger.
func newTestConnector() *Connector {
	return New(WithLogger(slog.New(slog.DiscardHandler)))
}

// checkpointRecorder records every checkpointed cursor; fail, when set, is
// invoked after recording and its error is returned (used to abort a
// backfill at a page boundary).
type checkpointRecorder struct {
	mu      sync.Mutex
	cursors []sdk.Cursor
	fail    func(cur sdk.Cursor) error
}

func (r *checkpointRecorder) Checkpoint(_ context.Context, cur sdk.Cursor) error {
	r.mu.Lock()
	r.cursors = append(r.cursors, cur)
	fail := r.fail
	r.mu.Unlock()
	if fail != nil {
		return fail(cur)
	}
	return nil
}

func (r *checkpointRecorder) all() []sdk.Cursor {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]sdk.Cursor, len(r.cursors))
	copy(out, r.cursors)
	return out
}
