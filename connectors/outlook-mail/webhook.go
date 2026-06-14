package outlookmail

import (
	"context"
	"net/http"

	"github.com/asker/asker/connectors/sdk"
)

// HandleWebhook implements sdk.Connector. Outlook Mail has no push path in this
// milestone: Microsoft Graph change notifications require a public https
// notification endpoint plus a subscription lifecycle (create, renew before
// expiry, validate the clientState and validationToken handshake), which is
// deferred to a later milestone. Spec.SupportsWebhook is false, so the hub
// schedules polling only and never routes requests here; this method returns
// sdk.ErrWebhookUnsupported to honor the contract regardless.
func (c *Connector) HandleWebhook(_ context.Context, _ sdk.Config, _ *http.Request, _ sdk.Emit) error {
	return sdk.ErrWebhookUnsupported
}
