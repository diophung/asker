// Package jira implements the Asker Jira (Atlassian Cloud) connector: it
// backfills and incrementally syncs issues from a Jira site via the REST v3
// search API and maps each issue to a canonical askerv1.Document of type
// TICKET.
//
// # API subset
//
// The connector uses exactly one read endpoint, POST /rest/api/3/search,
// driven by JQL:
//
//   - FullSync issues jql="order by updated asc" and pages through the whole
//     result set with startAt/maxResults/total.
//   - IncrementalSync issues jql="updated >= '<cursor>' order by updated asc"
//     where <cursor> is the last-seen issue's updated time formatted to Jira's
//     JQL minute precision ("yyyy/MM/dd HH:mm"). Because JQL "updated >=" is
//     minute-granular and inclusive, the boundary issue is re-returned every
//     poll; the connector dedupes it by skipping any issue whose key equals the
//     cursor's boundary issue key (carried in the cursor) so steady-state polls
//     with no changes emit nothing.
//
// Each issue is requested with the fields the mapping needs and
// expand=renderedFields is NOT used; the connector reads the raw Atlassian
// Document Format (ADF) description and walks its node tree to plain text.
//
// # Cursor format
//
// Cursors are opaque to the hub but parsed here. The wire shape is:
//
//	"updated:<yyyy/MM/dd HH:mm>|key:<lastIssueKey>"
//
// <updated> is the JQL-formatted updated time of the newest issue seen so far
// and <lastIssueKey> is that issue's key, used to dedupe the inclusive JQL
// boundary on the next incremental pass. A mid-backfill checkpoint reuses the
// same shape: FullSync checkpoints the cursor of the last issue on each
// completed page, and the hub replays that verbatim into IncrementalSync,
// which simply continues the "updated >=" scan from there (idempotent
// (doc_id, version_etag) upserts absorb any overlap). The empty cursor means
// "from the beginning of time".
//
// # Deletions
//
// Jira's search API does not return deleted issues, and re-checking every
// known issue key each poll is too heavy for M2. This connector therefore
// detects deletions only when the API itself surfaces them: an issue whose
// status category is "done" with a configured deleted-like status name (see
// the deleted_statuses config field) is emitted as a tombstone. With no such
// configuration, M2 incremental sync focuses on created/updated issues and
// delete handling is partial — this matches what the REST search API allows
// without the generally-unavailable recently-deleted JQL. See README.md.
package jira

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
	"github.com/asker/asker/platform/safehttp"
)

const (
	// connectorID is the stable connector identifier baked into every doc_id
	// via sdk.DocID. It must never change once documents exist.
	connectorID = "jira"

	// defaultBaseURL is the Atlassian Cloud REST base when the instance config
	// does not override it. A real site is https://<site>.atlassian.net.
	defaultBaseURL = "https://your-domain.atlassian.net"

	// searchPath is the only read endpoint the connector calls.
	searchPath = "/rest/api/3/search"

	// pageSize is the maxResults page size for the search API.
	pageSize = 50

	// clientTimeout bounds every HTTP round-trip to the source.
	clientTimeout = 30 * time.Second

	// issueFieldList is the comma-separated field list requested per issue:
	// only what the Document mapping reads, to keep responses small.
	issueFieldList = "summary,description,status,issuetype,priority,project,reporter,assignee,creator,created,updated,comment"
)

// configSchema is the JSONSchema for instance configuration.
//
//   - base_url       — API endpoint (real Jira site, or the contract test's
//     replay server). Defaults to defaultBaseURL.
//   - project_keys   — optional list of Jira project keys to restrict the sync
//     to; empty means every project the token can read.
//   - deleted_statuses — optional list of status names that mean "deleted" at
//     the source; an issue whose status matches is tombstoned (see package doc
//     and README on partial delete handling).
const configSchema = `{
  "type": "object",
  "properties": {
    "base_url": {"type": "string"},
    "project_keys": {"type": "array", "items": {"type": "string"}},
    "deleted_statuses": {"type": "array", "items": {"type": "string"}}
  }
}`

// Connector implements sdk.Connector for Jira.
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

// New returns a ready-to-register Jira connector. It takes no required
// configuration; the hub builds instances from Config.
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
		DisplayName:     "Jira",
		AuthType:        sdk.AuthOAuth2,
		ConfigSchema:    json.RawMessage(configSchema),
		SupportsWebhook: false,
	}
}

// instanceConfig is the parsed ConfigJSON.
type instanceConfig struct {
	BaseURL         string   `json:"base_url"`
	ProjectKeys     []string `json:"project_keys"`
	DeletedStatuses []string `json:"deleted_statuses"`
}

// parseConfig decodes and sanity-checks ConfigJSON: valid JSON object and a
// well-formed http(s) base_url when one is given. An empty config is valid
// (the connector falls back to defaultBaseURL).
func parseConfig(raw []byte) (instanceConfig, error) {
	var conf instanceConfig
	if len(strings.TrimSpace(string(raw))) == 0 {
		return conf, nil
	}
	if err := json.Unmarshal(raw, &conf); err != nil {
		return conf, fmt.Errorf("jira: config is not valid JSON: %w", err)
	}
	conf.BaseURL = strings.TrimSpace(conf.BaseURL)
	if conf.BaseURL != "" {
		u, err := url.Parse(conf.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return conf, fmt.Errorf("jira: config field base_url %q is not a valid http(s) URL", conf.BaseURL)
		}
	}
	return conf, nil
}

