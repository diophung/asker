package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/asker/asker/platform/kafkautil"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

const (
	// seenKeyPrefix namespaces dedupe keys in Redis.
	seenKeyPrefix = "ingest:seen:"
	// seenTTL bounds how long a (doc_id, version_etag) replay is suppressed.
	seenTTL = 48 * time.Hour
)

// docProducer is the slice of kafkautil.Producer the handler needs; tests
// substitute an in-memory fake.
type docProducer interface {
	ProduceDocument(ctx context.Context, topic string, doc *askerv1.Document) error
}

// seenStore is the dedupe store. SetNX atomically records key with ttl when
// absent and reports whether THIS call set it (false = already seen). The
// production implementation is Redis (constructed in main); tests use an
// in-memory fake.
type seenStore interface {
	SetNX(ctx context.Context, key string, ttl time.Duration) (bool, error)
	Close() error
}

// handler implements the ingest stage: docs.raw -> normalize -> dedupe ->
// chunk -> docs.chunked. It is invoked sequentially by kafkautil.Consumer.Run
// (records are processed in order within a fetch), so its mutable state needs
// no locking.
type handler struct {
	producer docProducer
	seen     seenStore
	log      *slog.Logger

	// pendingKey is the dedupe key most recently SETNX'd whose produce has
	// not yet succeeded. SETNX happens before the produce, so when the
	// produce fails and kafkautil retries the same record, the retry sees
	// "already seen" for a document that never reached docs.chunked; matching
	// against pendingKey lets the retry proceed instead of dropping the
	// document. Cleared on successful produce.
	pendingKey string
}

func newHandler(producer docProducer, seen seenStore, log *slog.Logger) *handler {
	return &handler{producer: producer, seen: seen, log: log.With("component", "ingest.handler")}
}

// Handle processes one document from docs.raw. The ctx carries the
// tenancy.Context reconstructed by the consumer; the producer re-validates it
// against doc.TenantId (tenancy chokepoint). Returning an error triggers
// kafkautil's retry / dead-letter policy.
func (h *handler) Handle(ctx context.Context, doc *askerv1.Document) error {
	// Tombstones pass through untouched: no normalization, no dedupe, no
	// chunks. The index writer turns them into Vespa deletes.
	if doc.GetTombstone().GetDeleted() {
		if err := h.producer.ProduceDocument(ctx, kafkautil.TopicDocsChunked, doc); err != nil {
			return fmt.Errorf("produce tombstone %s: %w", doc.GetDocId(), err)
		}
		h.log.Info("tombstone passed through", "doc_id", doc.GetDocId())
		return nil
	}

	normalizeDocument(doc)

	key := seenKeyPrefix + doc.GetDocId() + ":" + doc.GetVersionEtag()
	fresh, err := h.seen.SetNX(ctx, key, seenTTL)
	switch {
	case err != nil:
		// Availability first: Redis down means duplicates may flow, which
		// downstream idempotent upserts absorb. Never block the pipeline.
		h.log.Warn("dedupe store unavailable; processing anyway",
			"doc_id", doc.GetDocId(), "error", err)
	case !fresh && key != h.pendingKey:
		h.log.Debug("duplicate document skipped",
			"doc_id", doc.GetDocId(), "version_etag", doc.GetVersionEtag())
		return nil
	}
	h.pendingKey = key

	chunks, truncated := buildChunks(doc)
	if truncated {
		h.log.Warn("chunk cap exceeded; truncating",
			"doc_id", doc.GetDocId(), "cap", maxChunksPerDoc, "body_bytes", len(doc.GetBodyText()))
	}
	doc.Chunks = chunks

	if err := h.producer.ProduceDocument(ctx, kafkautil.TopicDocsChunked, doc); err != nil {
		return fmt.Errorf("produce chunked %s: %w", doc.GetDocId(), err)
	}
	h.pendingKey = ""
	h.log.Debug("document chunked",
		"doc_id", doc.GetDocId(), "chunks", len(chunks), "body_bytes", len(doc.GetBodyText()))
	return nil
}
