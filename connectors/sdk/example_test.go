package sdk_test

// This file is documentation-by-example: a tiny but complete connector for a
// fake "notes" source, exercised end to end the way the connector hub (and a
// connector author's own test suite) would. The M2 "build a connector in
// under a day" tutorial walks through this same shape.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

// note is a record in the fake source.
type note struct {
	id, title, body, etag string
	created               time.Time
}

// notesConnector syncs the fake source. A real connector would hold an API
// client built from cfg.Token; this one holds the data directly.
type notesConnector struct {
	notes   []note   // current source contents
	deleted []string // source-native IDs deleted since the last cursor
}

const notesConnectorID = "notes"

func (notesConnector) Spec() sdk.Spec {
	return sdk.Spec{
		ID:          notesConnectorID,
		DisplayName: "Toy Notes",
		AuthType:    sdk.AuthToken,
		ConfigSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"folder": {"type": "string"}},
			"additionalProperties": false
		}`),
		SupportsWebhook: false,
	}
}

// Validate checks the config shape and that a credential is present. It must
// not emit documents and must not leak cfg.Token in errors.
func (notesConnector) Validate(_ context.Context, cfg sdk.Config) error {
	var parsed struct {
		Folder string `json:"folder"`
	}
	if err := json.Unmarshal(cfg.ConfigJSON, &parsed); err != nil {
		return fmt.Errorf("notes: invalid config: %w", err)
	}
	if len(cfg.Token) == 0 {
		return errors.New("notes: missing API token")
	}
	return nil
}

// document builds the canonical Document for one note, following the Emit
// contract: tenant from cfg, doc_id from sdk.DocID, version_etag set,
// ts.ingested left for the hub.
func (notesConnector) document(cfg sdk.Config, n note) *askerv1.Document {
	return &askerv1.Document{
		TenantId:       string(cfg.Tenant.TenantID()),
		ConnectorId:    notesConnectorID,
		SourceNativeId: n.id,
		DocId:          sdk.DocID(notesConnectorID, n.id),
		Type:           askerv1.DocType_FILE,
		Title:          n.title,
		BodyText:       n.body,
		Metadata:       map[string]string{"source": "toy-notes"},
		Ts:             &askerv1.Timestamps{Created: timestamppb.New(n.created)},
		VersionEtag:    n.etag,
	}
}

// FullSync emits every note, checkpointing after each one so an interrupted
// backfill resumes, and returns the cursor incremental sync continues from.
func (c notesConnector) FullSync(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
	for _, n := range c.notes {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := emit(ctx, c.document(cfg, n)); err != nil {
			return "", fmt.Errorf("notes: emit %s: %w", n.id, err)
		}
		if err := cfg.Checkpoint(ctx, sdk.Cursor("after:"+n.id)); err != nil {
			return "", fmt.Errorf("notes: checkpoint: %w", err)
		}
	}
	return sdk.Cursor(fmt.Sprintf("count:%d", len(c.notes))), nil
}

// IncrementalSync emits deletions observed since cur as tombstones — same
// doc_id rule, tombstone.deleted=true, and no body.
func (c notesConnector) IncrementalSync(ctx context.Context, cfg sdk.Config, cur sdk.Cursor, emit sdk.Emit) (sdk.Cursor, error) {
	for _, id := range c.deleted {
		doc := &askerv1.Document{
			TenantId:       string(cfg.Tenant.TenantID()),
			ConnectorId:    notesConnectorID,
			SourceNativeId: id,
			DocId:          sdk.DocID(notesConnectorID, id),
			Type:           askerv1.DocType_FILE,
			VersionEtag:    "deleted",
			Tombstone: &askerv1.Tombstone{
				Deleted:   true,
				DeletedAt: timestamppb.Now(),
			},
		}
		if err := emit(ctx, doc); err != nil {
			return "", fmt.Errorf("notes: emit tombstone %s: %w", id, err)
		}
	}
	return cur + ":caught-up", nil
}

// HandleWebhook: the toy source has no push path, so the hub will poll.
func (notesConnector) HandleWebhook(context.Context, sdk.Config, *http.Request, sdk.Emit) error {
	return sdk.ErrWebhookUnsupported
}

// testTenant builds a valid tenancy.Context the way the hub would — from
// verified claims, never from a bare string.
func testTenant(t *testing.T, tenant string) tenancy.Context {
	t.Helper()
	tc, err := tenancy.FromClaims(map[string]any{"tenant_id": tenant, "sub": "user-1"})
	if err != nil {
		t.Fatalf("tenancy.FromClaims(%q): %v", tenant, err)
	}
	return tc
}

// TestNotesConnectorEndToEnd drives the full Connector surface the way the
// hub does, asserting the Emit contract on everything that comes out.
func TestNotesConnectorEndToEnd(t *testing.T) {
	ctx := context.Background()
	conn := notesConnector{
		notes: []note{
			{id: "n-1", title: "groceries", body: "milk, eggs", etag: "v1", created: time.Unix(1700000000, 0)},
			{id: "n-2", title: "ideas", body: "build a search engine", etag: "v1", created: time.Unix(1700000100, 0)},
		},
		deleted: []string{"n-1"},
	}

	// Spec sanity — every connector test suite starts here.
	connectortest.RunSpecChecks(t, conn)

	// The hub registers connectors at startup and looks them up by spec ID.
	reg := sdk.NewRegistry()
	if err := reg.Register(conn); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, ok := reg.Get(notesConnectorID)
	if !ok {
		t.Fatal("registered connector not found by ID")
	}

	var checkpoints []sdk.Cursor
	cfg := sdk.Config{
		Tenant:     testTenant(t, "tenant-a"),
		InstanceID: "inst-1",
		ConfigJSON: []byte(`{"folder":"inbox"}`),
		Token:      []byte("test-token"),
		Checkpoint: func(_ context.Context, cur sdk.Cursor) error {
			checkpoints = append(checkpoints, cur)
			return nil
		},
	}

	if err := got.Validate(ctx, cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	badCfg := cfg
	badCfg.Token = nil
	if err := got.Validate(ctx, badCfg); err == nil {
		t.Error("Validate accepted a config with no token")
	}

	// Full sync: backfill everything.
	var rec connectortest.EmitRecorder
	cur, err := got.FullSync(ctx, cfg, rec.Emit)
	if err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if cur != "count:2" {
		t.Errorf("FullSync cursor = %q, want %q", cur, "count:2")
	}
	docs := rec.Docs()
	if len(docs) != 2 {
		t.Fatalf("FullSync emitted %d documents, want 2", len(docs))
	}
	for _, doc := range docs {
		connectortest.ValidateDocument(t, cfg, doc)
	}
	if docs[0].GetTitle() != "groceries" || docs[1].GetTitle() != "ideas" {
		t.Errorf("unexpected titles: %q, %q", docs[0].GetTitle(), docs[1].GetTitle())
	}
	if len(checkpoints) != 2 || checkpoints[1] != "after:n-2" {
		t.Errorf("checkpoints = %v, want one per note ending in after:n-2", checkpoints)
	}

	// Incremental sync: the deletion arrives as a tombstone.
	var incRec connectortest.EmitRecorder
	next, err := got.IncrementalSync(ctx, cfg, cur, incRec.Emit)
	if err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	if next == cur {
		t.Errorf("IncrementalSync did not advance the cursor: %q", next)
	}
	incDocs := incRec.Docs()
	if len(incDocs) != 1 {
		t.Fatalf("IncrementalSync emitted %d documents, want 1", len(incDocs))
	}
	tomb := incDocs[0]
	connectortest.ValidateDocument(t, cfg, tomb)
	if !tomb.GetTombstone().GetDeleted() {
		t.Error("expected a tombstone with deleted=true")
	}
	if want := sdk.DocID(notesConnectorID, "n-1"); tomb.GetDocId() != want {
		t.Errorf("tombstone doc_id = %q, want %q (same rule as the original document)", tomb.GetDocId(), want)
	}

	// No push path: the hub interprets ErrWebhookUnsupported as "poll".
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/webhooks/notes", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if err := got.HandleWebhook(ctx, cfg, req, rec.Emit); !errors.Is(err, sdk.ErrWebhookUnsupported) {
		t.Errorf("HandleWebhook error = %v, want ErrWebhookUnsupported", err)
	}
}

// TestNotesConnectorEmitFailure shows the error contract: an Emit failure is
// terminal for the sync pass and surfaces wrapped to the hub.
func TestNotesConnectorEmitFailure(t *testing.T) {
	conn := notesConnector{notes: []note{{id: "n-1", etag: "v1"}}}
	cfg := sdk.Config{
		Tenant:     testTenant(t, "tenant-a"),
		InstanceID: "inst-1",
		ConfigJSON: []byte(`{}`),
		Token:      []byte("tok"),
		Checkpoint: sdk.NopCheckpoint,
	}
	emitErr := errors.New("kafka is on fire")
	failingEmit := func(context.Context, *askerv1.Document) error { return emitErr }

	_, err := conn.FullSync(context.Background(), cfg, failingEmit)
	if !errors.Is(err, emitErr) {
		t.Errorf("FullSync error = %v, want wrapped %v", err, emitErr)
	}
}
