package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

// capturedRequest is one request as the Vespa stub observed it.
type capturedRequest struct {
	method      string
	path        string // escaped form, exactly as fed on the wire
	query       url.Values
	body        []byte
	contentType string
}

// vespaStub is an httptest server that records every request. respond, when
// non-nil, is called with the 1-based request count to script the response;
// otherwise the stub answers 200 with a minimal document/v1-shaped body.
type vespaStub struct {
	srv     *httptest.Server
	respond func(n int, rw http.ResponseWriter, r *http.Request)

	mu   sync.Mutex
	reqs []capturedRequest
}

func newVespaStub(t *testing.T, respond func(n int, rw http.ResponseWriter, r *http.Request)) *vespaStub {
	t.Helper()
	s := &vespaStub{respond: respond}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *vespaStub) serve(rw http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.reqs = append(s.reqs, capturedRequest{
		method:      r.Method,
		path:        r.URL.EscapedPath(),
		query:       r.URL.Query(),
		body:        body,
		contentType: r.Header.Get("Content-Type"),
	})
	n := len(s.reqs)
	s.mu.Unlock()
	if s.respond != nil {
		s.respond(n, rw, r)
		return
	}
	rw.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(rw, `{"pathId":"stub"}`)
}

func (s *vespaStub) requests() []capturedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]capturedRequest, len(s.reqs))
	copy(out, s.reqs)
	return out
}

