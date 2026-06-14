# services/enrich — the enrich worker

The Python enrich worker consumes `docs.chunked`, fills in the embeddings (and,
for media, the chunks themselves), and produces `docs.enriched` (ADR-004,
ADR-007). Every stage of the pipeline speaks the canonical `asker.v1.Document`
protobuf; there are no stage-private message types.

```
docs.chunked ──► enrich ──► docs.enriched
                   │
                   └──► docs.deadletter   (after 3 failed attempts)
```

Per-record contract (parity with `platform/kafkautil`, by test not construction):
re-validate the `tenant_id` header against the payload, retry handler failures 3×
with capped exponential backoff, quarantine to `docs.deadletter` with
`error`/`origin_topic` headers, and **commit only after a successful produce** —
at-least-once, never skipped silently.

## Two paths through `process()`

A record is routed by `Document.type` / `original.content_type`:

| Document                                   | path                                                        |
|--------------------------------------------|-------------------------------------------------------------|
| tombstone (`tombstone.deleted`)            | pass through byte-for-byte unchanged                        |
| text doc with chunks                       | embed each `chunk.text` via **TEI** (bge-m3), fill `embedding` |
| zero-chunk text doc                        | pass through unchanged                                      |
| **IMAGE / AUDIO / VIDEO** (or a media MIME) | the **media handler** (this is M3, below)                   |

All three branches produce-then-commit through the **same** retry/dead-letter
loop, so a transient broker/model error retries and then dead-letters — it never
crashes the worker, least of all on a delete tombstone.

## The media pipeline (M3, ADR-013)

A media document carries no `body_text`, so ingest produced no chunks for it; the
media handler (`enrich/media.py`) produces them. The design hinges on **two
embedding spaces** (ADR-013):

- **bge-m3** (text), length `EMBEDDING_DIM`, served by **TEI** — the same
  embedder the text path uses. Carries text / OCR / ASR chunk vectors.
- **CLIP** (image+text), length `CLIP_DIM`, served by the separate **`clip`**
  HTTP service. Carries image / video-keyframe chunk vectors for true
  text→image search.

A `Chunk` has one `embedding` field. The index-writer routes it to Vespa's
`embedding` field when `len == EMBEDDING_DIM` or `clip_embedding` when
`len == CLIP_DIM` (see `vespa/README.md`). `modality` + `start_ms`/`end_ms`
become Vespa's parallel `chunk_modalities` / `chunk_starts_ms` arrays so a hit
deep-links to the matched moment.

### Chunk / dim / modality contract produced by the handler

| chunk      | `modality` | `text`        | `embedding` length | time anchor              |
|------------|------------|---------------|--------------------|--------------------------|
| OCR        | `ocr`      | extracted text| `EMBEDDING_DIM` (bge-m3) | none (char offsets)  |
| ASR        | `asr`      | transcript    | `EMBEDDING_DIM` (bge-m3) | `start_ms`/`end_ms`  |
| image      | `caption`  | `""`          | `CLIP_DIM` (CLIP)  | none                     |
| keyframe   | `caption`  | `""`          | `CLIP_DIM` (CLIP)  | `start_ms = end_ms = ts` |

Per media type:

- **IMAGE** — fetch original bytes → Tesseract OCR (an `ocr` chunk via TEI, only
  when there is text) + CLIP image embedding (a `caption` chunk) + a Pillow
  thumbnail stored back through the hub. `media.width`/`height`/`thumbnail` set.
- **AUDIO** — faster-whisper (tiny, CPU, int8) → transcript segments merged into
  ~512-token windows → `asr` chunks (via TEI), each anchored at
  `start_ms`/`end_ms` (segment seconds × 1000). `media.duration_ms` +
  `media.transcript_lang` (detected language) set.
- **VIDEO** — ffmpeg extracts the audio track → the AUDIO path (time-anchored
  `asr` chunks); ffmpeg scene-detection (`select='gt(scene,0.4)'`, periodic
  `fps=1/5` fallback, capped at `MAX_KEYFRAMES`) → per keyframe a CLIP `caption`
  chunk (anchored at the frame time) **and** a `Keyframe{ts_ms, image, chunk_id}`
  (the frame JPEG stored through the hub). The first keyframe is the poster
  `media.thumbnail`; `media.duration_ms`/`width`/`height` from ffprobe.

A media doc that yields **zero chunks** (silent audio, no OCR text, no keyframes)
**still produces** to `docs.enriched` (so the doc exists and is searchable by
metadata) and logs that it did.

### Injected collaborators (testability — ADR-007)

Every external dependency is a small Protocol injected into `MediaHandler`
(declared in `enrich/media_clients.py`), exactly like the existing Kafka/embedder
injection. Unit tests substitute deterministic fakes and never run a real model,
binary, or HTTP server:

| collaborator      | Protocol         | production impl              | wraps                                   |
|-------------------|------------------|------------------------------|-----------------------------------------|
| hub media store   | `MediaStore`     | `HubMediaStore`              | `GET/PUT {HUB_MEDIA_URL}/internal/media`|
| CLIP client       | `ClipClient`     | `HttpClipClient`             | `POST {CLIP_URL}/embed/image`           |
| OCR               | `OCRFunc`        | `tesseract_ocr`              | Pillow + pytesseract + `tesseract`      |
| transcriber       | `Transcriber`    | `WhisperTranscriber`         | faster-whisper (CTranslate2, no torch)  |
| video extractor   | `VideoExtractor` | `FFmpegVideoExtractor`       | `ffmpeg` / `ffprobe` subprocess         |
| thumbnailer       | `Thumbnailer`    | `pillow_thumbnailer(max_px)` | Pillow resize → JPEG                     |
| text embedder     | `EmbedderLike`   | `Embedder` (shared)          | TEI `POST /embed`                       |

