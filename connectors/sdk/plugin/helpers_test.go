package plugin

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/asker/asker/connectors/sdk"
	pluginv1 "github.com/asker/asker/platform/proto/gen/go/asker/plugin/v1"
)

// makeWebhookReq builds a minimal POST webhook request with an empty body.
func makeWebhookReq() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/hook", http.NoBody)
}

// makeWebhookReqBody builds a POST webhook request carrying body.
func makeWebhookReqBody(body []byte) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/hook", bytes.NewReader(body))
}

// readAll reads body to completion, failing the test on error.
func readAll(t *testing.T, body io.ReadCloser) []byte {
	t.Helper()
	if body == nil {
		return nil
	}
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return out
}

// badTenantConfig returns a wire Config whose tenant_id violates the tenancy
// syntax rules, so configFromProto must reject it.
func badTenantConfig() *pluginv1.Config {
	return &pluginv1.Config{
		TenantId:   "bad tenant/with spaces", // space and slash are not allowed
		InstanceId: "inst-1",
	}
}

// ctxObservingConnector is a fake whose FullSync emits documents one at a time
// and checks ctx between emits. When the hub-side emit fails, the client cancels
// the stream context; the server forwards that cancellation into emit, so the
// connector's own ctx becomes canceled and it closes sawCancel and stops. If it
// ever emits all `total` docs it closes emittedAll instead, signaling the pass
// was NOT aborted.
type ctxObservingConnector struct {
	total       int
	emitGapWait time.Duration
	sawCancel   chan struct{}
	emittedAll  chan struct{}
}

func (c *ctxObservingConnector) Spec() sdk.Spec {
	return (&fakeConnector{}).Spec()
}

func (c *ctxObservingConnector) Validate(context.Context, sdk.Config) error { return nil }

func (c *ctxObservingConnector) FullSync(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
	for n := 0; n < c.total; n++ {
		if err := ctx.Err(); err != nil {
			close(c.sawCancel)
			return "", err
		}
		if err := emit(ctx, makeDoc(cfg, "full", n)); err != nil {
			// The emit error here is the server-side Send failure once the hub
			// cancels the stream. Surface it as terminal.
			return "", err
		}
		// Give the failing hub-side emit time to land and cancel the stream
		// before the next iteration.
		select {
		case <-ctx.Done():
			close(c.sawCancel)
			return "", ctx.Err()
		case <-time.After(c.emitGapWait):
		}
	}
	close(c.emittedAll)
	return "done", nil
}

func (c *ctxObservingConnector) IncrementalSync(context.Context, sdk.Config, sdk.Cursor, sdk.Emit) (sdk.Cursor, error) {
	return "", nil
}

func (c *ctxObservingConnector) HandleWebhook(context.Context, sdk.Config, *http.Request, sdk.Emit) error {
	return sdk.ErrWebhookUnsupported
}

// compile-time assertions that the test connectors satisfy sdk.Connector.
var (
	_ sdk.Connector = (*fakeConnector)(nil)
	_ sdk.Connector = (*ctxObservingConnector)(nil)
)
