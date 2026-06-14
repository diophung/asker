// Package gdrive implements the Asker Google Drive connector (ADR-012 ACL
// reference): initial backfill via files.list, incremental sync via the Drive
// changes feed (changes.getStartPageToken seeds the cursor; changes.list
// replays it), and — for each file — a permissions.list call that populates
// Document.acl so the pipeline captures who-may-see-it for the shared source.
//
// The connector speaks the Drive v3 REST API (https://www.googleapis.com/drive/v3)
// directly over net/http rather than a vendored SDK: it needs precise control
// of the few endpoints it touches and must stream non-JSON export/media bodies,
// both of which a thin client expresses cleanly. In production the client talks
// to real Drive; in dev/CI the instance config's base_url points it at a replay
// server (connectortest) or a fake. The bearer credential arrives in
// sdk.Config.Token already decrypted by the hub vault and is attached to every
// request as "Authorization: Bearer <token>" — it is never logged.
//
// # Cursor format
//
// Cursors are opaque to the hub but parsed here. Two shapes exist:
//
//   - "page:<changesPageToken>" — the steady-state incremental cursor. It is a
//     Drive changes page token (from changes.getStartPageToken at the end of a
//     backfill, or a newStartPageToken from a prior incremental pass).
//     IncrementalSync replays changes.list from it.
//   - "start:<changesStartToken>|files:<filesPageToken>" — a mid-backfill
//     checkpoint. <changesStartToken> is the changes start page token captured
//     BEFORE the file listing began (so every edit racing the backfill is
//     replayed by the first incremental pass and converges via idempotent
//     (doc_id, version_etag) upserts) and <filesPageToken> is the next
//     files.list page to process. When the hub replays this cursor into
//     IncrementalSync, the connector resumes the backfill at <filesPageToken>
//     and finishes exactly like FullSync would, returning "page:<startToken>".
//
// An unparseable cursor, or a Drive 400/410 reporting the changes page token is
// no longer valid, surfaces as sdk.ErrCursorExpired so the hub restarts a full
// sync rather than looping on a poison cursor.
package gdrive

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

const (
	// connectorID is the stable connector identifier baked into every doc_id
	// via sdk.DocID. It must never change once documents exist.
	connectorID = "gdrive"

	// defaultBaseURL is the real Drive v3 API base used when the instance
	// config supplies no base_url override.
	defaultBaseURL = "https://www.googleapis.com/drive/v3"

	// listPageSize is the files.list / changes.list page size.
	listPageSize = 100

	// clientTimeout bounds a single HTTP round-trip to the source.
	clientTimeout = 30 * time.Second
)

// configSchema is the JSONSchema for instance configuration. base_url is the
// API endpoint override (dev/CI: replay server or fake; empty: real Drive).
// All fields are optional: a Drive instance is fully described by its OAuth
// token, so the connector needs no required configuration.
const configSchema = `{
  "type": "object",
  "properties": {
    "base_url": {"type": "string"}
  }
}`

// Connector implements sdk.Connector for Google Drive.
type Connector struct {
	log *slog.Logger
	now func() time.Time
}

// Option customizes a Connector (logger / clock injection for tests).
type Option func(*Connector)

// WithLogger sets the structured logger (default slog.Default()).
func WithLogger(l *slog.Logger) Option {
	return func(c *Connector) {
		if l != nil {
			c.log = l
		}
	}
}

// withClock overrides the deletion-time clock (tests only).
func withClock(now func() time.Time) Option {
	return func(c *Connector) {
		if now != nil {
			c.now = now
		}
	}
}

// New returns a ready-to-register Google Drive connector. It is the
// zero-config constructor the hub uses; it returns the sdk.Connector interface
// so the registry can hold it directly.
func New(opts ...Option) sdk.Connector {
	c := &Connector{log: slog.Default(), now: time.Now}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Spec implements sdk.Connector. SupportsWebhook is false: Drive push
// (changes.watch) needs a publicly reachable, channel-registered webhook
// endpoint, which is hub infrastructure deferred past M2 — the hub polls via
// IncrementalSync to meet the freshness SLA.
func (c *Connector) Spec() sdk.Spec {
	return sdk.Spec{
		ID:              connectorID,
		DisplayName:     "Google Drive",
		AuthType:        sdk.AuthOAuth2,
		ConfigSchema:    json.RawMessage(configSchema),
		SupportsWebhook: false,
	}
}

// instanceConfig is the parsed ConfigJSON.
type instanceConfig struct {
	BaseURL string `json:"base_url"`
}

// parseConfig decodes and sanity-checks ConfigJSON: it must be a JSON object
// and, when base_url is supplied, a well-formed http(s) URL.
func parseConfig(raw []byte) (instanceConfig, error) {
	var conf instanceConfig
	if len(raw) == 0 {
		// An empty config is valid: the token fully describes the instance.
		return conf, nil
	}
	if err := json.Unmarshal(raw, &conf); err != nil {
		return conf, fmt.Errorf("gdrive: config is not valid JSON: %w", err)
	}
	conf.BaseURL = strings.TrimSpace(conf.BaseURL)
	if conf.BaseURL != "" {
		u, err := url.Parse(conf.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return conf, fmt.Errorf("gdrive: config field base_url %q is not a valid http(s) URL", conf.BaseURL)
		}
	}
	return conf, nil
}

// Validate implements sdk.Connector: the config must parse, and when a token
// is present it must survive one cheap authenticated round-trip (list a single
// file). Error messages are shown to users verbatim and never include the
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
	cl, err := c.newClient(conf, cfg.Token)
	if err != nil {
		return fmt.Errorf("gdrive: build API client: %w", err)
	}
	if _, err := cl.listFiles(ctx, "", 1); err != nil {
		return fmt.Errorf("gdrive: credential check failed: %w", err)
	}
	return nil
}

// HandleWebhook implements sdk.Connector. Drive push is not wired in M2
// (Spec.SupportsWebhook is false), so the hub never routes here; return the
// sentinel so a misconfiguration falls back to polling rather than failing.
func (c *Connector) HandleWebhook(_ context.Context, _ sdk.Config, _ *http.Request, _ sdk.Emit) error {
	return sdk.ErrWebhookUnsupported
}

// errExpiredToken is the marker returned by the client when Drive reports the
// changes page token is no longer valid; IncrementalSync maps it onto
// sdk.ErrCursorExpired.
var errExpiredToken = errors.New("gdrive: changes page token expired at the source")
