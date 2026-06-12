package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

// errVespaPermanent marks a 4xx response from Vespa: retrying the same feed
// cannot succeed. The handler still returns it (kafkautil exhausts its 3
// attempts and quarantines the record to docs.deadletter), but it never
// triggers the in-handler retry.
var errVespaPermanent = errors.New("vespa rejected the request (4xx, permanent)")

// feedTimeout bounds each individual Vespa HTTP request.
const feedTimeout = 10 * time.Second

// writer turns canonical Documents from docs.enriched into Vespa document/v1
// operations: tombstones become DELETEs, everything else a full-fields PUT
// (plain PUT upserts — ?create=true semantics; see vespa/README.md).
type writer struct {
	vespaURL string // base URL, no trailing slash
	dim      int    // EMBEDDING_DIM; every chunk vector must have exactly this length
	timeout  time.Duration
	client   *http.Client
	log      *slog.Logger
}

func newWriter(vespaURL string, dim int, logger *slog.Logger) (*writer, error) {
	vespaURL = strings.TrimRight(vespaURL, "/")
	if vespaURL == "" {
		return nil, errors.New("index-writer: vespa url must not be empty")
	}
	if dim <= 0 {
		return nil, fmt.Errorf("index-writer: embedding dim must be positive, got %d", dim)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &writer{
		vespaURL: vespaURL,
		dim:      dim,
		timeout:  feedTimeout,
		client:   &http.Client{},
		log:      logger.With("component", "index-writer.writer"),
	}, nil
}

// Handle is the kafkautil.Handler for docs.enriched. Errors it returns are
// retried by the consumer (3 attempts) and then dead-lettered.
func (w *writer) Handle(ctx context.Context, doc *askerv1.Document) error {
	// Fail closed: the consumer installs the tenancy.Context from the record
	// header, but this writer must never build a document path without one.
	tc, err := tenancy.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("index-writer: refusing to index without tenant: %w", err)
	}
	if string(tc.TenantID()) != doc.GetTenantId() {
		return fmt.Errorf("index-writer: context tenant %q does not match document tenant %q",
			tc.TenantID(), doc.GetTenantId())
	}
	if doc.GetDocId() == "" {
		return errors.New("index-writer: document without doc_id")
	}

	docURL := w.documentURL(doc.GetTenantId(), doc.GetDocId())

	if doc.GetTombstone().GetDeleted() {
		// Vespa's document/v1 DELETE is idempotent: 200 even when the
		// document is already absent, which is exactly what at-least-once
		// redelivery needs.
		if err := w.send(ctx, http.MethodDelete, docURL, nil, doc.GetDocId()); err != nil {
			return err
		}
		w.log.Info("document deleted from vespa",
			"tenant_id", doc.GetTenantId(), "doc_id", doc.GetDocId())
		return nil
	}

	fields, err := w.buildFields(doc)
	if err != nil {
		return err
	}
	body, err := json.Marshal(feedDocument{Fields: fields})
	if err != nil {
		return fmt.Errorf("index-writer: marshal feed for doc %s: %w", doc.GetDocId(), err)
	}
	if err := w.send(ctx, http.MethodPut, docURL, body, doc.GetDocId()); err != nil {
		return err
	}
	w.log.Info("document fed to vespa",
		"tenant_id", doc.GetTenantId(), "doc_id", doc.GetDocId(),
		"version_etag", doc.GetVersionEtag(), "chunks", len(doc.GetChunks()))
	return nil
}

// documentURL builds the per-tenant-group document/v1 path. Both segments are
// path-escaped: tenant ids are already restricted to a safe alphabet
// (platform/tenancy), but doc ids are connector-derived and untrusted.
func (w *writer) documentURL(tenantID, docID string) string {
	return w.vespaURL + "/document/v1/asker/doc/group/" +
		url.PathEscape(tenantID) + "/" + url.PathEscape(docID)
}

// feedDocument is the document/v1 feed envelope.
type feedDocument struct {
	Fields vespaFields `json:"fields"`
}

// vespaFields mirrors the M1 schema feed shape exactly (vespa/README.md).
type vespaFields struct {
	DocID        string       `json:"doc_id"`
	ConnectorID  string       `json:"connector_id"`
	Type         string       `json:"type"`
	Title        string       `json:"title"`
	Body         string       `json:"body"`
	Chunks       []string     `json:"chunks,omitempty"`
	Embedding    *vespaTensor `json:"embedding,omitempty"`
	Participants []string     `json:"participants,omitempty"`
	MetadataJSON string       `json:"metadata_json"`
	CreatedAt    int64        `json:"created_at"`
	ModifiedAt   int64        `json:"modified_at"`
	VersionEtag  string       `json:"version_etag"`
	ACL          []string     `json:"acl,omitempty"`
}

// vespaTensor is the blocks form of the mixed tensor
// tensor<bfloat16>(chunk{},x[dim]): one dense array per chunk label, the
// label being the stringified index of the chunk in the chunks array.
type vespaTensor struct {
	Blocks map[string][]float32 `json:"blocks"`
}

