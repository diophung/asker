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

// seenStore is the dedupe store. Seen reports whether key was already recorded
// (a read); MarkSeen records key with ttl AFTER the document is durably
// produced. The ordering matters: recording only after a successful produce
// keeps ingest at-least-once. Were the key recorded before the produce, a
// crash in between would leave a "seen" key for a document that never reached
// docs.chunked, and the redelivered record would be dropped as a false
// duplicate (silent data loss). The production implementation is Redis
// (constructed in main); tests use an in-memory fake.
type seenStore interface {
	Seen(ctx context.Context, key string) (bool, error)
	MarkSeen(ctx context.Context, key string, ttl time.Duration) error
	Close() error
}

// handler implements the ingest stage: docs.raw -> normalize -> dedupe ->
// chunk -> docs.chunked. It is invoked sequentially by kafkautil.Consumer.Run
// (records are processed in order within a fetch), and because docs.raw is
// keyed by tenant_id every record for a given (doc_id, version_etag) lands on
// one partition handled by one consumer — so there is no concurrent processing
// of the same dedupe key and the read-then-mark sequence needs no locking.
type handler struct {
	producer docProducer
	seen     seenStore
	log      *slog.Logger
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

	// Dedupe is a READ before the produce (skip documents already pushed to
	// docs.chunked) and a WRITE only after a successful produce (below). On a
	// read error, fail OPEN: duplicates that slip through are absorbed by the
	// idempotent (doc_id, version_etag) upsert downstream; the pipeline must
	// never block on Redis.
	key := seenKeyPrefix + doc.GetDocId() + ":" + doc.GetVersionEtag()
	switch seen, err := h.seen.Seen(ctx, key); {
	case err != nil:
		h.log.Warn("dedupe store unavailable; processing anyway",
			"doc_id", doc.GetDocId(), "error", err)
	case seen:
		h.log.Debug("duplicate document skipped",
			"doc_id", doc.GetDocId(), "version_etag", doc.GetVersionEtag())
		return nil
	}

	chunks, truncated := buildChunks(doc)
	if truncated {
		h.log.Warn("chunk cap exceeded; truncating",
			"doc_id", doc.GetDocId(), "cap", maxChunksPerDoc, "body_bytes", len(doc.GetBodyText()))
	}
	doc.Chunks = chunks

	if err := h.producer.ProduceDocument(ctx, kafkautil.TopicDocsChunked, doc); err != nil {
		return fmt.Errorf("produce chunked %s: %w", doc.GetDocId(), err)
	}

	// Record the key only now that the document is durably on docs.chunked. A
	// crash before this point redelivers the record (uncommitted offset) and
	// re-produces — a harmless duplicate, never a drop. A failure to record is
	// non-fatal for the same reason: the document IS produced; at worst a later
	// redelivery re-produces it.
	if err := h.seen.MarkSeen(ctx, key, seenTTL); err != nil {
		h.log.Warn("failed to record dedupe key; duplicates may flow",
			"doc_id", doc.GetDocId(), "error", err)
	}
	h.log.Debug("document chunked",
		"doc_id", doc.GetDocId(), "chunks", len(chunks), "body_bytes", len(doc.GetBodyText()))
	return nil
}
