// Package confluence implements the Asker Confluence (Atlassian Cloud)
// connector: it indexes wiki pages from a Confluence site via the Cloud REST
// API (/wiki/rest/api). Initial backfill pages through
// GET /rest/api/content?type=page (offset pagination); incremental sync uses
// GET /rest/api/content/search with a CQL filter on lastModified, plus a
// status=trashed window to surface deletions as tombstones.
//
// There is no vendored Atlassian Go SDK in the module, so the connector talks
// to the REST API directly over net/http (the norm for Graph/Slack/Atlassian
// connectors). The base URL comes from the instance config's "base_url" (e.g.
// https://<site>.atlassian.net/wiki); contract tests point it at a replay
// server. The bearer credential arrives in sdk.Config.Token already decrypted
// by the hub vault — the connector only ever sees a currently-valid token.
//
// # Authentication
//
// Atlassian Cloud accepts either Basic auth (email:api_token) or an OAuth 2.0
// Bearer token. This connector uses Bearer: it sets
// "Authorization: Bearer <Config.Token>" on every request. The hub may also be
// configured to pass a Basic credential; that is hub/integrator wiring and is
// out of this connector's scope (see README).
//
// # Cursor format
//
// The incremental cursor is the lastModified timestamp of the newest page seen
// so far, in Confluence's "yyyy-MM-dd HH:mm" form (the granularity CQL accepts
// for lastModified comparisons). IncrementalSync replays
// content/search?cql=lastModified>="<cursor>" order by lastModified asc, so a
// cursor never expires at the source (CQL by timestamp is always replayable) —
// this connector therefore never returns sdk.ErrCursorExpired, documented on
// IncrementalSync.
package confluence

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/asker/asker/connectors/sdk"
)

const (
	// connectorID is the stable connector identifier baked into every doc_id
	// via sdk.DocID. It must never change once documents exist.
	connectorID = "confluence"

	// defaultBaseURL is the real Confluence Cloud REST root assumed when the
	// instance config omits base_url. A real deployment always sets base_url to
	// its own site (https://<site>.atlassian.net/wiki); this default only keeps
	// the field optional.
	defaultBaseURL = "https://your-domain.atlassian.net/wiki"

	// apiPrefix is the REST path prefix appended to base_url for every call.
	apiPrefix = "/rest/api"

	// listPageSize is the offset-pagination page size for content listing and
	// CQL search. Confluence caps a single page at 250; 50 is a safe default.
	listPageSize = 50

	// httpTimeout bounds every source request so a hung Confluence call cannot
	// stall a sync indefinitely. The hub owns overall retry/backoff.
	httpTimeout = 30 * time.Second

	// expandParam requests the page sub-resources the Document mapping needs in
	// one round-trip: storage body, version, space, and history (creator).
	expandParam = "body.storage,version,space,history"
)

// configSchema is the JSONSchema for instance configuration. base_url is the
// Confluence site REST root (https://<site>.atlassian.net/wiki); space_key,
// when set, scopes the sync to a single space. The schema allows extra keys so
// the hub may merge runtime fields.
const configSchema = `{
  "type": "object",
  "properties": {
    "base_url": {"type": "string"},
    "space_key": {"type": "string"}
  }
}`

// Connector implements sdk.Connector for Confluence.
type Connector struct {
	log *slog.Logger
	now func() time.Time
}

// Option customizes a Connector.
type Option func(*Connector)

// WithLogger sets the structured logger (default slog.Default()).
func WithLogger(l *slog.Logger) Option {
	return func(c *Connector) {
		if l != nil {
			c.log = l
		}
	}
}

// withClock overrides the time source (used by tests for deterministic
// tombstone deleted_at and empty-space cursors).
func withClock(now func() time.Time) Option {
	return func(c *Connector) {
		if now != nil {
			c.now = now
		}
	}
}