// baseURL returns the configured base URL or the default, with any trailing
// slash trimmed so path joins are clean.
func (conf instanceConfig) baseURL() string {
	b := conf.BaseURL
	if b == "" {
		b = defaultBaseURL
	}
	return strings.TrimRight(b, "/")
}

// Validate implements sdk.Connector: the config must parse, and when a token
// is present it must survive one cheap authenticated round-trip (a single-page
// search). Error messages are shown to users verbatim and never include the
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
	cl := c.newClient(conf, cfg.Token)
	if _, err := cl.search(ctx, "order by updated asc", 0, 1); err != nil {
		// The upstream Jira body may name fields, accounts, or filter detail;
		// it is logged server-side by search and must not reach the user. Surface
		// only a generic, credential-free reachability message.
		c.log.Debug("jira validate credential check failed", "instance_id", cfg.InstanceID, "error", err)
		return errors.New("jira: could not reach Jira with the supplied credentials; check the site URL and that the connection is authorized")
	}
	return nil
}

// client is the thin Jira REST client: a *http.Client whose transport injects
// the bearer credential plus the resolved base URL.
type client struct {
	http *http.Client
	base string
	// log records server-side diagnostics (e.g. the upstream error detail on a
	// non-200) at debug level. The user-facing error never carries that detail.
	log *slog.Logger
}

// newClient builds a Jira REST client that adds "Authorization: Bearer
// <token>" to every request. The hub owns refresh; the connector only ever
// sees a currently-valid token.
func (c *Connector) newClient(conf instanceConfig, token []byte) *client {
	return &client{
		http: &http.Client{
			// base_url is tenant-supplied; the SSRF-guarded base refuses
			// loopback/metadata/private/cluster IPs at connect time so the bearer
			// token is never sent to an internal host.
			Transport: &bearerTransport{token: string(token), base: safehttp.GuardedBase()},
			Timeout:   clientTimeout,
		},
		base: conf.baseURL(),
		log:  c.log,
	}
}

// bearerTransport adds "Authorization: Bearer <token>" and a JSON Accept
// header to every request without mutating the caller's request.
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

// searchRequest is the POST /rest/api/3/search body.
type searchRequest struct {
	JQL        string   `json:"jql"`
	StartAt    int      `json:"startAt"`
	MaxResults int      `json:"maxResults"`
	Fields     []string `json:"fields"`
}

// search runs one page of POST /rest/api/3/search. A 4xx for a bad/expired
// JQL bound surfaces to the caller as a typed apiError so IncrementalSync can
// map it to sdk.ErrCursorExpired.
func (cl *client) search(ctx context.Context, jql string, startAt, maxResults int) (*searchResponse, error) {
	body, err := json.Marshal(searchRequest{
		JQL:        jql,
		StartAt:    startAt,
		MaxResults: maxResults,
		Fields:     strings.Split(issueFieldList, ","),
	})
	if err != nil {
		return nil, fmt.Errorf("jira: marshal search request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cl.base+searchPath, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("jira: build search request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := cl.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jira: search request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		ae := readAPIError(resp)
		// Log the upstream detail server-side only; the apiError surfaced to the
		// caller (and on to the user via Validate/IncrementalSync) is
		// credential- and body-free.
		if cl.log != nil && len(ae.messages) > 0 {
			cl.log.Debug("jira API returned a non-200 response",
				"status", ae.status, "upstream_messages", ae.messages)
		}
		return nil, ae
	}
	var out searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("jira: decode search response: %w", err)
	}
	return &out, nil
}

// apiError is a non-200 response from the Jira REST API. The upstream error
// detail (messages) is retained for server-side logging only; Error() never
// includes it, so echoing an apiError to the user cannot leak field names,
// account ids, filter detail, or other upstream content.
type apiError struct {
	status   int
	messages []string
}

// Error implements error. It is intentionally generic — status-only — because
// this string can reach the user via Validate/IncrementalSync. The upstream
// messages are logged server-side at debug (see client.search), not surfaced.
func (e *apiError) Error() string {
	return fmt.Sprintf("jira: API returned status %d", e.status)
}

// readAPIError builds an apiError from a non-200 response, parsing the
// standard Jira error envelope ({"errorMessages":[...],"errors":{...}}) when
// present. The parsed messages are kept for server-side logging only.
func readAPIError(resp *http.Response) *apiError {
	var env struct {
		ErrorMessages []string          `json:"errorMessages"`
		Errors        map[string]string `json:"errors"`
	}
	// Best-effort decode; an unparseable body still yields a status-only error.
	_ = json.NewDecoder(resp.Body).Decode(&env)
	msgs := append([]string(nil), env.ErrorMessages...)
	for field, msg := range env.Errors {
		msgs = append(msgs, field+": "+msg)
	}
	return &apiError{status: resp.StatusCode, messages: msgs}
}

// isCursorRejected reports whether err is an API error that means the JQL
// bound (the cursor) is no longer usable — Jira returns 400 with an
// "errors.jql" / "errorMessages" complaint for a malformed bound time.
func isCursorRejected(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.status == http.StatusBadRequest
}
