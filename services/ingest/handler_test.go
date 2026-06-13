package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/platform/kafkautil"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// fakeProducer captures produced documents in memory.
type fakeProducer struct {
	mu     sync.Mutex
	err    error
	topics []string
	docs   []*askerv1.Document
}

func (f *fakeProducer) ProduceDocument(_ context.Context, topic string, doc *askerv1.Document) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.topics = append(f.topics, topic)
	f.docs = append(f.docs, proto.Clone(doc).(*askerv1.Document))
	return nil
}

func (f *fakeProducer) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeProducer) produced() []*askerv1.Document {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*askerv1.Document(nil), f.docs...)
}

// fakeSeen is the in-memory miniredis-like seenStore for tests.
type fakeSeen struct {
	mu      sync.Mutex
	err     error // returned by both Seen and MarkSeen
	markErr error // returned by MarkSeen only (Seen still succeeds)
	keys    map[string]time.Duration
}

func newFakeSeen() *fakeSeen { return &fakeSeen{keys: make(map[string]time.Duration)} }

func (f *fakeSeen) Seen(_ context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	_, ok := f.keys[key]
	return ok, nil
}

func (f *fakeSeen) MarkSeen(_ context.Context, key string, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if f.markErr != nil {
		return f.markErr
	}
	f.keys[key] = ttl
	return nil
}

func (f *fakeSeen) Close() error { return nil }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func rawDoc(docID, etag string) *askerv1.Document {
	return &askerv1.Document{
		TenantId:       "tenant-a",
		DocId:          docID,
		ConnectorId:    "gmail",
		SourceNativeId: "native-" + docID,
		Type:           askerv1.DocType_EMAIL,
		Title:          "Subject of " + docID,
		BodyText:       "Hello there.\n\nThis is the body of " + docID + ".",
		VersionEtag:    etag,
	}
}

func TestHandleChunksAndProduces(t *testing.T) {
	t.Parallel()
	prod, seen := &fakeProducer{}, newFakeSeen()
	h := newHandler(prod, seen, discardLogger())

	doc := rawDoc("doc-1", "etag-1")
	if err := h.Handle(context.Background(), doc); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	docs := prod.produced()
	if len(docs) != 1 {
		t.Fatalf("produced %d docs, want 1", len(docs))
	}
	if prod.topics[0] != kafkautil.TopicDocsChunked {
		t.Errorf("produced to %q, want %q", prod.topics[0], kafkautil.TopicDocsChunked)
	}
	out := docs[0]
	if len(out.GetChunks()) == 0 {
		t.Fatal("produced document has no chunks")
	}
	if got := out.GetChunks()[0].GetChunkId(); got != "doc-1#0" {
		t.Errorf("chunk id = %q, want %q", got, "doc-1#0")
	}
	ttl, ok := seen.keys[seenKeyPrefix+"doc-1:etag-1"]
	if !ok {
		t.Fatalf("dedupe key not recorded; keys: %v", seen.keys)
	}
	if ttl != seenTTL {
		t.Errorf("dedupe TTL = %v, want %v", ttl, seenTTL)
	}
}

func TestHandleTombstonePassesThroughUntouched(t *testing.T) {
	t.Parallel()
	prod, seen := &fakeProducer{}, newFakeSeen()
	h := newHandler(prod, seen, discardLogger())

	// Deliberately messy fields: a tombstone must NOT be normalized,
	// deduped, or chunked — it flows through byte-identical.
	doc := &askerv1.Document{
		TenantId:    "tenant-a",
		DocId:       "doc-del",
		ConnectorId: "gmail",
		Title:       "  messy   title\r\n",
		BodyText:    "",
		Tombstone:   &askerv1.Tombstone{Deleted: true, DeletedAt: timestamppb.New(time.Unix(1700000000, 0))},
	}
	want := proto.Clone(doc).(*askerv1.Document)

	if err := h.Handle(context.Background(), doc); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	docs := prod.produced()
	if len(docs) != 1 {
		t.Fatalf("produced %d docs, want 1", len(docs))
	}
	if !proto.Equal(docs[0], want) {
		t.Errorf("tombstone modified in flight:\n got %v\nwant %v", docs[0], want)
	}
	if len(seen.keys) != 0 {
		t.Errorf("tombstone hit the dedupe store: %v", seen.keys)
	}
}

