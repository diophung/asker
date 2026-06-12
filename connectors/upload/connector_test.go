package upload

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
	"github.com/asker/asker/platform/tenancy"
)

// testTenant builds a valid tenancy.Context for id.
func testTenant(t *testing.T, id string) tenancy.Context {
	t.Helper()
	tc, err := tenancy.FromClaims(map[string]any{"tenant_id": id})
	if err != nil {
		t.Fatalf("FromClaims(%q): %v", id, err)
	}
	return tc
}

func testConfig(t *testing.T) sdk.Config {
	t.Helper()
	return sdk.Config{
		Tenant:     testTenant(t, "tenant-a"),
		InstanceID: "inst-1",
		ConfigJSON: []byte(`{}`),
		Checkpoint: sdk.NopCheckpoint,
	}
}

func TestSpec(t *testing.T) {
	conn := New()
	connectortest.RunSpecChecks(t, conn)

	spec := conn.Spec()
	if spec.ID != "upload" {
		t.Errorf("Spec.ID = %q, want %q", spec.ID, "upload")
	}
	if spec.AuthType != sdk.AuthNone {
		t.Errorf("Spec.AuthType = %v, want AuthNone", spec.AuthType)
	}
	if spec.SupportsWebhook {
		t.Error("Spec.SupportsWebhook = true, want false")
	}
	if got := string(spec.ConfigSchema); got != `{"type":"object"}` {
		t.Errorf("Spec.ConfigSchema = %s, want {\"type\":\"object\"}", got)
	}
}

func TestRegister(t *testing.T) {
	reg := sdk.NewRegistry()
	if err := reg.Register(New()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := reg.Get("upload"); !ok {
		t.Fatal("registry does not know \"upload\" after Register")
	}
}

func TestValidateAcceptsAnything(t *testing.T) {
	if err := New().Validate(context.Background(), testConfig(t)); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestFullSyncIsNoop(t *testing.T) {
	var rec connectortest.EmitRecorder
	cur, err := New().FullSync(context.Background(), testConfig(t), rec.Emit)
	if err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if cur != "" {
		t.Errorf("FullSync cursor = %q, want empty", cur)
	}
	if docs := rec.Docs(); len(docs) != 0 {
		t.Errorf("FullSync emitted %d documents, want 0", len(docs))
	}
}

func TestIncrementalSyncReturnsCursorUnchanged(t *testing.T) {
	var rec connectortest.EmitRecorder
	for _, cur := range []sdk.Cursor{"", "opaque-position"} {
		got, err := New().IncrementalSync(context.Background(), testConfig(t), cur, rec.Emit)
		if err != nil {
			t.Fatalf("IncrementalSync(%q): %v", cur, err)
		}
		if got != cur {
			t.Errorf("IncrementalSync(%q) cursor = %q, want unchanged", cur, got)
		}
	}
	if docs := rec.Docs(); len(docs) != 0 {
		t.Errorf("IncrementalSync emitted %d documents, want 0", len(docs))
	}
}

func TestHandleWebhookUnsupported(t *testing.T) {
	var rec connectortest.EmitRecorder
	req := httptest.NewRequest("POST", "/webhooks/upload/inst-1", nil)
	err := New().HandleWebhook(context.Background(), testConfig(t), req, rec.Emit)
	if !errors.Is(err, sdk.ErrWebhookUnsupported) {
		t.Errorf("HandleWebhook err = %v, want sdk.ErrWebhookUnsupported", err)
	}
}
