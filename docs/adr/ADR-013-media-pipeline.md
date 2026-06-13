# ADR-013: Media pipeline (OCR, CLIP, ASR) and the two embedding spaces

## Status

Accepted (M3).

## Context

M3 makes images, audio, and video searchable: OCR + CLIP for images, faster-whisper ASR for
audio/video with timestamp-anchored chunks (a hit deep-links to the moment), ffmpeg keyframes for
video, and thumbnails. Two facts shape the design:

1. **Text and images do not share one embedding space.** Text/OCR/ASR/caption content is embedded
   with `bge-m3` via TEI (ADR-005), a text model. True *text→image* semantic search (a text query
   matching an image with no shared keywords) needs **CLIP**, whose text and image encoders share a
   *different* space. So Asker has two vector spaces, queried differently.
2. **Original blobs are envelope-encrypted in Go** (`platform/crypto`, per-tenant DEK; ADR
   M1/security), but enrichment is **Python** (ADR-007). The Python worker cannot decrypt blobs
   without reimplementing the security-critical crypto.

## Decision

**Two embedding spaces, two retrieval arms.**
- The existing `embedding` tensor (bge-m3, `EMBEDDING_DIM`) holds text/OCR/ASR/caption chunk
  vectors — media is searchable in the unified text space via its extracted text.
- A new `clip_embedding` tensor (`CLIP_DIM` = 512 for ViT-B/32) holds CLIP *image* vectors on
  image/keyframe chunks. For *text→image*, the query service also encodes the query with **CLIP
  text** and runs a `nearestNeighbor` over `clip_embedding`. Results from the two arms are merged
  (the text arm via the M2 hybrid `rank()`, the CLIP arm by closeness), de-duplicated by `doc_id`.

**A `clip` model service (Python, HTTP), analogous to TEI.** It serves `POST /embed/text` and
`POST /embed/image` from one loaded CLIP model so the two encoders stay in the same space. Enrich
calls `/embed/image` for images/keyframes; the query service calls `/embed/text` for text→image
queries. Dev/CI use ViT-B/32 (small); `CLIP_MODEL` is overridable. `CLIP_DIM` is deploy-time config
(like `EMBEDDING_DIM`) and templated into the Vespa schema by `vespa/deploy.sh`.

**Media bytes reach the Python enrich worker through a Go internal endpoint.** The connector-hub —
which already owns `platform/blob` + the tenant cipher — exposes internal-only, `x-asker-tenant`-
scoped `GET /internal/media?key=...` (returns DECRYPTED bytes) and `PUT /internal/media?key=...`
(encrypts + stores thumbnails/keyframes). The crypto stays in Go; the worker never sees a DEK. The
trust boundary is the internal network (ADR-009), hardened by mTLS/NetworkPolicy in M4.

**Pipeline by media type (in the Python enrich worker, small models):**
- **IMAGE**: fetch blob → Tesseract OCR → an `ocr` chunk (bge-m3 via TEI) + CLIP image embedding on
  a `caption`/image chunk (`clip_embedding`) → a thumbnail stored back via the hub. `MediaInfo`
  gets width/height/thumbnail.
- **AUDIO**: faster-whisper (tiny) → transcript segments → `asr` chunks with `start_ms`/`end_ms`
  (bge-m3). `MediaInfo` gets duration + transcript language.
- **VIDEO**: ffmpeg extracts the audio track → whisper (as audio); ffmpeg scene-detection extracts
  keyframes → CLIP per keyframe (`Keyframe` + a chunk carrying its `clip_embedding`, time-anchored)
  + a poster thumbnail. ASR + keyframe chunks coexist on one Document.

**Chunk anchoring.** `Chunk.start_ms`/`end_ms`/`modality` (added in the M3 proto foundation) carry
the time offset and kind; Vespa stores parallel `chunk_starts_ms`/`chunk_ends_ms`/`chunk_modalities`
arrays so the query path returns the matched chunk's offset for deep-linking, plus the per-keyframe
`clip_embedding`.

**Ingest** routes by `Document.type`/`original.content_type`: media docs (no `body_text`) pass
through `docs.chunked` unchanged (no text chunking) — the enrich worker produces their chunks.

## Consequences

- The enrich image grows (faster-whisper + ffmpeg + Tesseract); CLIP/torch live in the separate
  `clip` service. Dev/CI use tiny models (whisper-tiny, ViT-B/32) — the bge-small pattern (ADR-005)
  — so the stack fits a constrained dev VM; production sizes up in M4/M5.
- The query path gains a CLIP arm and a second model dependency; degradation (ADR-006) extends: if
  the `clip` service is down, text→image silently drops to the text/OCR arm (never fail closed).
- The hub gains a decrypt/encrypt media endpoint — a deliberate, internal-only widening of its
  role, justified by keeping envelope crypto in one (Go) place. Removed/locked down in M4.
- Two embedding spaces mean two `EMBEDDING_DIM`/`CLIP_DIM` knobs that must match their models;
  mismatches are rejected at index time (as with bge-m3).
- M3 exit criterion ("search a spoken phrase → the video at the right timestamp") rides entirely on
  the ASR→bge-m3 arm (unified space) — the simplest, most robust path; CLIP text→image is the
  additional spec feature.