// waitRequests polls until the stub has seen at least n requests or the
// timeout elapses, returning whatever arrived.
func (s *vespaStub) waitRequests(n int, timeout time.Duration) []capturedRequest {
	deadline := time.Now().Add(timeout)
	for {
		reqs := s.requests()
		if len(reqs) >= n || time.Now().After(deadline) {
			return reqs
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newTestWriter(t *testing.T, vespaURL string, dim int) *writer {
	t.Helper()
	w, err := newWriter(vespaURL, dim, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newWriter: %v", err)
	}
	return w
}

func tenantCtx(t *testing.T, tenant string) context.Context {
	t.Helper()
	tc, err := tenancy.FromHeaderValue(tenant)
	if err != nil {
		t.Fatalf("tenancy.FromHeaderValue(%q): %v", tenant, err)
	}
	return tenancy.WithContext(context.Background(), tc)
}

// richDoc is a fully populated enriched document: chunks with vectors (dim 4,
// exact binary fractions so JSON round-trips bit-for-bit), participants of
// all three shapes, metadata, timestamps, ACL, etag.
func richDoc() *askerv1.Document {
	return &askerv1.Document{
		TenantId:       "tenant-a",
		DocId:          "doc-rich-1",
		ConnectorId:    "gmail",
		SourceNativeId: "msg-42",
		Type:           askerv1.DocType_EMAIL,
		Title:          "Quarterly planning",
		BodyText:       "Full extracted body text for quarterly planning.",
		Chunks: []*askerv1.Chunk{
			{ChunkId: "doc-rich-1#0", Text: "First chunk text.", CharStart: 0, CharEnd: 17,
				Embedding: []float32{0.25, -0.5, 0.125, 1}},
			{ChunkId: "doc-rich-1#1", Text: "Second chunk text.", CharStart: 17, CharEnd: 35,
				Embedding: []float32{0.0625, 2, -0.25, 0.5}},
		},
		Metadata: map[string]string{
			"sender": "alice@example.com",
			"labels": "INBOX",
		},
		Participants: []*askerv1.Participant{
			{Name: "Alice Smith", Email: "alice@example.com", Role: "from"},
			{Handle: "U123BOB", Role: "to"},
			{Email: "carol@example.com", Role: "cc"},
		},
		Ts: &askerv1.Timestamps{
			Created:  timestamppb.New(time.Unix(1718000000, 0)),
			Modified: timestamppb.New(time.Unix(1718000600, 0)),
			Ingested: timestamppb.New(time.Unix(1718000700, 0)),
		},
		Acl:         &askerv1.AclInfo{AllowedPrincipals: []string{"user:alice@example.com", "user:bob@example.com"}},
		VersionEtag: "etag-1",
	}
}

// goldenRichFeed is the EXACT feed JSON the rich document must produce
// (vespa/README.md feed shape, EMBEDDING_DIM=4). Compared structurally.
const goldenRichFeed = `{
  "fields": {
    "doc_id": "doc-rich-1",
    "connector_id": "gmail",
    "type": "EMAIL",
    "title": "Quarterly planning",
    "body": "Full extracted body text for quarterly planning.",
    "chunks": ["First chunk text.", "Second chunk text."],
    "embedding": {
      "blocks": {
        "0": [0.25, -0.5, 0.125, 1],
        "1": [0.0625, 2, -0.25, 0.5]
      }
    },
    "participants": [
      "Alice Smith <alice@example.com>",
      "U123BOB",
      "carol@example.com"
    ],
    "metadata_json": "{\"labels\":\"INBOX\",\"sender\":\"alice@example.com\"}",
    "created_at": 1718000000,
    "modified_at": 1718000600,
    "version_etag": "etag-1",
    "acl": ["user:alice@example.com", "user:bob@example.com"]
  }
}`

// asJSONValue parses raw JSON into the generic any-form so two encodings can
// be compared structurally regardless of key order or whitespace.
func asJSONValue(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return v
}

// feedFields unmarshals a captured PUT body and returns its "fields" object.
func feedFields(t *testing.T, body []byte) map[string]any {
	t.Helper()
	doc, ok := asJSONValue(t, body).(map[string]any)
	if !ok {
		t.Fatalf("feed body is not a JSON object: %s", body)
	}
	fields, ok := doc["fields"].(map[string]any)
	if !ok {
		t.Fatalf("feed body has no fields object: %s", body)
	}
	return fields
}

func TestHandleFeedsRichDocumentGolden(t *testing.T) {
	t.Parallel()
	stub := newVespaStub(t, nil)
	w := newTestWriter(t, stub.srv.URL, 4)

	if err := w.Handle(tenantCtx(t, "tenant-a"), richDoc()); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	reqs := stub.requests()
	if len(reqs) != 1 {
		t.Fatalf("vespa saw %d requests, want 1", len(reqs))
	}
	req := reqs[0]
	if req.method != http.MethodPut {
		t.Errorf("method = %s, want PUT", req.method)
	}
	if want := "/document/v1/asker/doc/group/tenant-a/doc-rich-1"; req.path != want {
		t.Errorf("path = %q, want %q", req.path, want)
	}
	if req.contentType != "application/json" {
		t.Errorf("content type = %q, want application/json", req.contentType)
	}
	got := asJSONValue(t, req.body)
	want := asJSONValue(t, []byte(goldenRichFeed))
	if !reflect.DeepEqual(got, want) {
		gotPretty, _ := json.MarshalIndent(got, "", "  ")
		wantPretty, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("feed JSON mismatch\ngot:\n%s\nwant:\n%s", gotPretty, wantPretty)
	}
}

func TestHandleTombstoneDeletes(t *testing.T) {
	t.Parallel()
	stub := newVespaStub(t, nil)
	w := newTestWriter(t, stub.srv.URL, 4)

	doc := richDoc() // tombstones may still carry stale fields; only the delete matters
	doc.Tombstone = &askerv1.Tombstone{
		Deleted:   true,
		DeletedAt: timestamppb.New(time.Unix(1718001000, 0)),
	}
	if err := w.Handle(tenantCtx(t, "tenant-a"), doc); err != nil {
		t.Fatalf("Handle(tombstone): %v", err)
	}

	reqs := stub.requests()
	if len(reqs) != 1 {
		t.Fatalf("vespa saw %d requests, want 1", len(reqs))
	}
	req := reqs[0]
	if req.method != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", req.method)
	}
	if want := "/document/v1/asker/doc/group/tenant-a/doc-rich-1"; req.path != want {
		t.Errorf("path = %q, want %q", req.path, want)
	}
	if len(req.body) != 0 {
		t.Errorf("DELETE carried a body: %s", req.body)
	}
}

func TestHandleTombstoneAbsentDocumentStill200(t *testing.T) {
	t.Parallel()
	// Vespa returns 200 for deletes of absent documents; the handler must
	// treat that as success (idempotent replay).
	stub := newVespaStub(t, func(_ int, rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(rw, `{"pathId":"/document/v1/asker/doc/group/tenant-a/doc-gone","id":"id:asker:doc:g=tenant-a:doc-gone"}`)
	})
	w := newTestWriter(t, stub.srv.URL, 4)

	doc := &askerv1.Document{
		TenantId:  "tenant-a",
		DocId:     "doc-gone",
		Tombstone: &askerv1.Tombstone{Deleted: true},
	}
	if err := w.Handle(tenantCtx(t, "tenant-a"), doc); err != nil {
		t.Fatalf("Handle(tombstone for absent doc): %v", err)
	}
	if got := len(stub.requests()); got != 1 {
		t.Errorf("vespa saw %d requests, want 1", got)
	}
}

func TestHandleRejectsDimensionMismatch(t *testing.T) {
	t.Parallel()
	stub := newVespaStub(t, nil)
	w := newTestWriter(t, stub.srv.URL, 4)

	doc := richDoc()
	doc.Chunks[1].Embedding = []float32{0.1, 0.2, 0.3} // 3 dims, EMBEDDING_DIM is 4

	err := w.Handle(tenantCtx(t, "tenant-a"), doc)
	if err == nil {
		t.Fatal("Handle with wrong-dimension vector: want error, got nil")
	}
	if !strings.Contains(err.Error(), "EMBEDDING_DIM") {
		t.Errorf("error = %v, want mention of EMBEDDING_DIM", err)
	}
	// A misconfigured dimension must never reach Vespa at all.
	if got := len(stub.requests()); got != 0 {
		t.Errorf("vespa saw %d requests, want 0", got)
	}
}

func TestHandleOmitsEmbeddingWithoutFullVectors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*askerv1.Document)
	}{
		{"no vectors at all", func(d *askerv1.Document) {
			for _, c := range d.Chunks {
				c.Embedding = nil
			}
		}},
		{"partial vectors", func(d *askerv1.Document) {
			d.Chunks[1].Embedding = nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := newVespaStub(t, nil)
			w := newTestWriter(t, stub.srv.URL, 4)
			doc := richDoc()
			tc.mutate(doc)
			if err := w.Handle(tenantCtx(t, "tenant-a"), doc); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			reqs := stub.requests()
			if len(reqs) != 1 {
				t.Fatalf("vespa saw %d requests, want 1", len(reqs))
			}
			fields := feedFields(t, reqs[0].body)
			if _, present := fields["embedding"]; present {
				t.Errorf("embedding field present, want omitted: %s", reqs[0].body)
			}
			if chunks, ok := fields["chunks"].([]any); !ok || len(chunks) != 2 {
				t.Errorf("chunks = %v, want the 2 chunk texts", fields["chunks"])
			}
		})
	}
}