func TestHandleNonDeletedTombstoneFieldIsProcessed(t *testing.T) {
	t.Parallel()
	prod, seen := &fakeProducer{}, newFakeSeen()
	h := newHandler(prod, seen, discardLogger())

	doc := rawDoc("doc-2", "etag-2")
	doc.Tombstone = &askerv1.Tombstone{Deleted: false} // present but not deleted
	if err := h.Handle(context.Background(), doc); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	docs := prod.produced()
	if len(docs) != 1 || len(docs[0].GetChunks()) == 0 {
		t.Error("document with deleted=false tombstone was not chunked")
	}
}

func TestHandleDuplicateSkipped(t *testing.T) {
	t.Parallel()
	prod, seen := &fakeProducer{}, newFakeSeen()
	h := newHandler(prod, seen, discardLogger())

	if err := h.Handle(context.Background(), rawDoc("doc-1", "etag-1")); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	// Replay of the same (doc_id, version_etag): skipped, returns nil so the
	// consumer commits.
	if err := h.Handle(context.Background(), rawDoc("doc-1", "etag-1")); err != nil {
		t.Fatalf("duplicate Handle: %v", err)
	}
	if got := len(prod.produced()); got != 1 {
		t.Errorf("produced %d docs, want 1 (duplicate must be skipped)", got)
	}

	// Same doc with a NEW etag is a content change, not a duplicate.
	if err := h.Handle(context.Background(), rawDoc("doc-1", "etag-2")); err != nil {
		t.Fatalf("new-etag Handle: %v", err)
	}
	if got := len(prod.produced()); got != 2 {
		t.Errorf("produced %d docs, want 2 (new etag must flow)", got)
	}
}

func TestHandleRedisDownFailsOpen(t *testing.T) {
	t.Parallel()
	prod := &fakeProducer{}
	seen := newFakeSeen()
	seen.err = errors.New("connection refused")
	h := newHandler(prod, seen, discardLogger())

	for i := 0; i < 2; i++ {
		if err := h.Handle(context.Background(), rawDoc("doc-1", "etag-1")); err != nil {
			t.Fatalf("Handle %d with redis down: %v", i, err)
		}
	}
	// Availability first: both flow through (downstream upserts are
	// idempotent), nothing is lost.
	if got := len(prod.produced()); got != 2 {
		t.Errorf("produced %d docs, want 2 (fail open when redis is down)", got)
	}
}

// TestHandleRetryAfterProduceFailure: record-after-produce means a failed
// produce leaves NO dedupe key, so the redelivered record re-produces instead
// of being dropped as a false duplicate.
func TestHandleRetryAfterProduceFailure(t *testing.T) {
	t.Parallel()
	prod, seen := &fakeProducer{}, newFakeSeen()
	h := newHandler(prod, seen, discardLogger())

	prod.setErr(errors.New("kafka unavailable"))
	err := h.Handle(context.Background(), rawDoc("doc-1", "etag-1"))
	if err == nil || !strings.Contains(err.Error(), "kafka unavailable") {
		t.Fatalf("Handle with failing producer: got %v, want produce error", err)
	}
	if _, marked := seen.keys[seenKeyPrefix+"doc-1:etag-1"]; marked {
		t.Fatal("dedupe key recorded despite a failed produce (record-after-produce violated)")
	}

	// The broker recovers; the consumer retries the same record.
	prod.setErr(nil)
	if err := h.Handle(context.Background(), rawDoc("doc-1", "etag-1")); err != nil {
		t.Fatalf("retry Handle: %v", err)
	}
	if got := len(prod.produced()); got != 1 {
		t.Fatalf("produced %d docs, want 1 (retry must proceed despite no key)", got)
	}

	// Now the key is recorded; a genuine replay of the same record is skipped.
	if err := h.Handle(context.Background(), rawDoc("doc-1", "etag-1")); err != nil {
		t.Fatalf("post-success duplicate Handle: %v", err)
	}
	if got := len(prod.produced()); got != 1 {
		t.Errorf("produced %d docs, want still 1 (recorded key suppresses replay)", got)
	}
}

