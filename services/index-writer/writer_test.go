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

// testCLIPDim is the CLIP_DIM used across writer tests. It is deliberately
// different from the EMBEDDING_DIM (4) the tests pass so the two vector spaces
// are distinguishable by length, mirroring the dev config (384 vs 512).
const testCLIPDim = 6

func newTestWriter(t *testing.T, vespaURL string, dim int) *writer {
	t.Helper()
	w, err := newWriter(vespaURL, dim, testCLIPDim, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
    "chunk_starts_ms": [0, 0],
    "chunk_ends_ms": [0, 0],
    "chunk_modalities": ["text", "text"],
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

// feedFields unmarshals a captured feed POST body and returns its "fields"
// object.
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
	// document/v1 full puts are POST; PUT would be parsed as a partial update.
	if req.method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.method)
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
	if _, err := newWriter("", 4, 6, nil); err == nil {
		t.Error("newWriter with empty url: want error, got nil")
	}
	if _, err := newWriter("http://vespa:8080", 0, 6, nil); err == nil {
		t.Error("newWriter with dim 0: want error, got nil")
	}
	if _, err := newWriter("http://vespa:8080", 4, 0, nil); err == nil {
		t.Error("newWriter with clip dim 0: want error, got nil")
	}
	if _, err := newWriter("http://vespa:8080", 4, 4, nil); err == nil {
		t.Error("newWriter with clip dim == embedding dim: want error, got nil")
	}
	w, err := newWriter("http://vespa:8080/", 384, 512, nil)
	if err != nil {
		t.Fatalf("newWriter: %v", err)
	}
	// Trailing slash is trimmed so the document path never doubles it.
	if got, want := w.documentURL("t1", "d1"), "http://vespa:8080/document/v1/asker/doc/group/t1/d1"; got != want {
		t.Errorf("documentURL = %q, want %q", got, want)
	}
}

// --- M3 media feed (ADR-013) ----------------------------------------------

// audioDoc is an AUDIO document whose ASR transcript produced two time-anchored
// chunks. Their vectors are bge-m3 (EMBEDDING_DIM=4) — speech transcript text
// lives in the unified text space, not the CLIP space — so they feed the
// `embedding` tensor with parallel chunk_starts_ms/ends_ms/modalities arrays.
func audioDoc() *askerv1.Document {
	return &askerv1.Document{
		TenantId:    "tenant-a",
		DocId:       "doc-audio-1",
		ConnectorId: "upload",
		Type:        askerv1.DocType_AUDIO,
		Title:       "standup recording",
		Chunks: []*askerv1.Chunk{
			{ChunkId: "doc-audio-1#0", Text: "Good morning team.", StartMs: 0, EndMs: 1500,
				Modality: "asr", Embedding: []float32{0.5, -0.25, 0.125, 1}},
			{ChunkId: "doc-audio-1#1", Text: "Let us review the roadmap.", StartMs: 1500, EndMs: 4200,
				Modality: "asr", Embedding: []float32{-0.5, 0.0625, 2, 0.25}},
		},
		Ts:    &askerv1.Timestamps{Created: timestamppb.New(time.Unix(1718000000, 0))},
		Media: &askerv1.MediaInfo{DurationMs: 4200, TranscriptLang: "en"},
	}
}

const goldenAudioFeed = `{
  "fields": {
    "doc_id": "doc-audio-1",
    "connector_id": "upload",
    "type": "AUDIO",
    "title": "standup recording",
    "body": "",
    "chunks": ["Good morning team.", "Let us review the roadmap."],
    "embedding": {
      "blocks": {
        "0": [0.5, -0.25, 0.125, 1],
        "1": [-0.5, 0.0625, 2, 0.25]
      }
    },
    "chunk_starts_ms": [0, 1500],
    "chunk_ends_ms": [1500, 4200],
    "chunk_modalities": ["asr", "asr"],
    "metadata_json": "{}",
    "created_at": 1718000000,
    "modified_at": 1718000000,
    "version_etag": "",
    "media_duration_ms": 4200,
    "transcript_lang": "en"
  }
}`

func TestHandleFeedsAudioDocumentGolden(t *testing.T) {
	t.Parallel()
	stub := newVespaStub(t, nil)
	w := newTestWriter(t, stub.srv.URL, 4)

	if err := w.Handle(tenantCtx(t, "tenant-a"), audioDoc()); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	reqs := stub.requests()
	if len(reqs) != 1 {
		t.Fatalf("vespa saw %d requests, want 1", len(reqs))
	}
	got := asJSONValue(t, reqs[0].body)
	want := asJSONValue(t, []byte(goldenAudioFeed))
	if !reflect.DeepEqual(got, want) {
		gotPretty, _ := json.MarshalIndent(got, "", "  ")
		wantPretty, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("audio feed JSON mismatch\ngot:\n%s\nwant:\n%s", gotPretty, wantPretty)
	}
}

