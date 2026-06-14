// Package outlookmail implements the Asker Outlook Mail connector for
// Microsoft 365 over Microsoft Graph (v1.0): initial backfill via
// GET /me/messages (paged with @odata.nextLink), incremental sync via the
// GET /me/messages/delta delta query (cursor = the returned @odata.deltaLink),
// and no push path (Graph change notifications require a public https endpoint
// and subscription lifecycle — deferred to a later milestone).
//
// The connector is written against the Graph REST API directly with net/http
// (there is no vendored Graph SDK in the module). In production the client
// talks to https://graph.microsoft.com/v1.0; in dev/CI the instance config's
// base_url points it at a replay server / fake. The bearer credential arrives
// in sdk.Config.Token as the raw OAuth2 access token, decrypted by the hub.
//
// # Cursor format
//
// Cursors are opaque to the hub but interpreted here. A cursor is the absolute
// delta URL Graph hands back so a later poll resumes exactly where the last one
// stopped:
//
//   - "" (empty) — no position yet. IncrementalSync treats this as a stale
//     cursor (ErrCursorExpired) so the hub runs a FullSync first; FullSync
//     establishes the first real cursor.
//   - a "<base>/me/messages/delta?$deltatoken=..." URL — the steady-state
//     incremental cursor (the @odata.deltaLink from the previous delta round).
//
// FullSync derives the first delta cursor by walking the delta query to its
// end (Graph returns @odata.deltaLink on the last delta page); it also paginates
// /me/messages for the document backfill, checkpointing each page's
// @odata.nextLink so an interrupted backfill resumes instead of restarting.
//
// A 410 Gone on a stale deltaLink means the source can no longer replay from
// that token; IncrementalSync wraps it as sdk.ErrCursorExpired and the hub
// restarts a full sync.
package outlookmail

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
	// connectorID is the stable connector identifier baked into every doc_id
	// via sdk.DocID. It must never change once documents exist.
	connectorID = "outlook-mail"

	// defaultBaseURL is the real Microsoft Graph v1.0 base. The instance
	// config's base_url overrides it for dev/CI (replay server / fake).
	defaultBaseURL = "https://graph.microsoft.com/v1.0"

	// pageSize is the $top page size for both the message list and the delta
	// query. Graph caps message list pages at 1000; 50 keeps pages small and
	// resumable.
	pageSize = 50

	// httpTimeout bounds a single Graph request. The hub owns overall sync
	// budgeting; this only guards against a hung socket.
	httpTimeout = 30 * time.Second

	// maxBodyBytes bounds a single Graph JSON response read so a pathological
	// upstream cannot exhaust memory.
	maxBodyBytes = 32 << 20
)

// messageSelect is the $select projection requested for every message: only
// the fields the Document mapping consumes, to keep payloads small.
const messageSelect = "id,subject,body,bodyPreview,from,toRecipients,ccRecipients," +
	"webLink,parentFolderId,conversationId,createdDateTime,lastModifiedDateTime"

// configSchema is the JSONSchema for instance configuration. base_url is the
// API endpoint override (dev/CI; empty -> real Graph); user_principal_name is
// the mailbox owner used as a sanity check during Validate.
const configSchema = `{
  "type": "object",
  "properties": {
    "base_url": {"type": "string"},
    "user_principal_name": {"type": "string"}
  }
}`

