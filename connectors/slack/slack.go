// Package slack implements the Asker Slack connector: backfill of channel
// messages via the Web API (conversations.list + conversations.history),
// incremental sync via per-channel conversations.history with an `oldest`
// watermark, and push via the Slack Events API callback (HandleWebhook).
//
// The connector talks to the Slack Web API over net/http (there is no
// vendored Slack SDK). In production the base URL is https://slack.com/api;
// in dev/CI the instance config's base_url points the client at a
// connectortest.ReplayServer backed by a committed cassette. The bearer
// credential (a bot/user OAuth token, "xoxb-..."/"xoxp-...") arrives in
// sdk.Config.Token already decrypted by the hub vault and is attached as
// "Authorization: Bearer <token>" on every call.
//
// # Cursor format
//
// Slack has no global delta feed: each conversation is paginated and read
// independently, so the cursor encodes a per-channel high-water mark. It is a
// JSON object mapping channel id -> the latest message ts the connector has
// emitted for that channel:
//
//	{"C123":"1700000200.000100","C456":"1700000050.000000"}
//
// IncrementalSync calls conversations.history for each channel with
// oldest=<that ts> (exclusive) to fetch only newer messages, then advances
// the map to the newest ts it saw. FullSync returns the same JSON map built
// from the latest ts per channel. A cursor that is not the connector's own
// JSON object shape surfaces as sdk.ErrCursorExpired so the hub restarts a
// full sync rather than looping on a poison cursor; Slack's own
// "invalid_cursor" pagination error maps to ErrCursorExpired as well.
package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/platform/safehttp"
)

const (
	// connectorID is the stable connector identifier baked into every
	// doc_id via sdk.DocID. It must never change.
	connectorID = "slack"

	// defaultBaseURL is the real Slack Web API base used when the instance
	// config omits base_url.
	defaultBaseURL = "https://slack.com/api"

	// historyLimit is the conversations.history / conversations.list page
	// size. Slack caps history at 1000; 200 keeps responses small and is the
	// documented sweet spot.
	historyLimit = 200

	// clientTimeout bounds a single Web API round-trip.
	clientTimeout = 30 * time.Second

	// maxWebhookBody bounds the accepted Events API payload. Real events are
	// a few KB; the cap defends against a hostile sender.
	maxWebhookBody = 1 << 20
)

// configSchema is the JSONSchema for instance configuration. base_url is the
// API endpoint override (dev/CI: replay server; empty: real Slack);
// signing_secret is the Slack app signing secret used to verify Events API
// callbacks. The hub may merge additional keys at runtime, so the schema
// allows extra properties.
const configSchema = `{
  "type": "object",
  "properties": {
    "base_url": {"type": "string"},
    "signing_secret": {"type": "string"}
  }
}`

// Connector implements sdk.Connector for Slack.
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

// New returns a ready-to-register Slack connector. It takes no required
// configuration: the hub builds instances from sdk.Config.
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
		ID:              connectorID,
		DisplayName:     "Slack",
		AuthType:        sdk.AuthOAuth2,
		ConfigSchema:    json.RawMessage(configSchema),
		SupportsWebhook: true,
	}
}

// instanceConfig is the parsed ConfigJSON.
type instanceConfig struct {
	BaseURL       string `json:"base_url"`
	SigningSecret string `json:"signing_secret"`
}

// baseURL returns the configured API base, defaulting to real Slack.
func (conf instanceConfig) baseURL() string {
	if conf.BaseURL != "" {
		return conf.BaseURL
	}
	return defaultBaseURL
}

// parseConfig decodes and sanity-checks ConfigJSON: valid JSON object and a
// well-formed http(s) base_url when one is supplied.
func parseConfig(raw []byte) (instanceConfig, error) {
	var conf instanceConfig
	if len(raw) == 0 {
		return conf, nil
	}
	if err := json.Unmarshal(raw, &conf); err != nil {
		return conf, fmt.Errorf("slack: config is not valid JSON: %w", err)
	}
	if conf.BaseURL != "" {
		u, err := url.Parse(conf.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return conf, fmt.Errorf("slack: config field base_url %q is not a valid http(s) URL", conf.BaseURL)
		}
	}
	return conf, nil
}