// videoDoc is a VIDEO document carrying BOTH an ASR transcript chunk (bge-m3,
// EMBEDDING_DIM=4, modality "asr") and a keyframe chunk whose CLIP image vector
// (CLIP_DIM=6, modality "caption") feeds clip_embedding. Media dims + thumbnail
// poster come from MediaInfo. This exercises the two-space routing on one doc.
func videoDoc() *askerv1.Document {
	return &askerv1.Document{
		TenantId:    "tenant-a",
		DocId:       "doc-video-1",
		ConnectorId: "upload",
		Type:        askerv1.DocType_VIDEO,
		Title:       "demo clip",
		Chunks: []*askerv1.Chunk{
			{ChunkId: "doc-video-1#0", Text: "Welcome to the demo.", StartMs: 0, EndMs: 2000,
				Modality: "asr", Embedding: []float32{0.25, -0.5, 0.125, 1}},
			// Keyframe chunk: CLIP image vector, no transcript text.
			{ChunkId: "doc-video-1#1", StartMs: 5000, EndMs: 5000, Modality: "caption",
				Embedding: []float32{0.5, -0.25, 0.125, 1, -0.0625, 2}},
		},
		Ts: &askerv1.Timestamps{Created: timestamppb.New(time.Unix(1718000000, 0))},
		Media: &askerv1.MediaInfo{
			DurationMs:     12000,
			Width:          1920,
			Height:         1080,
			Thumbnail:      &askerv1.BlobRef{Bucket: "media", Key: "thumb/doc-video-1.jpg"},
			TranscriptLang: "en",
			Keyframes: []*askerv1.Keyframe{
				{TsMs: 5000, ChunkId: "doc-video-1#1"},
			},
		},
	}
}

// goldenVideoFeed: the ASR chunk's bge-m3 vector lands in `embedding` (block
// "0"); the keyframe chunk's CLIP vector lands in `clip_embedding` (block "1");
// `embedding` is NOT omitted even though chunk 1 lacks a bge-m3 vector, because
// chunk 1 is a CLIP-only chunk and excluded from the bge-m3 completeness gate.
const goldenVideoFeed = `{
  "fields": {
    "doc_id": "doc-video-1",
    "connector_id": "upload",
    "type": "VIDEO",
    "title": "demo clip",
    "body": "",
    "chunks": ["Welcome to the demo.", ""],
    "embedding": {
      "blocks": {
        "0": [0.25, -0.5, 0.125, 1]
      }
    },
    "clip_embedding": {
      "blocks": {
        "1": [0.5, -0.25, 0.125, 1, -0.0625, 2]
      }
    },
    "chunk_starts_ms": [0, 5000],
    "chunk_ends_ms": [2000, 5000],
    "chunk_modalities": ["asr", "caption"],
    "metadata_json": "{}",
    "created_at": 1718000000,
    "modified_at": 1718000000,
    "version_etag": "",
    "media_duration_ms": 12000,
    "media_width": 1920,
    "media_height": 1080,
    "thumbnail_key": "thumb/doc-video-1.jpg",
    "transcript_lang": "en"
  }
}`

func TestHandleFeedsVideoDocumentGolden(t *testing.T) {
	t.Parallel()
	stub := newVespaStub(t, nil)
	w := newTestWriter(t, stub.srv.URL, 4)

	if err := w.Handle(tenantCtx(t, "tenant-a"), videoDoc()); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	reqs := stub.requests()
	if len(reqs) != 1 {
		t.Fatalf("vespa saw %d requests, want 1", len(reqs))
	}
	got := asJSONValue(t, reqs[0].body)
	want := asJSONValue(t, []byte(goldenVideoFeed))
	if !reflect.DeepEqual(got, want) {
		gotPretty, _ := json.MarshalIndent(got, "", "  ")
		wantPretty, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("video feed JSON mismatch\ngot:\n%s\nwant:\n%s", gotPretty, wantPretty)
	}
}

// imageDoc is an IMAGE document: an OCR text chunk (bge-m3) plus a CLIP image
// chunk, with width/height/thumbnail in MediaInfo and no duration.
func imageDoc() *askerv1.Document {
	return &askerv1.Document{
		TenantId:    "tenant-a",
		DocId:       "doc-image-1",
		ConnectorId: "upload",
		Type:        askerv1.DocType_IMAGE,
		Title:       "scanned receipt",
		Chunks: []*askerv1.Chunk{
			{ChunkId: "doc-image-1#0", Text: "TOTAL $42.00", Modality: "ocr",
				Embedding: []float32{0.125, 0.25, -0.5, 1}},
			{ChunkId: "doc-image-1#1", Modality: "caption",
				Embedding: []float32{1, 0.5, 0.25, 0.125, 0.0625, -2}},
		},
		Media: &askerv1.MediaInfo{
			Width:     800,
			Height:    600,
			Thumbnail: &askerv1.BlobRef{Bucket: "media", Key: "thumb/doc-image-1.png"},
		},
	}
}

