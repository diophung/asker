package gmail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/asker/asker/connectors/sdk"
)

// maxWebhookBody bounds the accepted push payload. Real notifications are a
// few hundred bytes.
const maxWebhookBody = 1 << 20

// pushEnvelope is the Cloud Pub/Sub push delivery wrapper Gmail
// notifications arrive in (and the fake-gmail dev shim reproduces).
type pushEnvelope struct {
	Message struct {
		// Data is base64 of a pushData JSON document.
		Data        string `json:"data"`
		MessageID   string `json:"messageId"`
		PublishTime string `json:"publishTime"`
	} `json:"message"`
	Subscription string `json:"subscription"`
}

// pushData is the decoded Gmail notification payload.
type pushData struct {
	EmailAddress string `json:"emailAddress"`
	HistoryID    uint64 `json:"historyId"`
}

// HandleWebhook implements sdk.Connector. Gmail pushes are notification-only
// — they carry no message content — so the handler just validates the
// Pub/Sub envelope shape and that the notification is for this instance's
// mailbox, then returns nil; the hub triggers an immediate IncrementalSync
// after a nil return, which fetches the actual changes via history.list.
func (c *Connector) HandleWebhook(_ context.Context, cfg sdk.Config, r *http.Request, _ sdk.Emit) error {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return err
	}
	if r == nil || r.Body == nil {
		return errors.New("gmail: webhook request has no body")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		return fmt.Errorf("gmail: read webhook body: %w", err)
	}

	var env pushEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("gmail: webhook payload is not a Pub/Sub push envelope: %w", err)
	}
	if env.Message.Data == "" {
		return errors.New("gmail: webhook envelope has no message.data")
	}
	raw, ok := decodeBase64(env.Message.Data)
	if !ok {
		return errors.New("gmail: webhook message.data is not base64")
	}
	var data pushData
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("gmail: webhook message.data is not a Gmail notification: %w", err)
	}
	if data.EmailAddress == "" {
		return errors.New("gmail: webhook notification has no emailAddress")
	}
	if !strings.EqualFold(data.EmailAddress, conf.UserEmail) {
		return fmt.Errorf("gmail: webhook notification is for mailbox %q, not this instance's %q",
			data.EmailAddress, conf.UserEmail)
	}

	c.log.Debug("gmail push notification accepted",
		"instance_id", cfg.InstanceID, "history_id", data.HistoryID)
	return nil
}