// bearerTransport adds "Authorization: Bearer <token>" to every request. The
// hub owns refresh; the connector only ever sees a currently-valid token.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

// RoundTrip implements http.RoundTripper without mutating the caller's request.
func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

// client is a thin Slack Web API client: a base URL plus an *http.Client whose
// transport injects the bearer token.
type client struct {
	base string
	http *http.Client
}

// newClient builds a Web API client for conf bound to token. The transport is
// the default unless base is the real API; tests inject a replay server via
// base_url so http.DefaultTransport is fine.
func newClient(conf instanceConfig, token []byte) *client {
	return &client{
		base: strings.TrimRight(conf.baseURL(), "/"),
		http: &http.Client{
			// base_url is tenant-supplied; the SSRF-guarded base refuses
			// loopback/metadata/private/cluster IPs at connect time so the bearer
			// token is never sent to an internal host.
			Transport: &bearerTransport{token: string(token), base: safehttp.GuardedBase()},
			Timeout:   clientTimeout,
		},
	}
}

// slackError mirrors the Slack Web API error envelope. Every Web API response
// carries `ok`; on failure `error` names the reason ("invalid_auth",
// "invalid_cursor", "channel_not_found", ...).
type slackError struct {
	reason string
}

func (e *slackError) Error() string { return "slack: API error " + e.reason }

// isAPIError reports whether err is a Slack API error whose reason is one of
// reasons.
func isAPIError(err error, reasons ...string) bool {
	var se *slackError
	if !errors.As(err, &se) {
		return false
	}
	for _, r := range reasons {
		if se.reason == r {
			return true
		}
	}
	return false
}

// apiResponse is the common envelope every Web API method returns.
type apiResponse struct {
	OK               bool   `json:"ok"`
	Error            string `json:"error"`
	ResponseMetadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
}

// get issues an authenticated GET to method (e.g. "conversations.list") with
// the supplied query params, decoding the JSON body into out. A non-2xx HTTP
// status or an `"ok":false` body is returned as an error; an `"ok":false`
// body yields a *slackError carrying the reason so callers can branch on it.
func (cl *client) get(ctx context.Context, method string, params url.Values, out any) error {
	rawURL := cl.base + "/" + method
	if len(params) > 0 {
		rawURL += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("slack: build request for %s: %w", method, err)
	}
	resp, err := cl.http.Do(req)
	if err != nil {
		return fmt.Errorf("slack: %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxWebhookBody))
	if err != nil {
		return fmt.Errorf("slack: read %s response: %w", method, err)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("slack: %s: HTTP status %d", method, resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("slack: decode %s response: %w", method, err)
	}
	// Re-decode the common envelope to inspect ok/error without constraining
	// the caller's struct shape.
	var env apiResponse
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("slack: decode %s envelope: %w", method, err)
	}
	if !env.OK {
		reason := env.Error
		if reason == "" {
			reason = "unknown"
		}
		return fmt.Errorf("slack: %s failed: %w", method, &slackError{reason: reason})
	}
	return nil
}

// Validate implements sdk.Connector: the config must parse, and when a token
// is present it must survive a cheap authenticated call (auth.test). Error
// messages are shown to users verbatim and never include the token.
func (c *Connector) Validate(ctx context.Context, cfg sdk.Config) error {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return err
	}
	if len(cfg.Token) == 0 {
		// No credential yet (instance created before the OAuth flow
		// completes): config-only validation.
		return nil
	}
	cl := newClient(conf, cfg.Token)
	var resp struct {
		apiResponse
		Team   string `json:"team"`
		UserID string `json:"user_id"`
	}
	if err := cl.get(ctx, "auth.test", nil, &resp); err != nil {
		return fmt.Errorf("slack: credential check failed: %w", err)
	}
	return nil
}