// Connector implements sdk.Connector for Outlook Mail (Microsoft Graph).
type Connector struct {
	log *slog.Logger
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

// New returns a ready-to-register Outlook Mail connector. It is the
// zero-config constructor the hub uses to build instances from Config.
func New(opts ...Option) sdk.Connector {
	c := &Connector{log: slog.Default()}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Spec implements sdk.Connector.
func (c *Connector) Spec() sdk.Spec {
	return sdk.Spec{
		ID:              connectorID,
		DisplayName:     "Outlook Mail",
		AuthType:        sdk.AuthOAuth2,
		ConfigSchema:    json.RawMessage(configSchema),
		SupportsWebhook: false,
	}
}

// instanceConfig is the parsed ConfigJSON.
type instanceConfig struct {
	BaseURL           string `json:"base_url"`
	UserPrincipalName string `json:"user_principal_name"`
}

// parseConfig decodes and sanity-checks ConfigJSON: it must be a JSON object,
// and base_url, when supplied, must be a well-formed http(s) URL. The base
// URL defaults to real Graph when omitted.
func parseConfig(raw []byte) (instanceConfig, error) {
	var conf instanceConfig
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &conf); err != nil {
			return conf, fmt.Errorf("outlook-mail: config is not valid JSON: %w", err)
		}
	}
	conf.BaseURL = strings.TrimRight(strings.TrimSpace(conf.BaseURL), "/")
	conf.UserPrincipalName = strings.TrimSpace(conf.UserPrincipalName)
	if conf.BaseURL == "" {
		conf.BaseURL = defaultBaseURL
	} else {
		u, err := url.Parse(conf.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return conf, fmt.Errorf("outlook-mail: config field base_url %q is not a valid http(s) URL", conf.BaseURL)
		}
	}
	return conf, nil
}

// Validate implements sdk.Connector: the config must parse, and when a token
// is present it must survive one cheap authenticated round-trip (list one
// message). Error messages are shown to users verbatim and never include the
// token.
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
	client := newClient(cfg.Token)
	probe := conf.BaseURL + "/me/messages?$select=id&$top=1"
	var page messageListPage
	if err := c.getJSON(ctx, client, probe, &page); err != nil {
		return fmt.Errorf("outlook-mail: credential check failed: %w", err)
	}
	return nil
}

// newClient builds an *http.Client whose transport adds the bearer credential
// from token to every request. The hub owns refresh; the connector only ever
// sees a currently-valid token.
func newClient(token []byte) *http.Client {
	return &http.Client{
		// base_url is tenant-supplied; the SSRF-guarded base refuses
		// loopback/metadata/private/cluster IPs at connect time so the bearer
		// token is never sent to an internal host.
		Transport: &bearerTransport{token: string(token), base: safehttp.GuardedBase()},
		Timeout:   httpTimeout,
	}
}

// bearerTransport adds "Authorization: Bearer <token>" to every request
// without mutating the caller's request.
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

// graphError is an HTTP-status-carrying error returned by getJSON for non-2xx
// responses, so callers can detect the 410 Gone that signals an expired delta
// cursor.
type graphError struct {
	status int
	body   string
}

func (e *graphError) Error() string {
	// The Graph error body carries a machine code and message (never the token,
	// which travels only in the request header), so include a bounded snippet
	// for diagnostics.
	snippet := e.body
	const max = 200
	if len(snippet) > max {
		snippet = snippet[:max]
	}
	if snippet == "" {
		return fmt.Sprintf("outlook-mail: graph returned status %d", e.status)
	}
	return fmt.Sprintf("outlook-mail: graph returned status %d: %s", e.status, snippet)
}

// isGone reports whether err is a Graph 410 Gone (a stale delta token).
func isGone(err error) bool {
	var ge *graphError
	return errors.As(err, &ge) && ge.status == http.StatusGone
}

// getJSON GETs rawURL with client, honoring ctx, and decodes a 2xx JSON body
// into out. A non-2xx response becomes a *graphError (never including the
// token, which lives only in the request header). The response body is bounded
// by maxBodyBytes.
func (c *Connector) getJSON(ctx context.Context, client *http.Client, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("outlook-mail: build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("outlook-mail: GET %s: %w", redactURL(rawURL), err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("outlook-mail: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &graphError{status: resp.StatusCode, body: string(body)}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("outlook-mail: decode response from %s: %w", redactURL(rawURL), err)
	}
	return nil
}

// redactURL strips the query string from a URL for logging/errors: delta and
// skip tokens are not secrets but are noisy, and we never want to risk echoing
// anything sensitive a source appended.
func redactURL(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i] + "?<redacted>"
	}
	return raw
}
