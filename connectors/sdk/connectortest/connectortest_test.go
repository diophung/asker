package connectortest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

// recordTB wraps a real *testing.T but intercepts Errorf so tests can assert
// that the helpers in this package flag (or pass) specific inputs without
// failing the enclosing test.
type recordTB struct {
	testing.TB
	mu   sync.Mutex
	msgs []string
}

func newRecordTB(t *testing.T) *recordTB { return &recordTB{TB: t} }

func (r *recordTB) Helper() {}

func (r *recordTB) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

func (r *recordTB) failed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.msgs) > 0
}

func (r *recordTB) contains(substr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.msgs {
		if strings.Contains(m, substr) {
			return true
		}
	}
	return false
}

func testConfig(t *testing.T, tenant string) sdk.Config {
	t.Helper()
	tc, err := tenancy.FromClaims(map[string]any{"tenant_id": tenant, "sub": "user-1"})
	if err != nil {
		t.Fatalf("tenancy.FromClaims(%q): %v", tenant, err)
	}
	return sdk.Config{
		Tenant:     tc,
		InstanceID: "inst-1",
		ConfigJSON: []byte(`{}`),
		Checkpoint: sdk.NopCheckpoint,
	}
}

func validDoc(tenant string) *askerv1.Document {
	return &askerv1.Document{
		TenantId:       tenant,
		ConnectorId:    "notes",
		SourceNativeId: "n-1",
		DocId:          sdk.DocID("notes", "n-1"),
		Type:           askerv1.DocType_FILE,
		Title:          "title",
		BodyText:       "body",
		VersionEtag:    "v1",
		Ts:             &askerv1.Timestamps{Created: timestamppb.Now()},
	}
}

func TestEmitRecorderCollectsInOrder(t *testing.T) {
	var rec EmitRecorder
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		doc := validDoc("tenant-a")
		doc.Title = fmt.Sprintf("doc-%d", i)
		if err := rec.Emit(ctx, doc); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	}
	docs := rec.Docs()
	if len(docs) != 3 {
		t.Fatalf("Docs() returned %d documents, want 3", len(docs))
	}
	for i, doc := range docs {
		if want := fmt.Sprintf("doc-%d", i); doc.GetTitle() != want {
			t.Errorf("Docs()[%d].Title = %q, want %q (emission order)", i, doc.GetTitle(), want)
		}
	}
}

func TestEmitRecorderNilDocument(t *testing.T) {
	var rec EmitRecorder
	if err := rec.Emit(context.Background(), nil); err == nil {
		t.Error("Emit(nil) returned nil error")
	}
	if got := len(rec.Docs()); got != 0 {
		t.Errorf("recorder holds %d documents after nil emit, want 0", got)
	}
}

// TestEmitRecorderIsolation: the recorder must hold its own clone, and
// Docs() must return an independent snapshot slice.
func TestEmitRecorderIsolation(t *testing.T) {
	var rec EmitRecorder
	doc := validDoc("tenant-a")
	if err := rec.Emit(context.Background(), doc); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	doc.Title = "mutated-after-emit"
	if got := rec.Docs()[0].GetTitle(); got != "title" {
		t.Errorf("recorded document mutated through the caller's pointer: title = %q", got)
	}

	snap := rec.Docs()
	snap[0] = nil
	if rec.Docs()[0] == nil {
		t.Error("mutating the Docs() snapshot slice changed the recorder's state")
	}
}

// TestEmitRecorderConcurrent hammers Emit and Docs from many goroutines;
// run with -race to prove thread safety.
func TestEmitRecorderConcurrent(t *testing.T) {
	var rec EmitRecorder
	ctx := context.Background()
	const goroutines, perGoroutine = 16, 50

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				doc := validDoc("tenant-a")
				doc.SourceNativeId = fmt.Sprintf("g%d-i%d", g, i)
				if err := rec.Emit(ctx, doc); err != nil {
					t.Errorf("Emit: %v", err)
				}
				rec.Docs() // concurrent reads must be safe too
			}
		}()
	}
	wg.Wait()

	docs := rec.Docs()
	if len(docs) != goroutines*perGoroutine {
		t.Fatalf("recorded %d documents, want %d", len(docs), goroutines*perGoroutine)
	}
	seen := make(map[string]bool, len(docs))
	for _, doc := range docs {
		if doc == nil {
			t.Fatal("recorded a nil document")
		}
		if seen[doc.GetSourceNativeId()] {
			t.Errorf("document %q recorded twice", doc.GetSourceNativeId())
		}
		seen[doc.GetSourceNativeId()] = true
	}
}

