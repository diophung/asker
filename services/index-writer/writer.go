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
// operations: tombstones become DELETEs, everything else a full-document POST
// (document/v1 "put": creates or fully replaces, so replays are idempotent).
// PUT is reserved by document/v1 for partial updates with
// {"fields":{"f":{"assign":...}}} syntax — never used here (see
// vespa/README.md).
type writer struct {
	vespaURL string // base URL, no trailing slash
	dim      int    // EMBEDDING_DIM; bge-m3 (text/ocr/asr/caption) chunk vectors must have exactly this length
	clipDim  int    // CLIP_DIM; CLIP image/keyframe chunk vectors must have exactly this length (ADR-013)
	timeout  time.Duration
	client   *http.Client
	log      *slog.Logger
}

func newWriter(vespaURL string, dim, clipDim int, logger *slog.Logger) (*writer, error) {
	vespaURL = strings.TrimRight(vespaURL, "/")
	if vespaURL == "" {
		return nil, errors.New("index-writer: vespa url must not be empty")
	}
	if dim <= 0 {
		return nil, fmt.Errorf("index-writer: embedding dim must be positive, got %d", dim)
	}
	if clipDim <= 0 {
		return nil, fmt.Errorf("index-writer: clip dim must be positive, got %d", clipDim)
	}
	if dim == clipDim {
		// A chunk's vector is routed to embedding vs clip_embedding purely by
		// its length (see buildFields); equal dims make that undecidable.
		return nil, fmt.Errorf("index-writer: clip dim (%d) must differ from embedding dim (%d)", clipDim, dim)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &writer{
		vespaURL: vespaURL,
		dim:      dim,
		clipDim:  clipDim,
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
	if err := w.send(ctx, http.MethodPost, docURL, body, doc.GetDocId()); err != nil {
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

// vespaFields mirrors the schema feed shape exactly (vespa/README.md) plus the
// M3 media additions (ADR-013).
type vespaFields struct {
	DocID       string       `json:"doc_id"`
	ConnectorID string       `json:"connector_id"`
	Type        string       `json:"type"`
	Title       string       `json:"title"`
	Body        string       `json:"body"`
	Chunks      []string     `json:"chunks,omitempty"`
	Embedding   *vespaTensor `json:"embedding,omitempty"`
	// CLIPEmbedding holds CLIP image/keyframe vectors (CLIP_DIM), keyed by the
	// same chunk array index as Chunks. Present only when at least one chunk
	// carries a CLIP vector; omitted entirely for pure-text documents (M3).
	CLIPEmbedding *vespaTensor `json:"clip_embedding,omitempty"`
	// ChunkStartsMs/ChunkEndsMs/ChunkModalities are parallel to Chunks (same
	// index order). They anchor media chunks in time and label their kind so
	// the query path can deep-link and tag hits (ADR-013). Text chunks carry
	// 0/0/"text". Omitted when there are no chunks.
	ChunkStartsMs   []int64  `json:"chunk_starts_ms,omitempty"`
	ChunkEndsMs     []int64  `json:"chunk_ends_ms,omitempty"`
	ChunkModalities []string `json:"chunk_modalities,omitempty"`
	Participants    []string `json:"participants,omitempty"`
	MetadataJSON    string   `json:"metadata_json"`
	CreatedAt       int64    `json:"created_at"`
	// EventStart/EventEnd are the event OCCURRENCE time (Unix epoch seconds),
	// parsed from metadata["start"]/["end"] for calendar events; 0/omitted for
	// non-event docs. The query path filters/orders schedule lookups on these
	// (the correct field for "next week" — created_at is the authoring time).
	EventStart  int64    `json:"event_start,omitempty"`
	EventEnd    int64    `json:"event_end,omitempty"`
	ModifiedAt  int64    `json:"modified_at"`
	VersionEtag string   `json:"version_etag"`
	ACL         []string `json:"acl,omitempty"`
	// Media metadata from Document.media (MediaInfo); zero/omitted for text
	// documents (ADR-013).
	MediaDurationMs int64  `json:"media_duration_ms,omitempty"`
	MediaWidth      int64  `json:"media_width,omitempty"`
	MediaHeight     int64  `json:"media_height,omitempty"`
	ThumbnailKey    string `json:"thumbnail_key,omitempty"`
	TranscriptLang  string `json:"transcript_lang,omitempty"`
}

// vespaTensor is the blocks form of the mixed tensor
// tensor<bfloat16>(chunk{},x[dim]): one dense array per chunk label, the
// label being the stringified index of the chunk in the chunks array.
type vespaTensor struct {
	Blocks map[string][]float32 `json:"blocks"`
}

// buildFields converts a Document into the Vespa feed fields. It returns an
// error when any chunk vector's length is neither EMBEDDING_DIM nor CLIP_DIM —
// a misconfigured/mismatched dimension must never silently index (ADR-005,
// ADR-013).
//
// Vector routing (M3): a chunk carries ONE vector in Chunk.embedding. The
// destination Vespa field is decided BY ITS LENGTH, not its modality:
//   - len == EMBEDDING_DIM -> the bge-m3 'embedding' tensor (text/ocr/asr/
//     caption text vectors, the unified text space).
//   - len == CLIP_DIM      -> the 'clip_embedding' tensor (CLIP image/keyframe
//     vectors, the text->image space).
//
// The wave-1 enrich worker fills Chunk.embedding with the bge-m3 vector for
// text chunks and the CLIP vector for image/keyframe chunks (modality is the
// human-readable label; length is the machine-checkable discriminator). Any
// other length dead-letters the record so a dim mismatch is never indexed.
func (w *writer) buildFields(doc *askerv1.Document) (vespaFields, error) {
	chunks := doc.GetChunks()
	texts := make([]string, 0, len(chunks))
	textBlocks := make(map[string][]float32, len(chunks))
	clipBlocks := make(map[string][]float32, len(chunks))
	var startsMs, endsMs []int64
	var modalities []string
	if len(chunks) > 0 {
		startsMs = make([]int64, len(chunks))
		endsMs = make([]int64, len(chunks))
		modalities = make([]string, len(chunks))
	}
	for i, c := range chunks {
		texts = append(texts, c.GetText())
		startsMs[i] = c.GetStartMs()
		endsMs[i] = c.GetEndMs()
		modalities[i] = chunkModality(c)

		emb := c.GetEmbedding()
		switch len(emb) {
		case 0:
			// No vector for this chunk (e.g. not yet enriched).
		case w.dim:
			textBlocks[strconv.Itoa(i)] = emb
		case w.clipDim:
			clipBlocks[strconv.Itoa(i)] = emb
		default:
			return vespaFields{}, fmt.Errorf(
				"index-writer: doc %s chunk %d (modality %q): embedding has %d dims, expected EMBEDDING_DIM=%d or CLIP_DIM=%d; refusing to index",
				doc.GetDocId(), i, chunkModality(c), len(emb), w.dim, w.clipDim)
		}
	}

	// The bge-m3 embedding field is fed only when EVERY text-eligible chunk has
	// a bge-m3 vector; a partially embedded document would silently lose recall
	// on the missing chunks. CLIP-only chunks (keyframes/images) legitimately
	// carry no bge-m3 vector, so they are excluded from this completeness check
	// — the gate is "every non-CLIP chunk has a bge-m3 vector".
	var textChunkCount int
	for _, c := range chunks {
		if len(c.GetEmbedding()) != w.clipDim {
			textChunkCount++
		}
	}
	var tensor *vespaTensor
	switch {
	case textChunkCount > 0 && len(textBlocks) == textChunkCount:
		tensor = &vespaTensor{Blocks: textBlocks}
	case len(textBlocks) > 0:
		w.log.Warn("document has bge-m3 vectors for only some text chunks; feeding without embeddings",
			"doc_id", doc.GetDocId(), "text_chunks", textChunkCount, "with_vectors", len(textBlocks))
	}

	// clip_embedding is present only for chunks that actually carry a CLIP
	// vector; the field is omitted entirely when no chunk has one (text docs).
	var clipTensor *vespaTensor
	if len(clipBlocks) > 0 {
		clipTensor = &vespaTensor{Blocks: clipBlocks}
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

	// Media metadata (MediaInfo); all zero/empty for text documents, where the
	// omitempty tags then drop the fields from the feed entirely.
	media := doc.GetMedia()

	return vespaFields{
		DocID:           doc.GetDocId(),
		ConnectorID:     doc.GetConnectorId(),
		Type:            doc.GetType().String(),
		Title:           doc.GetTitle(),
		Body:            doc.GetBodyText(),
		Chunks:          texts,
		Embedding:       tensor,
		CLIPEmbedding:   clipTensor,
		ChunkStartsMs:   startsMs,
		ChunkEndsMs:     endsMs,
		ChunkModalities: modalities,
		Participants:    participantStrings(doc.GetParticipants()),
		MetadataJSON:    string(metaJSON),
		CreatedAt:       created,
		EventStart:      eventEpoch(meta["start"]),
		EventEnd:        eventEpoch(meta["end"]),
		ModifiedAt:      modified,
		VersionEtag:     doc.GetVersionEtag(),
		ACL:             doc.GetAcl().GetAllowedPrincipals(),
		MediaDurationMs: media.GetDurationMs(),
		MediaWidth:      int64(media.GetWidth()),
		MediaHeight:     int64(media.GetHeight()),
		ThumbnailKey:    media.GetThumbnail().GetKey(),
		TranscriptLang:  media.GetTranscriptLang(),
	}, nil
}

// eventEpoch parses a calendar event start/end metadata value into Unix epoch
// seconds, accepting RFC3339 (timed events), a YYYY-MM-DD date (all-day events),
// or an already-epoch string. Returns 0 for empty/unparseable values so the
// omitempty field drops out for non-event documents (v3.2, DECISIONS D11).
func eventEpoch(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Unix()
		}
	}
	if sec, err := strconv.ParseInt(s, 10, 64); err == nil && sec > 0 {
		return sec
	}
	return 0
}

// chunkModality returns the chunk's modality, defaulting to "text" when unset
// so the parallel chunk_modalities array always has a meaningful label (the
// proto leaves modality empty for plain text chunks). The query path expects
// one of "text"|"ocr"|"asr"|"caption" (ADR-013).
func chunkModality(c *askerv1.Chunk) string {
	if m := c.GetModality(); m != "" {
		return m
	}
	return "text"
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
