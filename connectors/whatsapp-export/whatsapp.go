// Package whatsappexport implements the Asker WhatsApp "Export chat" importer.
//
// WhatsApp's "Export chat" feature produces a _chat.txt file: one logical
// message per timestamped line, with multi-line messages continuing on the
// following (un-timestamped) lines. This connector fetches that export over
// HTTP (the hub uploads the user-provided export to blob storage and supplies a
// URL in the instance config; contract tests point the URL at a replay server),
// parses it, and emits one CHAT_MESSAGE Document per message in file order.
//
// # Auth
//
// AuthNone: there is no API and no credential. The export URL is a plain GET.
//
// # Formats parsed
//
// Both common header families are recognized, in 12h and 24h variants, with or
// without seconds:
//
//	[2024-03-15, 9:42:13 AM] Alice: message text     (bracket)
//	3/15/24, 09:42 - Alice: message text             (dash)
//
// System lines that carry a timestamp header but no "Name: " sender segment
// ("Messages and calls are end-to-end encrypted", "Alice added Bob") are emitted
// as participant-less messages with sender "system" in metadata — we index them
// rather than skip them so they remain searchable. Attachment placeholders
// ("<Media omitted>") are indexed as their literal text.
//
// # Cursor / incremental import
//
// The export is append-only in normal use, so the cursor encodes "<count of
// messages>:<sha256 of the first count messages>". An incremental pass re-fetches
// the export: an unchanged file is a no-op, a grown file with the same prefix
// emits only the new tail, and a changed prefix surfaces sdk.ErrCursorExpired so
// the hub re-imports from scratch. See cursor.go.
//
// SupportsWebhook is false: an uploaded file has no push channel.
package whatsappexport

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
	// connectorID is the stable connector identifier baked into every doc_id via
	// sdk.DocID. It must never change once documents exist.
	connectorID = "whatsapp-export"

	// maxExportBytes bounds a single export download so a hostile or broken URL
	// cannot exhaust memory. WhatsApp text exports are well under this; media is
	// not part of the _chat.txt.
	maxExportBytes = 64 << 20

	// httpTimeout bounds the export download.
	httpTimeout = 60 * time.Second
)

// configSchema is the JSONSchema for instance configuration. export_url is the
// (required at sync time) location of the uploaded _chat.txt; chat_name is an
// optional display name used in titles, metadata, and the doc-id scope.
const configSchema = `{
  "type": "object",
  "properties": {
    "export_url": {"type": "string"},
    "chat_name": {"type": "string"}
  }
}`

// Connector implements sdk.Connector for WhatsApp chat exports.
type Connector struct {
	log  *slog.Logger
	http *http.Client
}

// Option customizes a Connector.
type Option func(*Connector)

// WithLogger sets the structured logger (default slog.Default()). It matches the
// wave-1 connectors' shape so the hub registry wires every connector uniformly.
func WithLogger(l *slog.Logger) Option {
	return func(c *Connector) {
		if l != nil {
			c.log = l
		}
	}
}

// New returns a ready-to-register WhatsApp export connector.
func New(opts ...Option) sdk.Connector {
	c := &Connector{
		log: slog.Default(),
		// export_url is tenant-supplied and fetched server-side; the SSRF-guarded
		// client refuses loopback/metadata/private/cluster IPs at connect time.
		http: safehttp.NewClientOrDefault(safehttp.WithTimeout(httpTimeout)),
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
		DisplayName:     "WhatsApp Export",
		AuthType:        sdk.AuthNone,
		ConfigSchema:    json.RawMessage(configSchema),
		SupportsWebhook: false,
	}
}

// instanceConfig is the parsed ConfigJSON.
type instanceConfig struct {
	ExportURL string `json:"export_url"`
	ChatName  string `json:"chat_name"`
	// BaseURL mirrors export_url for the contract harness, which injects the
	// replay server URL into "base_url" by default. The harness can also be told
	// to inject "export_url" directly; supporting both keeps the connector
	// testable either way. exportURL() resolves the effective URL.
	BaseURL string `json:"base_url"`
}

// exportURL returns the effective export URL: export_url when set, else base_url
// (the contract-harness default field). Whitespace is trimmed.
func (ic instanceConfig) exportURL() string {
	if u := strings.TrimSpace(ic.ExportURL); u != "" {
		return u
	}
	return strings.TrimSpace(ic.BaseURL)
}

// parseConfig decodes and sanity-checks ConfigJSON: valid JSON object and, when
// present, a well-formed http(s) export URL. The chat_name defaults to the empty
// string (titles then read "<empty> message" -> "message"); a real instance
// supplies one.
func parseConfig(raw []byte) (instanceConfig, error) {
	var ic instanceConfig
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &ic); err != nil {
			return ic, fmt.Errorf("whatsapp-export: config is not valid JSON: %w", err)
		}
	}
	ic.ChatName = strings.TrimSpace(ic.ChatName)
	if u := ic.exportURL(); u != "" {
		parsed, err := url.Parse(u)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return ic, fmt.Errorf("whatsapp-export: config field export_url %q is not a valid http(s) URL", u)
		}
	}
	return ic, nil
}