func TestHandle4xxIsPermanentNoRetry(t *testing.T) {
	t.Parallel()
	stub := newVespaStub(t, func(_ int, rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(rw, `{"message":"field type mismatch"}`)
	})
	w := newTestWriter(t, stub.srv.URL, 4)

	err := w.Handle(tenantCtx(t, "tenant-a"), richDoc())
	if err == nil {
		t.Fatal("Handle with 400 response: want error, got nil")
	}
	if !errors.Is(err, errVespaPermanent) {
		t.Errorf("error = %v, want errors.Is(err, errVespaPermanent)", err)
	}
	if !strings.Contains(err.Error(), "field type mismatch") {
		t.Errorf("error %v does not carry the vespa response body", err)
	}
	// Permanent: exactly one request, no in-handler retry.
	if got := len(stub.requests()); got != 1 {
		t.Errorf("vespa saw %d requests, want 1 (no retry on 4xx)", got)
	}
}

func TestHandle5xxRetriedOnceThenSucceeds(t *testing.T) {
	t.Parallel()
	stub := newVespaStub(t, func(n int, rw http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			rw.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(rw, "overloaded")
			return
		}
		rw.WriteHeader(http.StatusOK)
	})
	w := newTestWriter(t, stub.srv.URL, 4)

	if err := w.Handle(tenantCtx(t, "tenant-a"), richDoc()); err != nil {
		t.Fatalf("Handle with 503-then-200: %v", err)
	}
	if got := len(stub.requests()); got != 2 {
		t.Errorf("vespa saw %d requests, want 2 (single retry on 5xx)", got)
	}
}