func TestValidateDocumentAcceptsValidDocs(t *testing.T) {
	cfg := testConfig(t, "tenant-a")

	rt := newRecordTB(t)
	ValidateDocument(rt, cfg, validDoc("tenant-a"))
	if rt.failed() {
		t.Errorf("valid document flagged: %v", rt.msgs)
	}

	// A well-formed tombstone is valid too.
	tomb := validDoc("tenant-a")
	tomb.BodyText = ""
	tomb.Chunks = nil
	tomb.Tombstone = &askerv1.Tombstone{Deleted: true, DeletedAt: timestamppb.Now()}
	rt = newRecordTB(t)
	ValidateDocument(rt, cfg, tomb)
	if rt.failed() {
		t.Errorf("valid tombstone flagged: %v", rt.msgs)
	}

	// tombstone present but deleted=false places no body restrictions.
	alive := validDoc("tenant-a")
	alive.Tombstone = &askerv1.Tombstone{Deleted: false}
	rt = newRecordTB(t)
	ValidateDocument(rt, cfg, alive)
	if rt.failed() {
		t.Errorf("non-deleted tombstone field flagged: %v", rt.msgs)
	}
}

func TestValidateDocumentFlagsViolations(t *testing.T) {
	cfg := testConfig(t, "tenant-a")

	tests := []struct {
		name    string
		mutate  func(d *askerv1.Document)
		wantMsg string
	}{
		{
			name:    "empty tenant",
			mutate:  func(d *askerv1.Document) { d.TenantId = "" },
			wantMsg: "tenant_id is empty",
		},
		{
			name:    "wrong tenant",
			mutate:  func(d *askerv1.Document) { d.TenantId = "tenant-b" },
			wantMsg: "never emit for another tenant",
		},
		{
			name:    "empty connector_id",
			mutate:  func(d *askerv1.Document) { d.ConnectorId = "" },
			wantMsg: "connector_id is empty",
		},
		{
			name:    "empty source_native_id",
			mutate:  func(d *askerv1.Document) { d.SourceNativeId = "" },
			wantMsg: "source_native_id is empty",
		},
		{
			name:    "doc_id not derived via DocID",
			mutate:  func(d *askerv1.Document) { d.DocId = "freehand-id" },
			wantMsg: "want sdk.DocID",
		},
		{
			name:    "empty version_etag",
			mutate:  func(d *askerv1.Document) { d.VersionEtag = "" },
			wantMsg: "version_etag is empty",
		},
		{
			name:    "ts.ingested set by connector",
			mutate:  func(d *askerv1.Document) { d.Ts.Ingested = timestamppb.Now() },
			wantMsg: "ts.ingested is set",
		},
		{
			name: "tombstone with body_text",
			mutate: func(d *askerv1.Document) {
				d.Tombstone = &askerv1.Tombstone{Deleted: true}
			},
			wantMsg: "tombstone carries body_text",
		},
		{
			name: "tombstone with chunks",
			mutate: func(d *askerv1.Document) {
				d.BodyText = ""
				d.Chunks = []*askerv1.Chunk{{ChunkId: "c1", Text: "left behind"}}
				d.Tombstone = &askerv1.Tombstone{Deleted: true}
			},
			wantMsg: "chunks",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := validDoc("tenant-a")
			tt.mutate(doc)
			rt := newRecordTB(t)
			ValidateDocument(rt, cfg, doc)
			if !rt.failed() {
				t.Fatalf("violation not flagged")
			}
			if !rt.contains(tt.wantMsg) {
				t.Errorf("no error mentions %q; got %v", tt.wantMsg, rt.msgs)
			}
		})
	}
}

