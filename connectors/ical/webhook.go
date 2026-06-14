package ical

import (
	"context"
	"net/http"

	"github.com/asker/asker/connectors/sdk"
)

// HandleWebhook implements sdk.Connector. An iCalendar feed is pull-only: there
// is no push channel a feed can notify Asker through, so this connector declares
// SupportsWebhook=false and returns sdk.ErrWebhookUnsupported, which tells the
// hub to poll on its freshness schedule rather than route requests here.
func (c *Connector) HandleWebhook(_ context.Context, _ sdk.Config, _ *http.Request, _ sdk.Emit) error {
	return sdk.ErrWebhookUnsupported
}