func TestHandle5xxExhaustsSingleRetry(t *testing.T) {
	t.Parallel()
	stub := newVespaStub(t, func(_ int, rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(rw, "still broken")
	})
	w := newTestWriter(t, stub.srv.URL, 4)

	err := w.Handle(tenantCtx(t, "tenant-a"), richDoc())
	if err == nil {
		t.Fatal("Handle with persistent 500: want error, got nil")
	}
	if errors.Is(err, errVespaPermanent) {
		t.Errorf("5xx error should not be permanent: %v", err)
	}
	if !strings.Contains(err.Error(), "status 500") {
		t.Errorf("error = %v, want status 500 in message", err)
	}
	if got := len(stub.requests()); got != 2 {
		t.Errorf("vespa saw %d requests, want exactly 2 (one retry, then give up to kafkautil)", got)
	}
}

func TestHandleTimeoutRetriedOnce(t *testing.T) {
	t.Parallel()
	stub := newVespaStub(t, func(n int, rw http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			time.Sleep(400 * time.Millisecond) // beyond the test timeout below
		}
		rw.WriteHeader(http.StatusOK)
	})
	w := newTestWriter(t, stub.srv.URL, 4)
	w.timeout = 100 * time.Millisecond

	if err := w.Handle(tenantCtx(t, "tenant-a"), richDoc()); err != nil {
		t.Fatalf("Handle with timeout-then-200: %v", err)
	}
	if got := len(stub.waitRequests(2, 2*time.Second)); got < 2 {
		t.Errorf("vespa saw %d requests, want 2 (retry after timeout)", got)
	}
}

func TestHandlePathEscapesTenantAndDocID(t *testing.T) {
	t.Parallel()
	stub := newVespaStub(t, nil)
	w := newTestWriter(t, stub.srv.URL, 4)

	const tenant = "ten.ant_1-X"
	doc := &askerv1.Document{
		TenantId:    tenant,
		DocId:       "a/b c#d?e%f",
		ConnectorId: "upload",
		Type:        askerv1.DocType_FILE,
		Title:       "weird id",
	}
	if err := w.Handle(tenantCtx(t, tenant), doc); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	reqs := stub.requests()
	if len(reqs) != 1 {
		t.Fatalf("vespa saw %d requests, want 1", len(reqs))
	}
	want := "/document/v1/asker/doc/group/ten.ant_1-X/a%2Fb%20c%23d%3Fe%25f"
	if reqs[0].path != want {
		t.Errorf("escaped path = %q, want %q", reqs[0].path, want)
	}
	// Nothing from the doc id may leak into the query string.
	if len(reqs[0].query) != 0 {
		t.Errorf("query = %v, want empty", reqs[0].query)
	}
}

func TestHandleDocTypeEnumNames(t *testing.T) {
	t.Parallel()
	cases := map[askerv1.DocType]string{
		askerv1.DocType_DOC_TYPE_UNSPECIFIED: "DOC_TYPE_UNSPECIFIED",
		askerv1.DocType_EMAIL:                "EMAIL",
		askerv1.DocType_CHAT_MESSAGE:         "CHAT_MESSAGE",
		askerv1.DocType_FILE:                 "FILE",
		askerv1.DocType_CALENDAR_EVENT:       "CALENDAR_EVENT",
		askerv1.DocType_WIKI_PAGE:            "WIKI_PAGE",
		askerv1.DocType_TICKET:               "TICKET",
		askerv1.DocType_IMAGE:                "IMAGE",
		askerv1.DocType_VIDEO:                "VIDEO",
		askerv1.DocType_AUDIO:                "AUDIO",
	}
	stub := newVespaStub(t, nil)
	w := newTestWriter(t, stub.srv.URL, 4)
	n := 0
	for typ, name := range cases {
		doc := &askerv1.Document{TenantId: "tenant-a", DocId: "doc-type-test", Type: typ}
		if err := w.Handle(tenantCtx(t, "tenant-a"), doc); err != nil {
			t.Fatalf("Handle(type=%v): %v", typ, err)
		}
		n++
		reqs := stub.requests()
		if len(reqs) != n {
			t.Fatalf("vespa saw %d requests, want %d", len(reqs), n)
		}
		fields := feedFields(t, reqs[n-1].body)
		if got := fields["type"]; got != name {
			t.Errorf("type %v fed as %q, want %q", typ, got, name)
		}
	}
}