func TestHandleImageDocClipBlockAndDims(t *testing.T) {
	t.Parallel()
	stub := newVespaStub(t, nil)
	w := newTestWriter(t, stub.srv.URL, 4)

	if err := w.Handle(tenantCtx(t, "tenant-a"), imageDoc()); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	fields := feedFields(t, stub.requests()[0].body)

	// bge-m3 OCR vector -> embedding block "0"; CLIP vector -> clip_embedding "1".
	emb, ok := fields["embedding"].(map[string]any)
	if !ok {
		t.Fatalf("embedding missing/not an object: %v", fields["embedding"])
	}
	if blocks, _ := emb["blocks"].(map[string]any); len(blocks) != 1 || blocks["0"] == nil {
		t.Errorf("embedding blocks = %v, want only block \"0\"", emb["blocks"])
	}
	clip, ok := fields["clip_embedding"].(map[string]any)
	if !ok {
		t.Fatalf("clip_embedding missing/not an object: %v", fields["clip_embedding"])
	}
	blocks, _ := clip["blocks"].(map[string]any)
	if len(blocks) != 1 || blocks["1"] == nil {
		t.Fatalf("clip_embedding blocks = %v, want only block \"1\"", clip["blocks"])
	}
	if vec, _ := blocks["1"].([]any); len(vec) != testCLIPDim {
		t.Errorf("clip vector length = %d, want CLIP_DIM=%d", len(vec), testCLIPDim)
	}
	if got := fields["media_width"]; got != float64(800) {
		t.Errorf("media_width = %v, want 800", got)
	}
	if got := fields["media_height"]; got != float64(600) {
		t.Errorf("media_height = %v, want 600", got)
	}
	if got := fields["thumbnail_key"]; got != "thumb/doc-image-1.png" {
		t.Errorf("thumbnail_key = %v, want thumb/doc-image-1.png", got)
	}
	// No audio: media_duration_ms is zero and omitted.
	if _, present := fields["media_duration_ms"]; present {
		t.Errorf("media_duration_ms present for an image, want omitted")
	}
	if got, _ := fields["chunk_modalities"].([]any); len(got) != 2 || got[0] != "ocr" || got[1] != "caption" {
		t.Errorf("chunk_modalities = %v, want [ocr caption]", fields["chunk_modalities"])
	}
}

// A CLIP-dim vector is accepted (routes to clip_embedding); a vector whose
// length is NEITHER EMBEDDING_DIM nor CLIP_DIM is rejected before any feed, so
// a dimension mismatch dead-letters instead of silently indexing.
func TestHandleClipDimRoutingAndRejection(t *testing.T) {
	t.Parallel()

	t.Run("clip dim accepted", func(t *testing.T) {
		t.Parallel()
		stub := newVespaStub(t, nil)
		w := newTestWriter(t, stub.srv.URL, 4)
		doc := imageDoc()
		if err := w.Handle(tenantCtx(t, "tenant-a"), doc); err != nil {
			t.Fatalf("Handle with a CLIP_DIM vector: %v", err)
		}
		if got := len(stub.requests()); got != 1 {
			t.Fatalf("vespa saw %d requests, want 1", got)
		}
	})

	t.Run("neither dim rejected", func(t *testing.T) {
		t.Parallel()
		stub := newVespaStub(t, nil)
		w := newTestWriter(t, stub.srv.URL, 4) // EMBEDDING_DIM=4, CLIP_DIM=6
		doc := imageDoc()
		doc.Chunks[1].Embedding = []float32{0.1, 0.2, 0.3, 0.4, 0.5} // 5: neither 4 nor 6

		err := w.Handle(tenantCtx(t, "tenant-a"), doc)
		if err == nil {
			t.Fatal("Handle with a non-dim vector: want error, got nil")
		}
		if !strings.Contains(err.Error(), "EMBEDDING_DIM=4") || !strings.Contains(err.Error(), "CLIP_DIM=6") {
			t.Errorf("error = %v, want mention of both EMBEDDING_DIM=4 and CLIP_DIM=6", err)
		}
		// A misconfigured dimension must never reach Vespa.
		if got := len(stub.requests()); got != 0 {
			t.Errorf("vespa saw %d requests, want 0", got)
		}
	})
}

// A text document carries no media: clip_embedding and every media_* field are
// omitted, while the parallel chunk arrays still carry 0/0/"text".
func TestHandleTextDocOmitsMediaFields(t *testing.T) {
	t.Parallel()
	stub := newVespaStub(t, nil)
	w := newTestWriter(t, stub.srv.URL, 4)

	if err := w.Handle(tenantCtx(t, "tenant-a"), richDoc()); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	fields := feedFields(t, stub.requests()[0].body)
	for _, k := range []string{"clip_embedding", "media_duration_ms", "media_width", "media_height", "thumbnail_key", "transcript_lang"} {
		if _, present := fields[k]; present {
			t.Errorf("field %q present for a text doc, want omitted", k)
		}
	}
	if got, _ := fields["chunk_modalities"].([]any); len(got) != 2 || got[0] != "text" || got[1] != "text" {
		t.Errorf("chunk_modalities = %v, want [text text]", fields["chunk_modalities"])
	}
}