func TestValidateDocumentNilDocument(t *testing.T) {
	rt := newRecordTB(t)
	ValidateDocument(rt, testConfig(t, "tenant-a"), nil)
	if !rt.contains("nil document") {
		t.Errorf("nil document not flagged; got %v", rt.msgs)
	}
}

func TestValidateDocumentZeroTenantConfig(t *testing.T) {
	rt := newRecordTB(t)
	ValidateDocument(rt, sdk.Config{}, validDoc("tenant-a"))
	if !rt.contains("zero value") {
		t.Errorf("zero-value cfg.Tenant not flagged; got %v", rt.msgs)
	}
}

// specConnector implements sdk.Connector with a settable Spec for
// RunSpecChecks tests.
type specConnector struct{ spec sdk.Spec }

func (s specConnector) Spec() sdk.Spec                             { return s.spec }
func (s specConnector) Validate(context.Context, sdk.Config) error { return nil }
func (s specConnector) FullSync(context.Context, sdk.Config, sdk.Emit) (sdk.Cursor, error) {
	return "", nil
}
func (s specConnector) IncrementalSync(_ context.Context, _ sdk.Config, cur sdk.Cursor, _ sdk.Emit) (sdk.Cursor, error) {
	return cur, nil
}
func (s specConnector) HandleWebhook(context.Context, sdk.Config, *http.Request, sdk.Emit) error {
	return sdk.ErrWebhookUnsupported
}

func goodSpec() sdk.Spec {
	return sdk.Spec{
		ID:           "fake-gmail",
		DisplayName:  "Fake Gmail",
		AuthType:     sdk.AuthOAuth2,
		ConfigSchema: json.RawMessage(`{"type":"object"}`),
	}
}

func TestRunSpecChecksAcceptsGoodSpec(t *testing.T) {
	rt := newRecordTB(t)
	RunSpecChecks(rt, specConnector{spec: goodSpec()})
	if rt.failed() {
		t.Errorf("good spec flagged: %v", rt.msgs)
	}
}

func TestRunSpecChecksFlagsBadSpecs(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(s *sdk.Spec)
		wantMsg string
	}{
		{
			name:    "empty ID",
			mutate:  func(s *sdk.Spec) { s.ID = "" },
			wantMsg: "ID is empty",
		},
		{
			name:    "uppercase ID",
			mutate:  func(s *sdk.Spec) { s.ID = "Gmail" },
			wantMsg: "must match",
		},
		{
			name:    "ID with colon",
			mutate:  func(s *sdk.Spec) { s.ID = "gmail:v2" },
			wantMsg: "must match",
		},
		{
			name:    "ID with underscore",
			mutate:  func(s *sdk.Spec) { s.ID = "fake_gmail" },
			wantMsg: "must match",
		},
		{
			name:    "empty DisplayName",
			mutate:  func(s *sdk.Spec) { s.DisplayName = "" },
			wantMsg: "DisplayName is empty",
		},
		{
			name:    "nil ConfigSchema",
			mutate:  func(s *sdk.Spec) { s.ConfigSchema = nil },
			wantMsg: "not valid JSON",
		},
		{
			name:    "malformed ConfigSchema",
			mutate:  func(s *sdk.Spec) { s.ConfigSchema = json.RawMessage(`{"type":`) },
			wantMsg: "not valid JSON",
		},
		{
			name:    "unknown AuthType",
			mutate:  func(s *sdk.Spec) { s.AuthType = sdk.AuthType(99) },
			wantMsg: "AuthType",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := goodSpec()
			tt.mutate(&spec)
			rt := newRecordTB(t)
			RunSpecChecks(rt, specConnector{spec: spec})
			if !rt.failed() {
				t.Fatal("bad spec not flagged")
			}
			if !rt.contains(tt.wantMsg) {
				t.Errorf("no error mentions %q; got %v", tt.wantMsg, rt.msgs)
			}
		})
	}
}

func TestRunSpecChecksNilConnector(t *testing.T) {
	rt := newRecordTB(t)
	RunSpecChecks(rt, nil)
	if !rt.contains("nil connector") {
		t.Errorf("nil connector not flagged; got %v", rt.msgs)
	}
}