// buildFields converts a Document into the Vespa feed fields. It returns an
// error when any chunk vector's length differs from EMBEDDING_DIM — a
// misconfigured dimension must never silently index (ADR-005).
func (w *writer) buildFields(doc *askerv1.Document) (vespaFields, error) {
	chunks := doc.GetChunks()
	texts := make([]string, 0, len(chunks))
	blocks := make(map[string][]float32, len(chunks))
	for i, c := range chunks {
		texts = append(texts, c.GetText())
		emb := c.GetEmbedding()
		if len(emb) == 0 {
			continue
		}
		if len(emb) != w.dim {
			return vespaFields{}, fmt.Errorf(
				"index-writer: doc %s chunk %d: embedding has %d dims, EMBEDDING_DIM is %d; refusing to index",
				doc.GetDocId(), i, len(emb), w.dim)
		}
		blocks[strconv.Itoa(i)] = emb
	}

	// The embedding field is fed only when EVERY chunk has a vector; a
	// partially embedded document would silently lose recall on the missing
	// chunks, so feed it text-only and let a re-enrich fill the vectors.
	var tensor *vespaTensor
	switch {
	case len(chunks) > 0 && len(blocks) == len(chunks):
		tensor = &vespaTensor{Blocks: blocks}
	case len(blocks) > 0:
		w.log.Warn("document has vectors for only some chunks; feeding without embeddings",
			"doc_id", doc.GetDocId(), "chunks", len(chunks), "with_vectors", len(blocks))
	}

	meta := doc.GetMetadata()
	if meta == nil {
		meta = map[string]string{}
	}
	metaJSON, err := json.Marshal(meta) // map keys are marshaled sorted: deterministic
	if err != nil {
		return vespaFields{}, fmt.Errorf("index-writer: doc %s: marshal metadata: %w", doc.GetDocId(), err)
	}

	created := doc.GetTs().GetCreated().GetSeconds()
	modified := created
	if m := doc.GetTs().GetModified(); m != nil {
		modified = m.GetSeconds()
	}

	return vespaFields{
		DocID:        doc.GetDocId(),
		ConnectorID:  doc.GetConnectorId(),
		Type:         doc.GetType().String(),
		Title:        doc.GetTitle(),
		Body:         doc.GetBodyText(),
		Chunks:       texts,
		Embedding:    tensor,
		Participants: participantStrings(doc.GetParticipants()),
		MetadataJSON: string(metaJSON),
		CreatedAt:    created,
		ModifiedAt:   modified,
		VersionEtag:  doc.GetVersionEtag(),
		ACL:          doc.GetAcl().GetAllowedPrincipals(),
	}, nil
}

// participantStrings renders participants as "Name <email>"; the display name
// falls back to the platform handle. With no usable name the bare email (or
// bare handle when there is no email) is used. Entirely empty participants
// are dropped.
func participantStrings(ps []*askerv1.Participant) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		name := p.GetName()
		if name == "" {
			name = p.GetHandle()
		}
		email := p.GetEmail()
		switch {
		case name != "" && email != "":
			out = append(out, name+" <"+email+">")
		case email != "":
			out = append(out, email)
		case name != "":
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// send issues one Vespa document/v1 request with a single in-handler retry on
// 5xx/timeout/transport errors (kafkautil layers 3 more attempts on top). 4xx
// responses are permanent: logged with their body and returned without retry.
func (w *writer) send(ctx context.Context, method, reqURL string, body []byte, docID string) error {
	const attempts = 2
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		retryable, err := w.sendOnce(ctx, method, reqURL, body)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable || ctx.Err() != nil {
			return err
		}
		if attempt < attempts {
			w.log.Warn("vespa request failed; retrying once",
				"method", method, "doc_id", docID, "attempt", attempt, "error", err)
		}
	}
	return lastErr
}

// sendOnce performs a single request. The boolean reports whether the failure
// is retryable (5xx, timeout, transport error) as opposed to permanent (4xx).
func (w *writer) sendOnce(ctx context.Context, method, reqURL string, body []byte) (bool, error) {
	rctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()

	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(rctx, method, reqURL, rd)
	if err != nil {
		return false, fmt.Errorf("index-writer: build %s %s: %w", method, reqURL, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := w.client.Do(req)
	if err != nil {
		// Timeouts and transport errors are worth one immediate retry.
		return true, fmt.Errorf("index-writer: %s %s: %w", method, reqURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		return false, nil
	case resp.StatusCode >= 400 && resp.StatusCode <= 499:
		// Permanent: the same bytes will be rejected again. Log the response
		// body loudly — it carries Vespa's reason — and let kafkautil
		// dead-letter the record after its attempts.
		w.log.Error("vespa rejected request",
			"method", method, "url", reqURL, "status", resp.StatusCode, "body", string(respBody))
		return false, fmt.Errorf("index-writer: %s %s: status %d: %s: %w",
			method, reqURL, resp.StatusCode, respBody, errVespaPermanent)
	default:
		return true, fmt.Errorf("index-writer: %s %s: status %d: %s",
			method, reqURL, resp.StatusCode, respBody)
	}
}
