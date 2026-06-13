// Package ical implements the Asker iCalendar (RFC 5545) feed connector. It
// indexes a public or tokenized .ics feed URL — the kind exported by Google
// Calendar "secret address in iCal format", Outlook published calendars, Apple
// iCloud shared calendars, conference schedules, and the like — into Asker as
// CALENDAR_EVENT documents.
//
// # Auth
//
// AuthNone. An iCal feed is fetched by URL alone; access control is carried in
// the (often unguessable) URL itself, so the hub supplies no credential and
// Config.Token is empty. The connector never logs the feed URL's query string
// (it can be a bearer-equivalent secret) beyond what slog records at the field
// it is given.
//
// # Config
//
//	{"feed_url": "https://calendar.example.com/private-xxxx/basic.ics"}
//
// feed_url is the only field; it is the absolute http(s) URL of the .ics feed.
// In dev/CI the contract harness overwrites feed_url with a ReplayServer URL.
//
// # Cursor format
//
// A feed is pulled in full every time (there is no incremental API), so the
// cursor is a content fingerprint of the whole feed:
//
//	"<sha256-of-feed-body>:<max-DTSTAMP>"
//
// FullSync emits every VEVENT and returns this cursor. IncrementalSync re-fetches
// the feed: when the body hash equals the hash in the cursor, nothing changed and
// the same cursor is returned with no emissions; otherwise the new feed is diffed
// against the cursor's baseline (the connector re-derives the previous event set
// is not possible from the cursor alone, so the diff is by presence/sequence — see
// sync.go) and changed/new events are emitted as upserts while events that
// vanished from the feed or carry a canceled STATUS are emitted as tombstones.
//
// # Webhooks
//
// SupportsWebhook=false. Feeds are pull-only; HandleWebhook returns
// sdk.ErrWebhookUnsupported and the hub polls on its freshness schedule.
package ical

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
)

const (
	// connectorID is the stable connector identifier baked into every doc_id
	// via sdk.DocID. It must never change once documents exist.
	connectorID = "ical"

	// httpTimeout bounds a single feed fetch.
	httpTimeout = 30 * time.Second

	// maxFeedBytes bounds a feed read so a hostile or runaway feed cannot
	// exhaust memory. 64 MiB is far above any realistic .ics export.
	maxFeedBytes = 64 << 20
)

// configSchema is the JSONSchema for instance configuration: a single required
// feed_url string.
const configSchema = `{
  "type": "object",
  "properties": {
    "feed_url": {"type": "string"}
  },
  "required": ["feed_url"]
}`

// Connector implements sdk.Connector for iCalendar feeds.
type Connector struct {
	log  *slog.Logger
	now  func() time.Time
	http *http.Client
}

// Option customizes a Connector.
type Option func(*Connector)

// WithLogger sets the structured logger (default slog.Default()). It mirrors the
// wave-1 connectors so the hub registry can wire every connector uniformly.
func WithLogger(l *slog.Logger) Option {
	return func(c *Connector) {
		if l != nil {
			c.log = l
		}
	}
}

// New returns a ready-to-register iCalendar feed connector.
func New(opts ...Option) sdk.Connector {
	c := &Connector{
		log:  slog.Default(),
		now:  time.Now,
		http: &http.Client{Timeout: httpTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Spec implements sdk.Connector.
func (c *Connector) Spec() sdk.Spec {
	return sdk.Spec{
		ID:              connectorID,
		DisplayName:     "iCalendar Feed",
		AuthType:        sdk.AuthNone,
		ConfigSchema:    json.RawMessage(configSchema),
		SupportsWebhook: false,
	}
}

// instanceConfig is the parsed ConfigJSON.
type instanceConfig struct {
	FeedURL string `json:"feed_url"`
}

// parseConfig decodes and sanity-checks ConfigJSON: a valid JSON object with a
// well-formed absolute http(s) feed_url.
func parseConfig(raw []byte) (instanceConfig, error) {
	var conf instanceConfig
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &conf); err != nil {
			return conf, fmt.Errorf("ical: config is not valid JSON: %w", err)
		}
	}
	conf.FeedURL = strings.TrimSpace(conf.FeedURL)
	if conf.FeedURL == "" {
		return conf, errors.New("ical: config field feed_url is required")
	}
	u, err := url.Parse(conf.FeedURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return conf, fmt.Errorf("ical: config field feed_url %q is not a valid http(s) URL", conf.FeedURL)
	}
	return conf, nil
}

// Validate implements sdk.Connector: the config must parse, and the feed must be
// fetchable and parse into at least a well-formed VCALENDAR document. Error
// messages are shown to users verbatim; the feed URL may itself be a secret, so
// errors never echo its query string.
func (c *Connector) Validate(ctx context.Context, cfg sdk.Config) error {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return err
	}
	body, err := c.fetch(ctx, conf.FeedURL)
	if err != nil {
		return fmt.Errorf("ical: feed fetch failed: %w", redactURL(err, conf.FeedURL))
	}
	if !looksLikeICalendar(body) {
		return errors.New("ical: feed did not return iCalendar data (no BEGIN:VCALENDAR)")
	}
	return nil
}

// fetch performs one GET of feedURL, honoring ctx and the body-size cap. A non-2xx
// status is an error. The returned body is the raw feed text.
func (c *Connector) fetch(ctx context.Context, feedURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return "", fmt.Errorf("build feed request: %w", err)
	}
	req.Header.Set("Accept", "text/calendar, text/plain, */*")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes))
	if err != nil {
		return "", fmt.Errorf("read feed response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("feed status %d", resp.StatusCode)
	}
	return string(body), nil
}

// looksLikeICalendar reports whether body appears to be an iCalendar document.
func looksLikeICalendar(body string) bool {
	return strings.Contains(strings.ToUpper(body), "BEGIN:VCALENDAR")
}

// feedHash is the lowercase-hex SHA-256 of the raw feed body — the content
// fingerprint half of the cursor.
func feedHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// redactURL strips feedURL's query string out of an error so a tokenized feed
// URL never leaks into a user-facing message or a log line. Most net/http errors
// embed the full URL (including the secret query) in their text.
func redactURL(err error, feedURL string) error {
	if err == nil {
		return nil
	}
	u, perr := url.Parse(feedURL)
	if perr != nil || u.RawQuery == "" {
		return err
	}
	redacted := strings.ReplaceAll(err.Error(), u.RawQuery, "REDACTED")
	return errors.New(redacted)
}
