package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

// fakeConnectorID is the stable id the fake connector reports and bakes into
// every doc_id, so ValidateDocument's DocID check passes.
const fakeConnectorID = "fakeconn"

// fakeConnector is a deterministic sdk.Connector used by the parity suite. Its
// behavior is fully driven by exported fields so a single test can shape exactly
// the pass it needs (N docs, a checkpoint cadence, a cursor-expired return, a
// webhook that emits or is unsupported, a Validate error).
type fakeConnector struct {
	supportsWebhook bool

	// validateErr, when set, is returned by Validate.
	validateErr error

	// fullSyncDocs is the number of documents FullSync emits; it checkpoints
	// after each. fullSyncCursor is the cursor it returns on success.
	fullSyncDocs   int
	fullSyncCursor sdk.Cursor

	// incrementalErr, when set, is returned by IncrementalSync after emitting
	// incrementalDocs documents (used to exercise ErrCursorExpired).
	incrementalDocs   int
	incrementalCursor sdk.Cursor
	incrementalErr    error

	// webhookDocs is the number of documents HandleWebhook emits.
	webhookErr  error
	webhookDocs int

	// seenWebhook captures the request HandleWebhook received, for round-trip
	// assertions. Populated server-side (in the connector), so it only reflects
	// the in-process run unless the test reads it from the same instance.
	seenWebhook *http.Request
}

func (f *fakeConnector) Spec() sdk.Spec {
	return sdk.Spec{
		ID:              fakeConnectorID,
		DisplayName:     "Fake Connector",
		AuthType:        sdk.AuthToken,
		ConfigSchema:    json.RawMessage(`{"type":"object"}`),
		SupportsWebhook: f.supportsWebhook,
	}
}

func (f *fakeConnector) Validate(_ context.Context, _ sdk.Config) error {
	return f.validateErr
}

// makeDoc builds a canonical Document for source id n under cfg, satisfying the
// connectortest.ValidateDocument invariants.
func makeDoc(cfg sdk.Config, prefix string, n int) *askerv1.Document {
	nativeID := fmt.Sprintf("%s-%d", prefix, n)
	return &askerv1.Document{
		TenantId:       string(cfg.Tenant.TenantID()),
		ConnectorId:    fakeConnectorID,
		SourceNativeId: nativeID,
		DocId:          sdk.DocID(fakeConnectorID, nativeID),
		Type:           askerv1.DocType_FILE,
		Title:          fmt.Sprintf("doc %d", n),
		BodyText:       fmt.Sprintf("body of %s doc %d", prefix, n),
		VersionEtag:    fmt.Sprintf("etag-%s-%d", prefix, n),
	}
}

func (f *fakeConnector) FullSync(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
	for n := 0; n < f.fullSyncDocs; n++ {
		if err := emit(ctx, makeDoc(cfg, "full", n)); err != nil {
			return "", err
		}
		if err := cfg.Checkpoint(ctx, sdk.Cursor(fmt.Sprintf("full-cp-%d", n))); err != nil {
			return "", err
		}
	}
	return f.fullSyncCursor, nil
}

func (f *fakeConnector) IncrementalSync(ctx context.Context, cfg sdk.Config, _ sdk.Cursor, emit sdk.Emit) (sdk.Cursor, error) {
	for n := 0; n < f.incrementalDocs; n++ {
		if err := emit(ctx, makeDoc(cfg, "inc", n)); err != nil {
			return "", err
		}
		if err := cfg.Checkpoint(ctx, sdk.Cursor(fmt.Sprintf("inc-cp-%d", n))); err != nil {
			return "", err
		}
	}
	if f.incrementalErr != nil {
		return "", f.incrementalErr
	}
	return f.incrementalCursor, nil
}

func (f *fakeConnector) HandleWebhook(ctx context.Context, cfg sdk.Config, r *http.Request, emit sdk.Emit) error {
	f.seenWebhook = r
	if f.webhookErr != nil {
		return f.webhookErr
	}
	for n := 0; n < f.webhookDocs; n++ {
		if err := emit(ctx, makeDoc(cfg, "hook", n)); err != nil {
			return err
		}
	}
	return nil
}

// testTenant is the tenant id every test pass runs under.
const testTenant = "tenant-parity"

// testConfig builds an sdk.Config for the parity tests with the given Checkpoint
// (use sdk.NopCheckpoint or a recording checkpoint). t-less so both server- and
// client-driven runs share one config shape.
func testConfig(checkpoint sdk.Checkpoint) (sdk.Config, error) {
	tc, err := tenancy.FromClaims(map[string]any{"tenant_id": testTenant, "sub": "user-a"})
	if err != nil {
		return sdk.Config{}, err
	}
	return sdk.Config{
		Tenant:     tc,
		InstanceID: "inst-1",
		ConfigJSON: []byte(`{"k":"v"}`),
		Token:      []byte("per-call-token"),
		Checkpoint: checkpoint,
	}, nil
}