// TestHandleCrashBetweenProduceAndMarkDoesNotDrop is the regression for the
// at-least-once blocker: a crash after a successful produce but before the
// dedupe key is recorded (simulated by a fresh handler over the same seen
// store with the key absent) must re-produce on redelivery, never drop. With
// record-after-produce the key is simply absent, so the redelivered record
// flows again — the idempotent downstream upsert collapses the duplicate.
func TestHandleCrashBetweenProduceAndMarkDoesNotDrop(t *testing.T) {
	t.Parallel()
	seen := newFakeSeen()

	// First delivery: produce succeeds, then the process "crashes" before
	// MarkSeen — model that by failing only the MarkSeen call.
	prod1 := &fakeProducer{}
	seen.markErr = errors.New("crash before mark")
	h1 := newHandler(prod1, seen, discardLogger())
	if err := h1.Handle(context.Background(), rawDoc("doc-1", "etag-1")); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	if got := len(prod1.produced()); got != 1 {
		t.Fatalf("first delivery produced %d, want 1", got)
	}
	if _, marked := seen.keys[seenKeyPrefix+"doc-1:etag-1"]; marked {
		t.Fatal("key recorded despite the simulated crash before MarkSeen")
	}

	// Redelivery after restart: a brand-new handler, same seen store, key
	// still absent. The document MUST be re-produced, not dropped.
	seen.markErr = nil
	prod2 := &fakeProducer{}
	h2 := newHandler(prod2, seen, discardLogger())
	if err := h2.Handle(context.Background(), rawDoc("doc-1", "etag-1")); err != nil {
		t.Fatalf("redelivery Handle: %v", err)
	}
	if got := len(prod2.produced()); got != 1 {
		t.Fatalf("redelivery produced %d docs, want 1 (document must not be dropped)", got)
	}
}

func TestHandleEmptyBodyStillFlows(t *testing.T) {
	t.Parallel()
	prod, seen := &fakeProducer{}, newFakeSeen()
	h := newHandler(prod, seen, discardLogger())

	doc := rawDoc("doc-title-only", "")
	doc.BodyText = ""
	if err := h.Handle(context.Background(), doc); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	docs := prod.produced()
	if len(docs) != 1 {
		t.Fatalf("produced %d docs, want 1 (title-only docs are legal)", len(docs))
	}
	if len(docs[0].GetChunks()) != 0 {
		t.Errorf("title-only doc has %d chunks, want 0", len(docs[0].GetChunks()))
	}
	if docs[0].GetVersionEtag() == "" {
		t.Error("empty version_etag was not derived")
	}
}

func TestHandleChunkCapTruncates(t *testing.T) {
	t.Parallel()
	prod, seen := &fakeProducer{}, newFakeSeen()
	h := newHandler(prod, seen, discardLogger())

	doc := rawDoc("doc-huge", "etag-huge")
	doc.Type = askerv1.DocType_FILE
	doc.BodyText = strings.TrimSpace(strings.Repeat("word ", 30000)) // ~150 KB
	if err := h.Handle(context.Background(), doc); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	docs := prod.produced()
	if len(docs) != 1 {
		t.Fatalf("produced %d docs, want 1", len(docs))
	}
	if got := len(docs[0].GetChunks()); got != maxChunksPerDoc {
		t.Errorf("produced doc has %d chunks, want capped at %d", got, maxChunksPerDoc)
	}
}
