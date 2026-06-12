// Package gmail implements the Asker Gmail connector (the M1 flagship,
// ADR-008): initial backfill via users.messages.list + users.messages.get,
// incremental sync via users.history.list, and push via users.watch with
// Pub/Sub-shaped webhook notifications.
//
// The connector is written against the generated google.golang.org/api/gmail/v1
// client. In production the client talks to real Gmail; in dev/CI the
// instance config's base_url points it at the in-repo fake
// (tools/fake-gmail) via option.WithEndpoint. The bearer credential arrives
// in sdk.Config.Token as the raw bearer string (for fake-gmail:
// "fake-gmail-token:<email>").
//
// # Cursor format
//
// Cursors are opaque to the hub but parsed here. Two shapes exist:
//
//   - "history:<historyId>" — the steady-state incremental cursor.
//     IncrementalSync replays users.history.list from <historyId>.
//   - "history:<historyId>|page:<pageToken>" — a mid-backfill checkpoint.
//     FullSync checkpoints this after every completed messages.list page;
//     <historyId> is the mailbox history id captured BEFORE listing began
//     and <pageToken> is the next page to process. When the hub replays
//     this cursor into IncrementalSync, the connector resumes the backfill
//     at <pageToken> (no duplicate, no missing pages) and finishes exactly
//     like FullSync would, returning "history:<historyId>".
//
// Capturing the history id before the backfill means every mailbox change
// that races the backfill is replayed by the first incremental pass, which
// converges via idempotent (doc_id, version_etag) upserts.
package gmail

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

	gmailapi "google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/asker/asker/connectors/sdk"
)

const (
	// connectorID is the stable connector identifier baked into every
	// doc_id via sdk.DocID. It must never change.
	connectorID = "gmail"

	// gmailUser is the userId for every API call: the bearer token
	// determines the mailbox, so "me" is always correct.
	gmailUser = "me"

	// listPageSize is the messages.list / history.list page size mandated
	// by the M1 build contract.
	listPageSize = 100

	// watchTopicName is the Cloud Pub/Sub topic users.watch publishes to.
	// The fake only requires it to be non-empty; real-Gmail deployments
	// must provision this topic (see package issues).
	watchTopicName = "projects/asker-dev/topics/gmail-watch"

	// pushURLHeader is the fake-gmail dev shim (ADR-008): users.watch
	// callers supply the push delivery URL in this header because there is
	// no Cloud Pub/Sub in dev.
	pushURLHeader = "X-Asker-Push-Url"
)

// configSchema is the JSONSchema for instance configuration. base_url is the
// API endpoint override (dev/CI: fake-gmail; empty: real Gmail); user_email
// is the mailbox owner. The hub additionally merges webhook_url into the
// config it passes at runtime; the schema intentionally allows extra keys.
const configSchema = `{
  "type": "object",
  "properties": {
    "base_url": {"type": "string"},
    "user_email": {"type": "string"}
  },
  "required": ["user_email"]
}`

// Connector implements sdk.Connector for Gmail.
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

// New returns a ready-to-register Gmail connector.
func New(opts ...Option) *Connector {
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
		DisplayName:     "Gmail",
		AuthType:        sdk.AuthOAuth2,
		ConfigSchema:    json.RawMessage(configSchema),
		SupportsWebhook: true,
	}
}

// instanceConfig is the parsed ConfigJSON. webhook_url is merged in by the
// hub (it is not part of the user-facing schema).
type instanceConfig struct {
	BaseURL    string `json:"base_url"`
	UserEmail  string `json:"user_email"`
	WebhookURL string `json:"webhook_url"`
}

// parseConfig decodes and sanity-checks ConfigJSON against the published
// schema rules: valid JSON object, non-empty user_email, and a well-formed
// http(s) base_url when one is given.
func parseConfig(raw []byte) (instanceConfig, error) {
	var conf instanceConfig
	if len(raw) == 0 {
		return conf, errors.New("gmail: config is empty; user_email is required")
	}
	if err := json.Unmarshal(raw, &conf); err != nil {
		return conf, fmt.Errorf("gmail: config is not valid JSON: %w", err)
	}
	conf.UserEmail = strings.TrimSpace(conf.UserEmail)
	if conf.UserEmail == "" {
		return conf, errors.New("gmail: config field user_email is required")
	}
	if conf.BaseURL != "" {
		u, err := url.Parse(conf.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return conf, fmt.Errorf("gmail: config field base_url %q is not a valid http(s) URL", conf.BaseURL)
		}
	}
	return conf, nil
}

// Validate implements sdk.Connector: the config must parse, and when a token
// is present it must survive an authenticated round-trip to the source.
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
	svc, err := newService(ctx, conf, cfg.Token)
	if err != nil {
		return fmt.Errorf("gmail: build API client: %w", err)
	}

	prof, err := svc.Users.GetProfile(gmailUser).Context(ctx).Do()
	switch {
	case err == nil:
		if prof.EmailAddress != "" && !strings.EqualFold(prof.EmailAddress, conf.UserEmail) {
			return fmt.Errorf("gmail: token authenticates mailbox %q but config user_email is %q", prof.EmailAddress, conf.UserEmail)
		}
		return nil
	case isNotFound(err):
		// The in-repo fake-gmail does not implement users.getProfile (see
		// package issues); fall back to another authenticated round-trip so
		// the credential is still exercised.
		if _, lerr := svc.Users.Messages.List(gmailUser).MaxResults(1).Context(ctx).Do(); lerr != nil {
			return fmt.Errorf("gmail: credential check failed: %w", lerr)
		}
		return nil
	default:
		return fmt.Errorf("gmail: credential check failed: %w", err)
	}
}

// newService builds a Gmail API client that injects the bearer token from
// cfg.Token on every request. With base_url set (dev/CI) the client resolves
// gmail/v1/... paths against that endpoint; otherwise it talks to real Gmail.
func newService(ctx context.Context, conf instanceConfig, token []byte) (*gmailapi.Service, error) {
	client := &http.Client{
		Transport: &bearerTransport{token: string(token), base: http.DefaultTransport},
		Timeout:   30 * time.Second,
	}
	opts := []option.ClientOption{option.WithHTTPClient(client)}
	if conf.BaseURL != "" {
		opts = append(opts, option.WithEndpoint(conf.BaseURL))
	}
	return gmailapi.NewService(ctx, opts...)
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

// currentHistoryID returns the mailbox's current history id. The canonical
// path is users.getProfile; when that endpoint is absent (the in-repo
// fake-gmail, which 404s the route — see package issues) it falls back to a
// users.history.list probe from history id 0, whose response always carries
// the current history id on the fake.
func (c *Connector) currentHistoryID(ctx context.Context, svc *gmailapi.Service) (uint64, error) {
	prof, err := svc.Users.GetProfile(gmailUser).Context(ctx).Do()
	if err == nil {
		return prof.HistoryId, nil
	}
	if !isNotFound(err) {
		return 0, fmt.Errorf("gmail: users.getProfile: %w", err)
	}
	c.log.Warn("users.getProfile unavailable; probing history.list for the current history id (fake-gmail dev shim)")
	resp, perr := svc.Users.History.List(gmailUser).StartHistoryId(0).MaxResults(1).Context(ctx).Do()
	if perr != nil {
		return 0, fmt.Errorf("gmail: users.getProfile returned 404 and the history.list probe failed: %w", perr)
	}
	return resp.HistoryId, nil
}

// isNotFound reports whether err is an HTTP 404 from the API.
func isNotFound(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == http.StatusNotFound
}
