package msteams

import (
	"context"
	"net/http"

	"github.com/asker/asker/connectors/sdk"
)

// HandleWebhook implements sdk.Connector. Microsoft Graph change notifications
// for Teams are deferred (Spec.SupportsWebhook is false, like the other
// Graph-based connectors), so there is no push path: the hub polls via
// IncrementalSync. Per the SDK contract, return sdk.ErrWebhookUnsupported.
func (c *Connector) HandleWebhook(_ context.Context, _ sdk.Config, _ *http.Request, _ sdk.Emit) error {
	return sdk.ErrWebhookUnsupported
}
