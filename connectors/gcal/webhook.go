package gcal

import (
	"context"
	"net/http"

	"github.com/asker/asker/connectors/sdk"
)

// HandleWebhook implements sdk.Connector. Calendar push channels
// (events.watch) require a publicly reachable HTTPS callback URL, which the dev
// stack and the current hub deployment do not provide, so this connector
// declares SupportsWebhook=false and relies on the hub's polling fallback.
// Returning sdk.ErrWebhookUnsupported tells the hub to poll rather than route
// requests here.
func (c *Connector) HandleWebhook(_ context.Context, _ sdk.Config, _ *http.Request, _ sdk.Emit) error {
	return sdk.ErrWebhookUnsupported
}
