package slack

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/asker/asker/connectors/sdk"
)

// Slack signature constants (https://api.slack.com/authentication/verifying-requests-from-slack).
const (
	signatureVersion = "v0"
	signatureHeader  = "X-Slack-Signature"
	timestampHeader  = "X-Slack-Request-Timestamp"
	maxTimestampSkew = 5 * time.Minute
)

// eventCallback is the Slack Events API request envelope. A "url_verification"
// challenge is handled by the hub/gateway (it must echo `challenge` in the
// HTTP response, which HandleWebhook cannot do); HandleWebhook processes the
// "event_callback" type, whose inner `event` carries the actual message event.
type eventCallback struct {
	Type      string          `json:"type"`
	Token     string          `json:"token"`
	TeamID    string          `json:"team_id"`
	Challenge string          `json:"challenge"`
	Event     json.RawMessage `json:"event"`
}

// messageEvent is the inner "message" event Slack pushes. A plain message has
// no subtype; "message_changed" carries the edited message under `message`;
// "message_deleted" carries the deleted ts in `deleted_ts`.
type messageEvent struct {
	Type            string   `json:"type"`
	Subtype         string   `json:"subtype"`
	Channel         string   `json:"channel"`
	ChannelType     string   `json:"channel_type"`
	EventTS         string   `json:"event_ts"`
	User            string   `json:"user"`
	BotID           string   `json:"bot_id"`
	Text            string   `json:"text"`
	TS              string   `json:"ts"`
	ThreadTS        string   `json:"thread_ts"`
	Team            string   `json:"team"`
	DeletedTS       string   `json:"deleted_ts"`
	PreviousChannel string   `json:"previous_channel"`
	Message         *message `json:"message"`
}

// HandleWebhook implements sdk.Connector: the push path for the Slack Events
// API. It verifies the request originated from Slack (X-Slack-Signature HMAC
// over the raw body, with a fresh X-Slack-Request-Timestamp), then for an
// "event_callback" carrying a "message" event emits the affected Document:
//
//   - a plain message (or "message_changed" edit) -> an upsert with the same
//     doc_id and a version_etag derived from the (edited) ts, so an edit
//     replaces the prior version idempotently;
//   - a "message_deleted" -> a tombstone for the deleted ts.
//
// The "url_verification" challenge handshake is NOT handled here: HandleWebhook
// can only emit documents, not write an HTTP response body, so the hub/gateway
// answers the challenge before any event is routed to this connector.
func (c *Connector) HandleWebhook(ctx context.Context, cfg sdk.Config, r *http.Request, emit sdk.Emit) error {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return err
	}
	if r == nil || r.Body == nil {
		return errors.New("slack: webhook request has no body")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		return fmt.Errorf("slack: read webhook body: %w", err)
	}
	if err := c.verifySignature(conf, r.Header, body); err != nil {
		return err
	}

	var env eventCallback
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("slack: webhook payload is not a Slack event envelope: %w", err)
	}
	switch env.Type {
	case "url_verification":
		// The hub/gateway echoes env.Challenge in the HTTP response; nothing
		// to emit here. Returning nil acknowledges a well-formed handshake.
		c.log.Debug("slack url_verification handshake observed; hub answers the challenge",
			"instance_id", cfg.InstanceID)
		return nil
	case "event_callback":
		return c.handleEvent(ctx, cfg, env.Event, emit)
	default:
		c.log.Debug("slack webhook ignored unhandled envelope type",
			"instance_id", cfg.InstanceID, "type", env.Type)
		return nil
	}
}

// handleEvent decodes the inner event and emits the affected Document for a
// "message" event (created/edited/deleted). Non-message events are ignored.
func (c *Connector) handleEvent(_ context.Context, cfg sdk.Config, raw json.RawMessage, emit sdk.Emit) error {
	if len(raw) == 0 {
		return errors.New("slack: event_callback has no event")
	}
	var ev messageEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return fmt.Errorf("slack: webhook event is not a message event: %w", err)
	}
	if ev.Type != "message" {
		c.log.Debug("slack webhook ignored non-message event",
			"instance_id", cfg.InstanceID, "event_type", ev.Type)
		return nil
	}
	tenant := string(cfg.Tenant.TenantID())

	switch ev.Subtype {
	case "message_deleted":
		if ev.DeletedTS == "" || ev.Channel == "" {
			return errors.New("slack: message_deleted event missing channel or deleted_ts")
		}
		return emit(context.Background(), c.tombstoneDocument(tenant, ev.Channel, ev.DeletedTS, ev.EventTS))
	case "message_changed":
		if ev.Message == nil || ev.Channel == "" {
			return errors.New("slack: message_changed event missing channel or message")
		}
		ch := channelInfo{ID: ev.Channel}
		return emit(context.Background(), messageDocument(tenant, ch, *ev.Message))
	case "", "bot_message", "thread_broadcast", "me_message", "file_share":
		if ev.TS == "" || ev.Channel == "" {
			return errors.New("slack: message event missing channel or ts")
		}
		m := message{
			Type:     "message",
			Subtype:  ev.Subtype,
			User:     ev.User,
			BotID:    ev.BotID,
			Text:     ev.Text,
			TS:       ev.TS,
			ThreadTS: ev.ThreadTS,
			Team:     ev.Team,
		}
		ch := channelInfo{ID: ev.Channel}
		return emit(context.Background(), messageDocument(tenant, ch, m))
	default:
		c.log.Debug("slack webhook ignored message subtype",
			"instance_id", cfg.InstanceID, "subtype", ev.Subtype)
		return nil
	}
}

// verifySignature checks the X-Slack-Signature HMAC over the raw body and the
// freshness of X-Slack-Request-Timestamp, per Slack's request-verification
// spec. It requires a signing_secret in the instance config; without one the
// request is rejected (the hub must supply it). Error messages never include
// the secret or the signature.
func (c *Connector) verifySignature(conf instanceConfig, header http.Header, body []byte) error {
	if conf.SigningSecret == "" {
		return errors.New("slack: signing_secret is not configured; cannot verify webhook authenticity")
	}
	tsStr := header.Get(timestampHeader)
	if tsStr == "" {
		return errors.New("slack: webhook missing " + timestampHeader)
	}
	tsSec, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return fmt.Errorf("slack: webhook %s is not an integer: %w", timestampHeader, err)
	}
	if skew := c.now().Sub(time.Unix(tsSec, 0)); skew > maxTimestampSkew || skew < -maxTimestampSkew {
		return errors.New("slack: webhook timestamp is outside the allowed window (possible replay)")
	}

	got := header.Get(signatureHeader)
	if got == "" {
		return errors.New("slack: webhook missing " + signatureHeader)
	}
	base := signatureVersion + ":" + tsStr + ":" + string(body)
	mac := hmac.New(sha256.New, []byte(conf.SigningSecret))
	_, _ = mac.Write([]byte(base))
	want := signatureVersion + "=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(got)) {
		return errors.New("slack: webhook signature mismatch")
	}
	return nil
}

// computeSignature is the signing-side twin of verifySignature. The connector
// test suite uses it to sign fixtures the way Slack would; keeping the HMAC
// construction in one place ensures the two stay in lockstep.
func computeSignature(secret, timestamp string, body []byte) string {
	base := signatureVersion + ":" + timestamp + ":" + string(body)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(base))
	return signatureVersion + "=" + hex.EncodeToString(mac.Sum(nil))
}
