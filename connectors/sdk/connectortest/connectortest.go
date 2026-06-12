// Package connectortest contains test helpers for connector authors: an
// in-memory Emit recorder and assertions for the canonical-Document and Spec
// invariants pinned in the sdk package documentation. CI contract tests for
// every connector are built on these helpers, so a connector whose tests use
// ValidateDocument and RunSpecChecks ships with the invariants already
// proven.
package connectortest

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// EmitRecorder collects documents passed to its Emit method. It is safe for
// concurrent use and its zero value is ready to use:
//
//	var rec connectortest.EmitRecorder
//	cur, err := conn.FullSync(ctx, cfg, rec.Emit)
//	for _, doc := range rec.Docs() { connectortest.ValidateDocument(t, cfg, doc) }
type EmitRecorder struct {
	mu   sync.Mutex
	docs []*askerv1.Document
}

// Emit records a deep copy of doc (so later mutation by the connector cannot
// corrupt the recording) and never fails for a non-nil document. Its
// signature matches sdk.Emit; pass rec.Emit wherever an Emit is needed.
func (r *EmitRecorder) Emit(_ context.Context, doc *askerv1.Document) error {
	if doc == nil {
		return errors.New("connectortest: Emit called with nil document")
	}
	clone := proto.CloneOf(doc)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.docs = append(r.docs, clone)
	return nil
}

// Docs returns a snapshot of the recorded documents in emission order. The
// returned slice is a copy; the documents are the recorder's own clones.
func (r *EmitRecorder) Docs() []*askerv1.Document {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*askerv1.Document, len(r.docs))
	copy(out, r.docs)
	return out
}

// ValidateDocument asserts the canonical-Document invariants every connector
// must uphold for documents it emits under cfg (see the sdk package
// documentation):
//
//   - tenant_id is set and equals cfg.Tenant.TenantID()
//   - connector_id and source_native_id are set, and doc_id equals
//     sdk.DocID(connector_id, source_native_id)
//   - version_etag is set
//   - ts.ingested is unset (the hub stamps it)
//   - a tombstone (tombstone.deleted=true) carries no body: empty body_text
//     and no chunks
//
// Failures are reported via tb.Errorf so one call surfaces every violated
// invariant at once.
func ValidateDocument(tb testing.TB, cfg sdk.Config, doc *askerv1.Document) {
	tb.Helper()
	if doc == nil {
		tb.Errorf("connectortest: ValidateDocument called with nil document")
		return
	}

	tenant := string(cfg.Tenant.TenantID())
	if tenant == "" {
		tb.Errorf("connectortest: cfg.Tenant is the zero value; build it with tenancy.FromClaims")
	}
	switch got := doc.GetTenantId(); {
	case got == "":
		tb.Errorf("document %q: tenant_id is empty; it must equal cfg.Tenant.TenantID() (%q)", doc.GetDocId(), tenant)
	case got != tenant:
		tb.Errorf("document %q: tenant_id = %q, want cfg tenant %q — a connector must never emit for another tenant", doc.GetDocId(), got, tenant)
	}

	if doc.GetConnectorId() == "" {
		tb.Errorf("document %q: connector_id is empty", doc.GetDocId())
	}
	if doc.GetSourceNativeId() == "" {
		tb.Errorf("document %q: source_native_id is empty", doc.GetDocId())
	}
	if want := sdk.DocID(doc.GetConnectorId(), doc.GetSourceNativeId()); doc.GetDocId() != want {
		tb.Errorf("document: doc_id = %q, want sdk.DocID(%q, %q) = %q",
			doc.GetDocId(), doc.GetConnectorId(), doc.GetSourceNativeId(), want)
	}

	if doc.GetVersionEtag() == "" {
		tb.Errorf("document %q: version_etag is empty; set one so upserts are idempotent", doc.GetDocId())
	}
	if doc.GetTs().GetIngested() != nil {
		tb.Errorf("document %q: ts.ingested is set; the connector hub stamps it — leave it unset", doc.GetDocId())
	}

	if doc.GetTombstone().GetDeleted() {
		if doc.GetBodyText() != "" {
			tb.Errorf("document %q: tombstone carries body_text; tombstones must have no body", doc.GetDocId())
		}
		if n := len(doc.GetChunks()); n != 0 {
			tb.Errorf("document %q: tombstone carries %d chunks; tombstones must have no body", doc.GetDocId(), n)
		}
	}
}

// specIDPattern is the allowed shape of Spec.ID. Lowercase alphanumerics and
// hyphens only — IDs are baked into doc_ids forever and double as the DocID
// prefix, so ":" (the DocID separator) must be impossible.
var specIDPattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// RunSpecChecks asserts the sanity of a connector's Spec: a non-empty ID
// matching ^[a-z0-9-]+$, a non-empty DisplayName, ConfigSchema that is valid
// JSON (publish {"type":"object"} when the connector takes no config), and a
// known AuthType. Call it from every connector's test suite.
func RunSpecChecks(tb testing.TB, c sdk.Connector) {
	tb.Helper()
	if c == nil {
		tb.Errorf("connectortest: RunSpecChecks called with nil connector")
		return
	}
	spec := c.Spec()

	if spec.ID == "" {
		tb.Errorf("spec: ID is empty")
	} else if !specIDPattern.MatchString(spec.ID) {
		tb.Errorf("spec: ID %q must match %s", spec.ID, specIDPattern)
	}
	if spec.DisplayName == "" {
		tb.Errorf("spec %q: DisplayName is empty", spec.ID)
	}
	if !json.Valid(spec.ConfigSchema) {
		tb.Errorf("spec %q: ConfigSchema is not valid JSON (got %d bytes); connectors with no config should publish {\"type\":\"object\"}", spec.ID, len(spec.ConfigSchema))
	}
	switch spec.AuthType {
	case sdk.AuthNone, sdk.AuthOAuth2, sdk.AuthToken:
	default:
		tb.Errorf("spec %q: AuthType %d is not one of AuthNone, AuthOAuth2, AuthToken", spec.ID, int(spec.AuthType))
	}
}
