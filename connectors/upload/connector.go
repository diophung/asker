// Package upload implements the push-style "upload" connector. Documents
// enter Asker through the connector hub's /upload endpoint, which calls
// HandleUpload to store the original in the encrypted blob store and build
// the canonical Document; the hub then stamps ts.ingested and emits to
// Kafka. There is no external source to sync, so the sdk.Connector surface
// is a registered no-op: syncs return immediately and webhooks are
// unsupported.
package upload

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/asker/asker/connectors/sdk"
)

// ID is the stable connector identifier baked into every upload doc_id.
const ID = "upload"

// Connector is the sdk.Connector for direct uploads. It is stateless; the
// zero value is ready to use.
type Connector struct{}

var _ sdk.Connector = Connector{}

// New returns the upload connector, ready to register in an sdk.Registry.
func New() Connector { return Connector{} }

// Spec describes the connector: no credentials, no configuration, no push
// path of its own (uploads arrive via the hub's /upload endpoint, not
// /webhooks).
func (Connector) Spec() sdk.Spec {
	return sdk.Spec{
		ID:              ID,
		DisplayName:     "File Upload",
		AuthType:        sdk.AuthNone,
		ConfigSchema:    json.RawMessage(`{"type":"object"}`),
		SupportsWebhook: false,
	}
}

// Validate accepts any configuration: the connector takes none.
func (Connector) Validate(context.Context, sdk.Config) error { return nil }

// FullSync is a no-op — there is no source to backfill; uploads are pushed
// through HandleUpload as they arrive.
func (Connector) FullSync(context.Context, sdk.Config, sdk.Emit) (sdk.Cursor, error) {
	return "", nil
}

// IncrementalSync is a no-op and returns the cursor unchanged.
func (Connector) IncrementalSync(_ context.Context, _ sdk.Config, cur sdk.Cursor, _ sdk.Emit) (sdk.Cursor, error) {
	return cur, nil
}

// HandleWebhook reports that uploads have no webhook path.
func (Connector) HandleWebhook(context.Context, sdk.Config, *http.Request, sdk.Emit) error {
	return sdk.ErrWebhookUnsupported
}