The heavy libraries (faster-whisper / Pillow / pytesseract) and the
`ffmpeg`/`ffprobe`/`tesseract` binaries are imported / spawned **lazily inside
the production methods**, so importing the worker at startup is cheap and a
unit-test process that injects fakes never loads them. A collaborator failure
raises `MediaError` (or any exception), which the worker's retry/dead-letter loop
catches — it is never a crash.

The bytes never decrypt in Python: the connector-hub's `/internal/media`
endpoint owns the per-tenant envelope crypto (Go). `HubMediaStore` sends the
`x-asker-tenant` header (so the hub scopes the read to the tenant) and passes
back the full `BlobRef` (`key`+`sha256`+`bucket`+`content_type`) it already holds
from `Document.original`, because `blob.Get` verifies the plaintext digest.

## Configuration (environment)

| Env                    | Default                       | Meaning |
|------------------------|-------------------------------|---------|
| `KAFKA_BROKERS`        | `redpanda:9092`               | comma-separated brokers |
| `TEI_URL`              | `http://tei:80`               | bge-m3 text embedding service |
| `EMBEDDING_DIM`        | **required**                  | bge-m3 output dim (ADR-005); no default |
| `CLIP_URL`             | `http://clip:9800`            | CLIP image/text embedding service (ADR-013) |
| `CLIP_DIM`             | **required**                  | CLIP output dim (512 for ViT-B/32); no default |
| `HUB_MEDIA_URL`        | `http://connector-hub:9300`   | hub internal media decrypt/encrypt endpoint |
| `WHISPER_MODEL`        | `tiny`                        | faster-whisper model name |
| `WHISPER_COMPUTE_TYPE` | `int8`                        | CTranslate2 compute type (CPU) |
| `MAX_KEYFRAMES`        | `20`                          | per-video keyframe cap |
| `THUMBNAIL_MAX_PX`     | `512`                         | thumbnail longest-edge pixels |
| `ENRICH_HEALTH_ADDR`   | `:9601`                       | health listener (`/healthz`, `/readyz`) |

`EMBEDDING_DIM` and `CLIP_DIM` are required-with-no-default (like ADR-005): each
must match the model its service actually serves; a wrong guess feeds wrong-size
vectors to Vespa, which is strictly worse than refusing to start.

`/readyz` requires TEI healthy; CLIP being down does **not** fail readiness (the
media path degrades to the bge-m3 text/OCR/ASR arm — ADR-006/013) but is surfaced
in the response body.

## Dependencies and system binaries

Python (pinned in `requirements.txt`): `aiokafka`, `cramjam` (snappy codec —
the Go producers compress batches), `httpx`, `protobuf`, plus the M3 media deps
`faster-whisper` (CTranslate2, **no torch**), `pillow`, `pytesseract`.

System binaries (installed in the `Dockerfile`): `tesseract-ocr`, `ffmpeg`
(provides `ffprobe`). CLIP/torch are **not** here — CLIP runs in `services/clip`.

The whisper model downloads on **first use** (not at build) into `HF_HOME`
(`/home/nonroot/.cache/huggingface`). Mount a persistent volume there so the
weights survive restarts/CI and the first media document isn't blocked
re-downloading.

## Development

Target runtime is Python **3.12** (the Docker image; ML workers are 3.12 per
`CLAUDE.md`). Unit tests need no models/binaries — fakes only.

```sh
python3.12 -m venv .venv
./.venv/bin/pip install -r requirements.txt -r requirements-dev.txt
PYTHONPATH=.:../../platform/proto/gen/python ./.venv/bin/pytest -q
./.venv/bin/ruff check .
./.venv/bin/ruff format --check .
```

`pyproject.toml` already wires the `pythonpath` (`.` + the committed proto
gencode) for pytest, so a bare `pytest` works inside the venv too.

### Test layout

- `tests/test_config.py` — env parsing (`EMBEDDING_DIM`/`CLIP_DIM` required).
- `tests/test_embedder.py` — TEI client via `httpx.MockTransport`.
- `tests/test_worker.py` — the per-record contract + **media routing** (a media
  doc goes to the injected media handler, not the embedder; a media-handler
  failure retries then dead-letters; tombstones/no-handler pass through).
- `tests/test_media.py` — the media handler against injected fakes: IMAGE
  (OCR + caption + thumbnail), AUDIO (ASR ms-anchoring + windowing), VIDEO
  (ASR + keyframe captions + Keyframes + poster), the dim/modality routing,
  zero-chunk-still-produces, and collaborator-failure-raises.
- `tests/test_media_clients.py` — `HubMediaStore` / `HttpClipClient` over
  `httpx.MockTransport` (tenant header, BlobRef params, dim validation) + the
  pure helpers (showinfo PTS parsing, content-type → suffix).
- `tests/test_proto_roundtrip.py` — guards against proto/bindings drift.

## Docker

```sh
# Build context is the REPO ROOT (the committed proto gencode is copied in):
docker build -f services/enrich/Dockerfile .
# Compose healthcheck: ["CMD", "python", "-m", "enrich.main", "-healthcheck"]
```
