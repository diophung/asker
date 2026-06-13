// Package msteams implements the Asker Microsoft Teams connector against the
// Microsoft Graph v1.0 REST API. It backfills chat messages via
// GET /me/chats?$expand=members followed by GET /chats/{id}/messages, and syncs
// incrementally via the per-chat /messages/delta endpoint (a deltaLink-encoded
// cursor). Like the other Graph-based connectors in Asker (Outlook, Drive), it
// does not implement Graph change notifications yet, so SupportsWebhook is
// false and the hub polls.
//
// This connector was built from docs/connectors/building-a-connector.md using
// only the public SDK (connectors/sdk) and the cassette harness
// (connectors/sdk/connectortest), as a third-party developer would.
//
// # Auth
//
// Microsoft Graph uses OAuth2 (sdk.AuthOAuth2). The hub runs the
// authorization-code flow and delivers a live, scoped access token in
// sdk.Config.Token; the connector adds it as "Authorization: Bearer <token>"
// on every request and never logs or persists it.
//
// # Cursor format
//
// IncrementalSync needs a delta cursor per chat, but the SDK cursor is a single
// opaque string. The cursor is therefore a JSON object mapping chat id ->
// Graph deltaLink:
//
//	{"deltas":{"<chatId>":"<deltaLink>", ...}}
//
// FullSync captures, for every chat, the deltaLink obtained by draining that
// chat's /messages/delta endpoint AFTER the backfill, and returns this map so
// the first IncrementalSync continues exactly where the backfill stopped. A
// chat whose delta endpoint returns 410 Gone reports sdk.ErrCursorExpired, and
// the hub restarts FullSync (the connector never silently full-syncs itself).
package msteams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/asker/asker/connectors/sdk"
)

// connectorID is the stable connector identifier baked into every doc_id via
// sdk.DocID. It must never change once documents exist.
const connectorID = "msteams"

// defaultGraphBaseURL is the Microsoft Graph v1.0 endpoint used when the
// instance config supplies no base_url override (dev/CI point base_url at a
// replay server or fake).
const defaultGraphBaseURL = "https://graph.microsoft.com/v1.0"

// httpTimeout bounds a single Graph round-trip.
const httpTimeout = 30 * time.Second

// configSchema is the JSONSchema for instance configuration. base_url is the
// API endpoint override (dev/CI); the connector needs no other user config
// because the bearer token identifies the signed-in user (/me).
const configSchema = `{
  "type": "object",
  "properties": {
    "base_url": {"type": "string"}
  }
}`

// Connector implements sdk.Connector for Microsoft Teams.
type Connector struct {
	log *slog.Logger
	now func() time.Time
}

// Option customizes a Connector.
type Option func(*Connector)

// WithLogger sets the structured logger (default slog.Default()). It matches
// the wave-1 connectors' shape so the hub registry can wire every connector
// uniformly.
func WithLogger(l *slog.Logger) Option {
	return func(c *Connector) {
		if l != nil {
			c.log = l
		}
	}
}

// New returns a ready-to-register MS Teams connector.
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
		DisplayName:     "Microsoft Teams",
		AuthType:        sdk.AuthOAuth2,
		ConfigSchema:    json.RawMessage(configSchema),
		SupportsWebhook: false, // Graph change notifications deferred (like the other Graph connectors).
	}
}

// instanceConfig is the parsed ConfigJSON.
type instanceConfig struct {
	BaseURL string `json:"base_url"`
}

// parseConfig decodes and sanity-checks ConfigJSON: it must be a JSON object,
// and a base_url override, when present, must be a well-formed http(s) URL.
func parseConfig(raw []byte) (instanceConfig, error) {
	var conf instanceConfig
	if len(strings.TrimSpace(string(raw))) == 0 {
		// No config is valid: the connector falls back to real Graph.
		return conf, nil
	}
	if err := json.Unmarshal(raw, &conf); err != nil {
		return conf, fmt.Errorf("msteams: config is not valid JSON: %w", err)
	}
	conf.BaseURL = strings.TrimSpace(conf.BaseURL)
	if conf.BaseURL != "" {
		u, err := url.Parse(conf.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return conf, fmt.Errorf("msteams: config field base_url %q is not a valid http(s) URL", conf.BaseURL)
		}
	}
	return conf, nil
}

