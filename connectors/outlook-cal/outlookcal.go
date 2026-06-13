// Package outlookcal implements the Asker Outlook Calendar connector (M2):
// initial backfill via Microsoft Graph GET /me/events, incremental sync via
// the GET /me/calendarView/delta delta query, and no push path (Graph
// change-notification subscriptions are deferred — see package issues).
//
// The connector is written against the Microsoft Graph v1.0 REST API directly
// with net/http (there is no vendored Graph SDK in go.mod). In production it
// talks to https://graph.microsoft.com/v1.0; in dev/CI the instance config's
// base_url points it at a recorded cassette ReplayServer
// (connectortest.ReplayServer), so contract tests run with no live API calls
// (the M2 exit criterion, ADR-011). The bearer credential arrives in
// sdk.Config.Token already decrypted by the hub vault.
//
// # Cursor format
//
// Cursors are opaque to the hub but parsed here. Two shapes exist:
//
//   - "delta:<deltaLink>" — the steady-state incremental cursor. It wraps the
//     full @odata.deltaLink URL Graph returns at the end of a delta query;
//     IncrementalSync resumes the delta query from it. A 410 Gone (or 400
//     resyncRequired) on the deltaLink means the server pruned the position;
//     IncrementalSync surfaces sdk.ErrCursorExpired and the hub restarts a full
//     sync.
//   - "page:<nextLink>" — a mid-backfill checkpoint. FullSync checkpoints this
//     after every completed @odata.nextLink page; <nextLink> is the next page
//     to fetch. When the hub replays this cursor into IncrementalSync, the
//     connector resumes the backfill at <nextLink> and finishes exactly like
//     FullSync would, returning "delta:<deltaLink>".
//
// FullSync starts a fresh delta query first (so a delta token covering the
// whole backfill window is captured up front) and then pages the backfill;
// every event change racing the backfill is replayed by the first incremental
// pass and converges via idempotent (doc_id, version_etag) upserts.
package outlookcal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/asker/asker/connectors/sdk"
)

const (
	// connectorID is the stable connector identifier baked into every doc_id
	// via sdk.DocID. It must never change.
	connectorID = "outlook-cal"

	// defaultBaseURL is the Microsoft Graph v1.0 API root used when the
	// instance config does not override base_url (dev/CI points it at a
	// cassette ReplayServer instead).
	defaultBaseURL = "https://graph.microsoft.com/v1.0"

	// eventSelect is the $select projection requested on every events call so
	// the connector only fetches the fields it maps. Keeping it stable also
	// keeps cassette query strings stable.
	eventSelect = "id,subject,body,bodyPreview,location,start,end,isAllDay," +
		"organizer,attendees,webLink,createdDateTime,lastModifiedDateTime"

	// pageSize is the $top page size for the backfill and delta queries.
	pageSize = 50

	// clientTimeout bounds a single Graph HTTP round-trip.
	clientTimeout = 30 * time.Second
)

// configSchema is the JSONSchema for instance configuration. base_url is the
// API endpoint override (dev/CI: cassette ReplayServer; empty: real Graph);
// user_principal_name is the mailbox owner used by Validate to cross-check the
// token. base_url is intentionally optional so production instances omit it.
const configSchema = `{
  "type": "object",
  "properties": {
    "base_url": {"type": "string"},
    "user_principal_name": {"type": "string"}
  }
}`

// Connector implements sdk.Connector for Outlook Calendar.
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

// WithClock overrides the clock used for tombstone deleted_at timestamps (for
// deterministic tests).
func WithClock(now func() time.Time) Option {
	return func(c *Connector) {
		if now != nil {
			c.now = now
		}
	}
}

// New returns a ready-to-register Outlook Calendar connector. It is the
// zero-config constructor the hub uses to build instances from Config.
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
		DisplayName:     "Outlook Calendar",
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

// baseURL returns the effective Graph API root: the configured override or the
// real Graph default, always without a trailing slash.
func (conf instanceConfig) baseURL() string {
	if conf.BaseURL != "" {
		return strings.TrimRight(conf.BaseURL, "/")
	}
	return defaultBaseURL
}

// parseConfig decodes and sanity-checks ConfigJSON: it must be a JSON object,
// and a base_url, when given, must be a well-formed http(s) URL.
func parseConfig(raw []byte) (instanceConfig, error) {
	var conf instanceConfig
	if len(raw) == 0 {
		// No config at all is acceptable: every field is optional and the
		// connector falls back to the real Graph base URL.
		return conf, nil
	}
	if err := json.Unmarshal(raw, &conf); err != nil {
		return conf, fmt.Errorf("outlook-cal: config is not valid JSON: %w", err)
	}
	conf.UserPrincipalName = strings.TrimSpace(conf.UserPrincipalName)
	if conf.BaseURL != "" {
		u, err := url.Parse(conf.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return conf, fmt.Errorf("outlook-cal: config field base_url %q is not a valid http(s) URL", conf.BaseURL)
		}
	}
	return conf, nil
}

// Validate implements sdk.Connector: the config must parse, and when a token
// is present it must survive one cheap authenticated round-trip (GET /me).
// Error messages are shown to users verbatim and never include the token.
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
	cl := newClient(conf, cfg.Token)

	var me struct {
		UserPrincipalName string `json:"userPrincipalName"`
		Mail              string `json:"mail"`
	}
	if err := cl.getJSON(ctx, conf.baseURL()+"/me", &me); err != nil {
		return fmt.Errorf("outlook-cal: credential check failed: %w", err)
	}
	if conf.UserPrincipalName != "" {
		for _, got := range []string{me.UserPrincipalName, me.Mail} {
			if got != "" && strings.EqualFold(got, conf.UserPrincipalName) {
				return nil
			}
		}
		if me.UserPrincipalName != "" || me.Mail != "" {
			return fmt.Errorf("outlook-cal: token authenticates %q but config user_principal_name is %q",
				firstNonEmpty(me.UserPrincipalName, me.Mail), conf.UserPrincipalName)
		}
	}
	return nil
}

// firstNonEmpty returns the first non-empty argument, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// HandleWebhook implements sdk.Connector. Outlook Calendar does not implement a
// push path in M2 (Graph change-notification subscriptions are deferred), so
// the hub polls; this always reports the source has no webhook.
func (c *Connector) HandleWebhook(_ context.Context, _ sdk.Config, _ *http.Request, _ sdk.Emit) error {
	return sdk.ErrWebhookUnsupported
}