// Validate implements sdk.Connector: the config must parse and, when an export
// URL is configured, it must be a well-formed http(s) URL. There is no
// credential to check (AuthNone) and Validate makes no network call — the export
// may not be uploaded yet when an instance is first created.
func (c *Connector) Validate(_ context.Context, cfg sdk.Config) error {
	if _, err := parseConfig(cfg.ConfigJSON); err != nil {
		return err
	}
	return nil
}

// fetchExport GETs the export URL and returns its body, capped at
// maxExportBytes. ctx is honored. A non-2xx response is an error.
func (c *Connector) fetchExport(ctx context.Context, exportURL string) (string, error) {
	if exportURL == "" {
		return "", errors.New("whatsapp-export: no export_url configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, exportURL, nil)
	if err != nil {
		return "", fmt.Errorf("whatsapp-export: build export request: %w", err)
	}
	req.Header.Set("Accept", "text/plain")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("whatsapp-export: fetch export: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxExportBytes))
	if err != nil {
		return "", fmt.Errorf("whatsapp-export: read export: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("whatsapp-export: export URL returned status %d", resp.StatusCode)
	}
	return string(body), nil
}

// FullSync implements sdk.Connector: fetch the export, parse it, emit a
// CHAT_MESSAGE Document per message in order, and return the cursor pinned to
// the full message count. The whole export is one HTTP GET, so there is a single
// natural checkpoint after it is emitted.
func (c *Connector) FullSync(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
	ic, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return "", err
	}
	c.log.Info("whatsapp-export full sync starting",
		"instance_id", cfg.InstanceID, "chat_name", ic.ChatName)

	body, err := c.fetchExport(ctx, ic.exportURL())
	if err != nil {
		return "", err
	}
	msgs := parseExport(body)
	if err := c.emitRange(ctx, cfg, ic.ChatName, msgs, 0, emit); err != nil {
		return "", err
	}
	cur := makeCursor(msgs, len(msgs))
	if err := cfg.Checkpoint(ctx, cur); err != nil {
		return "", err
	}
	c.log.Info("whatsapp-export full sync complete",
		"instance_id", cfg.InstanceID, "messages", len(msgs))
	return cur, nil
}

// IncrementalSync implements sdk.Connector. It re-fetches the export and
// compares it against cur:
//
//   - unchanged (same count, same prefix hash): emit nothing, return cur;
//   - grown with an unchanged prefix: emit only the new tail and advance;
//   - a changed prefix (edited/deleted earlier messages, or a different chat
//     uploaded): sdk.ErrCursorExpired so the hub re-imports from scratch.
//
// An unparseable cursor is also treated as expired.
func (c *Connector) IncrementalSync(ctx context.Context, cfg sdk.Config, cur sdk.Cursor, emit sdk.Emit) (sdk.Cursor, error) {
	ic, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return "", err
	}
	pc, err := parseCursor(cur)
	if err != nil {
		return "", fmt.Errorf("%v: %w", err, sdk.ErrCursorExpired)
	}

	body, err := c.fetchExport(ctx, ic.exportURL())
	if err != nil {
		return "", err
	}
	msgs := parseExport(body)

	// A shrunk export means earlier messages vanished — the prefix the cursor
	// pins can no longer exist, so re-import.
	if len(msgs) < pc.count {
		return "", fmt.Errorf("whatsapp-export: export shrank from %d to %d messages: %w",
			pc.count, len(msgs), sdk.ErrCursorExpired)
	}

	// The first pc.count messages must hash identically to the cursor, or the
	// append-only assumption is broken (an edit/delete in the prefix).
	if pc.count > 0 && prefixHashOf(msgs, pc.count) != pc.prefixHash {
		return "", fmt.Errorf("whatsapp-export: export prefix changed since the last sync: %w", sdk.ErrCursorExpired)
	}

	if len(msgs) == pc.count {
		c.log.Info("whatsapp-export unchanged", "instance_id", cfg.InstanceID, "messages", len(msgs))
		return cur, nil // no-op
	}

	if err := c.emitRange(ctx, cfg, ic.ChatName, msgs, pc.count, emit); err != nil {
		return "", err
	}
	next := makeCursor(msgs, len(msgs))
	c.log.Info("whatsapp-export appended",
		"instance_id", cfg.InstanceID, "new_messages", len(msgs)-pc.count)
	return next, nil
}

// emitRange emits documents for msgs[from:] in order.
func (c *Connector) emitRange(ctx context.Context, cfg sdk.Config, chatName string, msgs []message, from int, emit sdk.Emit) error {
	tenant := string(cfg.Tenant.TenantID())
	for i := from; i < len(msgs); i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := emit(ctx, toDocument(tenant, chatName, msgs[i])); err != nil {
			return err
		}
	}
	return nil
}

// HandleWebhook implements sdk.Connector. An uploaded export file has no push
// channel, so the connector relies on the hub's polling fallback.
func (c *Connector) HandleWebhook(_ context.Context, _ sdk.Config, _ *http.Request, _ sdk.Emit) error {
	return sdk.ErrWebhookUnsupported
}