// baseURL returns the effective Graph base URL (override or default), trimmed
// of any trailing slash so path joins are clean.
func (conf instanceConfig) baseURL() string {
	if conf.BaseURL != "" {
		return strings.TrimRight(conf.BaseURL, "/")
	}
	return defaultGraphBaseURL
}

// Validate implements sdk.Connector: the config must parse, and when a token is
// present it must survive one cheap authenticated round-trip (GET /me). It
// emits nothing, and error messages never contain the token.
func (c *Connector) Validate(ctx context.Context, cfg sdk.Config) error {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return err
	}
	if len(cfg.Token) == 0 {
		// No credential yet (instance created before its OAuth flow finished):
		// config-only validation.
		return nil
	}
	gc := newGraphClient(conf, cfg.Token)
	var me struct {
		ID string `json:"id"`
	}
	if err := gc.getJSON(ctx, "/me", &me); err != nil {
		return fmt.Errorf("msteams: credential check failed: %w", err)
	}
	return nil
}

// errGone is the sentinel a graphClient request returns on HTTP 410 Gone, so
// IncrementalSync can translate it to sdk.ErrCursorExpired.
var errGone = errors.New("msteams: graph returned 410 Gone")

// graphClient is a tiny Microsoft Graph REST client bound to one base URL and
// bearer token. The hub owns OAuth refresh; the connector only ever sees a
// currently-valid token.
type graphClient struct {
	base   string
	token  string
	client *http.Client
}

// newGraphClient builds a graphClient for one instance run.
func newGraphClient(conf instanceConfig, token []byte) *graphClient {
	return &graphClient{
		base:   conf.baseURL(),
		token:  string(token),
		client: &http.Client{Timeout: httpTimeout},
	}
}

// getJSON issues GET ref and decodes a JSON response into out. ref may be a
// Graph-relative path ("/me/chats") or an absolute @odata.nextLink/deltaLink
// URL; an absolute URL is rebased onto the client's base URL so pagination and
// delta follow the configured endpoint (dev/CI replay server) rather than
// graph.microsoft.com. A 410 Gone surfaces as errGone.
func (g *graphClient) getJSON(ctx context.Context, ref string, out any) error {
	endpoint, err := g.resolve(ref)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("msteams: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("msteams: GET %s: %w", redactURL(endpoint), err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusGone {
		return errGone
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("msteams: GET %s: unexpected status %d", redactURL(endpoint), resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("msteams: decode %s response: %w", redactURL(endpoint), err)
	}
	return nil
}

// resolve turns a Graph reference into an absolute URL against the client base.
// A relative ref (leading "/") is appended to base. An absolute Graph URL (an
// @odata.nextLink or deltaLink) is rebased: its host/scheme are replaced with
// the client base's, preserving the path+query. Graph's v1.0 links embed the
// "/v1.0" prefix, which the base already carries, so the prefix is stripped to
// avoid duplication.
func (g *graphClient) resolve(ref string) (string, error) {
	base, err := url.Parse(g.base)
	if err != nil {
		return "", fmt.Errorf("msteams: bad base url: %w", err)
	}
	if !strings.HasPrefix(ref, "http://") && !strings.HasPrefix(ref, "https://") {
		return g.base + ensureLeadingSlash(ref), nil
	}
	link, err := url.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("msteams: bad odata link: %w", err)
	}
	path := link.Path
	// Strip a leading API-version segment ("/v1.0", "/beta") so it is not
	// duplicated when the base URL already ends in that version.
	for _, v := range []string{"/v1.0", "/beta"} {
		if strings.HasPrefix(path, v+"/") || path == v {
			path = strings.TrimPrefix(path, v)
			break
		}
	}
	out := *base
	out.Path = strings.TrimRight(base.Path, "/") + path
	out.RawQuery = link.RawQuery
	return out.String(), nil
}

// ensureLeadingSlash makes ref a clean path segment.
func ensureLeadingSlash(ref string) string {
	if strings.HasPrefix(ref, "/") {
		return ref
	}
	return "/" + ref
}

// redactURL strips any query string from a URL before it appears in an error
// (delta/nextLink tokens are opaque, not credentials, but defense in depth).
func redactURL(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i]
	}
	return raw
}
