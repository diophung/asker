// Package gcal implements the Asker Google Calendar connector (Calendar API
// v3): initial backfill via events.list?singleEvents=true paging, incremental
// sync via the calendar sync token (events.list?syncToken=...), and tombstones
// from events the source reports as canceled. Calendar push channels need a
// publicly reachable URL, so HandleWebhook is deferred and the hub polls
// (SupportsWebhook=false).
//
// The connector talks to the Calendar REST API over a plain net/http client
// (there is no need to pull in the generated client just to GET two endpoints).
// In production base_url is the real API; in dev/CI the instance config's
// base_url points it at a contract-test ReplayServer. The OAuth2 access token
// arrives in sdk.Config.Token already decrypted by the hub; the client's
// RoundTripper adds it as "Authorization: Bearer <token>" to every request.
//
// # Cursor format
//
// Cursors are opaque to the hub but parsed here. Two shapes exist:
//
//   - "sync:<syncToken>" — the steady-state incremental cursor. FullSync
//     returns it (the nextSyncToken the final backfill page carried) and
//     IncrementalSync replays events.list?syncToken=<syncToken> from it.
//   - "page:<pageToken>" — a mid-backfill checkpoint. FullSync checkpoints
//     this after every completed events.list page; <pageToken> is the next
//     page to fetch. When the hub replays this cursor into IncrementalSync the
//     connector resumes the backfill at <pageToken> and finishes exactly like
//     FullSync would, returning "sync:<syncToken>".
//
// A 410 Gone on a syncToken (the source pruned that position) surfaces as
// sdk.ErrCursorExpired so the hub clears the cursor and reruns FullSync.
package gcal

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
	connectorID = "gcal"

	// defaultBaseURL is the real Calendar API root used when the instance
	// config omits base_url.
	defaultBaseURL = "https://www.googleapis.com/calendar/v3"

	// defaultCalendarID is the calendar synced when the config omits one.
	// "primary" is the connecting account's own calendar.
	defaultCalendarID = "primary"

	// listPageSize is the events.list page size. Calendar caps a page at 2500;
	// 250 keeps individual responses small while bounding round-trips.
	listPageSize = 250

	// httpTimeout bounds a single API round-trip.
	httpTimeout = 30 * time.Second
)

// configSchema is the JSONSchema for instance configuration. base_url is the
// API endpoint override (dev/CI: the replay server; empty: the real API);
// calendar_id selects the calendar (default "primary").
const configSchema = `{
  "type": "object",
  "properties": {
    "base_url": {"type": "string"},
    "calendar_id": {"type": "string"}
  }
}`

// Connector implements sdk.Connector for Google Calendar.
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

// New returns a ready-to-register Google Calendar connector. It is the
// zero-config constructor the hub builds instances from.
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
		DisplayName:     "Google Calendar",
		AuthType:        sdk.AuthOAuth2,
		ConfigSchema:    json.RawMessage(configSchema),
		SupportsWebhook: false,
	}
}

// instanceConfig is the parsed ConfigJSON.
type instanceConfig struct {
	BaseURL    string `json:"base_url"`
	CalendarID string `json:"calendar_id"`
}

// parseConfig decodes and sanity-checks ConfigJSON: valid JSON object and a
// well-formed http(s) base_url when one is given. calendar_id defaults to
// "primary"; base_url defaults to the real API.
func parseConfig(raw []byte) (instanceConfig, error) {
	var conf instanceConfig
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &conf); err != nil {
			return conf, fmt.Errorf("gcal: config is not valid JSON: %w", err)
		}
	}
	conf.BaseURL = strings.TrimSpace(conf.BaseURL)
	conf.CalendarID = strings.TrimSpace(conf.CalendarID)
	if conf.CalendarID == "" {
		conf.CalendarID = defaultCalendarID
	}
	if conf.BaseURL == "" {
		conf.BaseURL = defaultBaseURL
	} else {
		u, err := url.Parse(conf.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return conf, fmt.Errorf("gcal: config field base_url %q is not a valid http(s) URL", conf.BaseURL)
		}
	}
	return conf, nil
}

// Validate implements sdk.Connector: the config must parse, and when a token
// is present it must survive one cheap authenticated round-trip (list a single
// event). Error messages are shown to users verbatim and never include the
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
	cl := newClient(conf, cfg.Token)
	if _, err := cl.listEvents(ctx, listParams{maxResults: 1}); err != nil {
		if errors.Is(err, sdk.ErrCursorExpired) {
			// A bare listing carries no syncToken, so a 410 cannot occur here;
			// guard anyway so a credential check never leaks the sentinel.
			return errors.New("gcal: credential check failed")
		}
		return fmt.Errorf("gcal: credential check failed: %w", err)
	}
	return nil
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