// New returns a ready-to-register Confluence connector. It is the zero-config
// constructor the hub builds instances from; per-instance settings arrive via
// sdk.Config.
func New(opts ...Option) sdk.Connector {
	c := &Connector{log: slog.Default(), now: time.Now}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Spec implements sdk.Connector.
func (c *Connector) Spec() sdk.Spec {
	return sdk.Spec{
		ID:          connectorID,
		DisplayName: "Confluence",
		AuthType:    sdk.AuthOAuth2,
		// Atlassian webhooks require an installed app/Connect descriptor, so
		// push is deferred; the hub schedules polling. See README "Cursor model".
		SupportsWebhook: false,
		ConfigSchema:    json.RawMessage(configSchema),
	}
}

// instanceConfig is the parsed ConfigJSON.
type instanceConfig struct {
	BaseURL  string `json:"base_url"`
	SpaceKey string `json:"space_key"`
}

// resolvedBaseURL returns the API base URL (config override or the real
// default), trimmed of a trailing slash.
func (ic instanceConfig) resolvedBaseURL() string {
	base := strings.TrimSpace(ic.BaseURL)
	if base == "" {
		base = defaultBaseURL
	}
	return strings.TrimRight(base, "/")
}

// parseConfig decodes and sanity-checks ConfigJSON: it must be a JSON object,
// and any base_url present must be a well-formed http(s) URL.
func parseConfig(raw []byte) (instanceConfig, error) {
	var conf instanceConfig
	if len(raw) == 0 {
		// An empty config is valid: base_url defaults and space_key is optional.
		return conf, nil
	}
	if err := json.Unmarshal(raw, &conf); err != nil {
		return conf, fmt.Errorf("confluence: config is not valid JSON: %w", err)
	}
	conf.BaseURL = strings.TrimSpace(conf.BaseURL)
	conf.SpaceKey = strings.TrimSpace(conf.SpaceKey)
	if conf.BaseURL != "" {
		u, err := url.Parse(conf.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return conf, fmt.Errorf("confluence: config field base_url %q is not a valid http(s) URL", conf.BaseURL)
		}
	}
	return conf, nil
}

// Validate implements sdk.Connector: the config must parse, and when a token is
// present it must survive one cheap authenticated round-trip (listing a single
// page). Error messages are surfaced to users verbatim and never include the
// token.
func (c *Connector) Validate(ctx context.Context, cfg sdk.Config) error {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return err
	}
	if len(cfg.Token) == 0 {
		// No credential yet (e.g. instance created before the OAuth flow
		// completes): config-only validation.
		return nil
	}
	client := newClient(conf, cfg.Token, c.log)
	// One cheap authed call: list a single page. A non-2xx is a credential or
	// reachability failure surfaced credential-free.
	q := url.Values{}
	q.Set("type", "page")
	q.Set("limit", "1")
	if conf.SpaceKey != "" {
		q.Set("spaceKey", conf.SpaceKey)
	}
	var probe contentPage
	if err := client.getJSON(ctx, "/content", q, &probe); err != nil {
		return fmt.Errorf("confluence: credential check failed: %w", err)
	}
	return nil
}

// apiClient is a thin REST client over net/http that injects the bearer token
// and resolves paths against the site's /rest/api root.
type apiClient struct {
	http    *http.Client
	baseURL string // "<base_url>/rest/api"
	log     *slog.Logger
}

// newClient builds an apiClient whose transport adds the bearer credential
// from token to every request. The hub owns refresh; the connector only sees a
// valid token. The logger receives server-side debug detail about non-2xx
// responses (status + body snippet); that detail never reaches the user-facing
// error string.
func newClient(conf instanceConfig, token []byte, log *slog.Logger) *apiClient {
	if log == nil {
		log = slog.Default()
	}
	return &apiClient{
		http: &http.Client{
			Transport: &bearerTransport{token: string(token), base: http.DefaultTransport},
			Timeout:   httpTimeout,
		},
		baseURL: conf.resolvedBaseURL() + apiPrefix,
		log:     log,
	}
}

// bearerTransport adds "Authorization: Bearer <token>" to every request
// without mutating the caller's request. The token is never logged.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	clone.Header.Set("Accept", "application/json")
	return t.base.RoundTrip(clone)
}

// apiError is a non-2xx response from Confluence. The user-facing Error string
// is deliberately generic: it never echoes the upstream Confluence response
// body, which can carry account/configuration detail and is a confusing,
// info-leaking surface to hand back to a tenant. The upstream status and body
// snippet are logged server-side at debug instead (see getJSON).
type apiError struct{}

// Error returns a generic, credential-free credential/reachability message.
// The upstream response status and body are intentionally omitted from the
// user-facing string; getJSON logs them server-side at debug.
func (apiError) Error() string {
	return "confluence: source request failed: the credential may be invalid or the Confluence site unreachable"
}

// maxErrorSnippet bounds how much of an error response body is captured for the
// server-side debug log. It is never placed in a user-facing error message.
const maxErrorSnippet = 512

// getJSON issues GET <baseURL><path>?<query>, honors ctx, and decodes a 2xx
// JSON body into out. A non-2xx becomes a generic apiError (the upstream status
// and body are logged at debug, never returned to the user).
func (c *apiClient) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	full := c.baseURL + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return fmt.Errorf("confluence: build request %s: %w", path, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("confluence: GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read the upstream body only for the server-side debug log; it is never
		// surfaced to the tenant (see apiError.Error). The credential is in the
		// request Authorization header, not in this response body, so logging the
		// body does not log the credential.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorSnippet))
		c.log.Debug("confluence source request returned non-2xx",
			"method", http.MethodGet,
			"path", path,
			"status", resp.StatusCode,
			"body_snippet", strings.TrimSpace(string(snippet)))
		return apiError{}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("confluence: decode %s response: %w", path, err)
	}
	return nil
}

// HandleWebhook implements sdk.Connector. Confluence push requires an installed
// Atlassian app (Connect/Forge descriptor) to register webhooks, which is hub
// configuration deferred past M2, so this connector has no push path and the
// hub uses polling.
func (c *Connector) HandleWebhook(_ context.Context, _ sdk.Config, _ *http.Request, _ sdk.Emit) error {
	return sdk.ErrWebhookUnsupported
}