func TestHandleFailsClosedWithoutTenant(t *testing.T) {
	t.Parallel()
	stub := newVespaStub(t, nil)
	w := newTestWriter(t, stub.srv.URL, 4)

	// No tenancy on the context at all.
	if err := w.Handle(context.Background(), richDoc()); !errors.Is(err, tenancy.ErrNoTenant) {
		t.Errorf("Handle without tenant: got %v, want ErrNoTenant", err)
	}
	// Context tenant differs from the document tenant.
	err := w.Handle(tenantCtx(t, "tenant-b"), richDoc())
	if err == nil || !strings.Contains(err.Error(), "does not match document tenant") {
		t.Errorf("Handle with mismatched tenant: got %v, want tenant mismatch error", err)
	}
	// Document without a doc id cannot form a document path.
	bad := richDoc()
	bad.DocId = ""
	if err := w.Handle(tenantCtx(t, "tenant-a"), bad); err == nil {
		t.Error("Handle without doc_id: want error, got nil")
	}
	if got := len(stub.requests()); got != 0 {
		t.Errorf("vespa saw %d requests, want 0 (fail closed)", got)
	}
}

func TestBuildFieldsDefaults(t *testing.T) {
	t.Parallel()
	w := newTestWriter(t, "http://vespa.invalid", 4)

	// Minimal document: no chunks, no metadata, no participants, no ts.
	fields, err := w.buildFields(&askerv1.Document{
		TenantId:    "tenant-a",
		DocId:       "doc-min",
		ConnectorId: "upload",
		Type:        askerv1.DocType_FILE,
		Title:       "bare",
	})
	if err != nil {
		t.Fatalf("buildFields: %v", err)
	}
	if fields.MetadataJSON != "{}" {
		t.Errorf("metadata_json = %q, want {}", fields.MetadataJSON)
	}
	if fields.Embedding != nil {
		t.Errorf("embedding = %v, want nil", fields.Embedding)
	}
	if len(fields.Chunks) != 0 || len(fields.Participants) != 0 || len(fields.ACL) != 0 {
		t.Errorf("chunks/participants/acl = %v/%v/%v, want all empty",
			fields.Chunks, fields.Participants, fields.ACL)
	}
	if fields.CreatedAt != 0 || fields.ModifiedAt != 0 {
		t.Errorf("created/modified = %d/%d, want 0/0 for absent ts", fields.CreatedAt, fields.ModifiedAt)
	}

	// Modified falls back to created when absent.
	fields, err = w.buildFields(&askerv1.Document{
		TenantId: "tenant-a",
		DocId:    "doc-created-only",
		Ts:       &askerv1.Timestamps{Created: timestamppb.New(time.Unix(1718000000, 0))},
	})
	if err != nil {
		t.Fatalf("buildFields: %v", err)
	}
	if fields.CreatedAt != 1718000000 || fields.ModifiedAt != 1718000000 {
		t.Errorf("created/modified = %d/%d, want 1718000000/1718000000",
			fields.CreatedAt, fields.ModifiedAt)
	}
}

func TestParticipantStrings(t *testing.T) {
	t.Parallel()
	got := participantStrings([]*askerv1.Participant{
		{Name: "Alice Smith", Email: "alice@example.com"},
		{Name: "Dave", Handle: "U42", Email: "dave@example.com"}, // name wins over handle
		{Handle: "U123BOB"},
		{Email: "carol@example.com"},
		{Handle: "U99", Email: "erin@example.com"}, // handle is the name fallback
		{}, // entirely empty: dropped
	})
	want := []string{
		"Alice Smith <alice@example.com>",
		"Dave <dave@example.com>",
		"U123BOB",
		"carol@example.com",
		"U99 <erin@example.com>",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("participantStrings = %v, want %v", got, want)
	}
	if participantStrings(nil) != nil {
		t.Error("participantStrings(nil) should be nil so the field is omitted")
	}
}

func TestNewWriterValidation(t *testing.T) {
	t.Parallel()
	if _, err := newWriter("", 4, nil); err == nil {
		t.Error("newWriter with empty url: want error, got nil")
	}
	if _, err := newWriter("http://vespa:8080", 0, nil); err == nil {
		t.Error("newWriter with dim 0: want error, got nil")
	}
	w, err := newWriter("http://vespa:8080/", 384, nil)
	if err != nil {
		t.Fatalf("newWriter: %v", err)
	}
	// Trailing slash is trimmed so the document path never doubles it.
	if got, want := w.documentURL("t1", "d1"), "http://vespa:8080/document/v1/asker/doc/group/t1/d1"; got != want {
		t.Errorf("documentURL = %q, want %q", got, want)
	}
}
