# services/clip — CLIP model service

The CLIP embedding service for Asker's M3 media pipeline (ADR-013). It exposes
**one loaded open_clip model** behind a small HTTP API so that text and image
vectors live in the **same** space — the prerequisite for true *text→image*
semantic search. It is analogous to the TEI service (which serves the bge-m3
*text* space, ADR-005); CLIP is a *second*, separate space.

- **Enrich** (the Python worker) calls `POST /embed/image` to embed image and
  video-keyframe chunks (stored in Vespa's `clip_embedding` tensor).
- **Query** calls `POST /embed/text` to encode a search query into the CLIP
  space and runs a `nearestNeighbor` over `clip_embedding` (the CLIP retrieval
  arm). If this service is down, the query path silently drops that arm and
  falls back to the bge-m3 text/OCR arm — degradation, never fail-closed
  (ADR-006/ADR-013).

## API

All endpoints are on a single listener (`CLIP_PORT`, default `:9800`).

### `POST /embed/text`
```json
// request
{"inputs": ["a red bicycle", "sunset over the sea"]}
// response (200)
{"embeddings": [[<float x CLIP_DIM>], [<float x CLIP_DIM>]]}
```

### `POST /embed/image`
```json
// request — base64-encoded image bytes (an optional "data:...;base64," prefix
// is accepted and stripped)
{"images_b64": ["<base64 of a PNG/JPEG/...>", "..."]}
// response (200)
{"embeddings": [[<float x CLIP_DIM>], ...]}
```

### `GET /health`
- `200 {"status":"ok"}` once the model is loaded.
- `503 {"status":"loading"}` while the model is still loading (the listener is
  up immediately; the model loads in a background thread).

**Contract guarantees (both embed endpoints):**
- Output order matches input order; one vector per input.
- Every vector has exactly `CLIP_DIM` floats and is **L2-normalized**, so a
  Vespa `angular`/dotproduct closeness over `clip_embedding` is cosine
  similarity.
- Text and image vectors come from the *same* loaded model and so share a
  space (a text query can match an image with no shared keywords).

**Error responses** are `{"error": "<message>"}` with:
- `400` — bad JSON, non-object body, missing/wrong-typed `inputs`/`images_b64`,
  invalid base64, or an undecodable image.
- `404` — unknown route.
- `411` — missing `Content-Length`.
- `413` — request body over `CLIP_MAX_BODY_BYTES`, or more than
  `CLIP_MAX_TEXTS` / `CLIP_MAX_IMAGES` items.
- `500` — the loaded model returned a vector whose length ≠ `CLIP_DIM`
  (operator misconfiguration — `CLIP_DIM` must match the model; see below).
- `503` — the model is not loaded yet.

## Configuration (environment)

| Env                 | Default     | Meaning |
|---------------------|-------------|---------|
| `CLIP_MODEL`        | `ViT-B-32`  | open_clip model architecture. |
| `CLIP_PRETRAINED`   | `openai`    | open_clip pretrained tag. |
| `CLIP_DIM`          | `512`       | Output dimension. **Must match the model** (`ViT-B-32`/`openai` → 512). Deploy-time config, like `EMBEDDING_DIM` for TEI (ADR-005); vectors of any other length are rejected. |
| `CLIP_PORT`         | `:9800`     | Listen address (`:9800` or `host:9800`). |
| `CLIP_MAX_TEXTS`    | `64`        | Max `inputs` per `/embed/text` call. |
| `CLIP_MAX_IMAGES`   | `32`        | Max `images_b64` per `/embed/image` call. |
| `CLIP_MAX_BODY_BYTES` | `33554432` (32 MiB) | Max request body size. |

`CLIP_DIM` is the schema knob `vespa/deploy.sh` substitutes for the
`clip_embedding` tensor (`@CLIP_DIM@`); it must equal this service's `CLIP_DIM`
and the model's true output dimension, or vectors are rejected at index time.

## Model loading and first-call latency

The model is **not** baked into the image; it loads lazily on startup into the
open_clip / huggingface cache (`HF_HOME`, default `/home/nonroot/.cache/...`).

- **Cold cache (first run):** downloads `ViT-B-32`/`openai` weights (~340 MB)
  then initializes the model. This takes seconds to a few minutes depending on
  bandwidth and CPU. During this window `/health` returns `503` and embed calls
  return `503 {"error":"clip model is not loaded yet"}`.
- **Warm cache:** model init only (a few seconds).

Mount a **persistent volume** at the cache directory so the weights survive
container restarts and CI runs (no re-download). Give the container's health
check a **generous `start_period`** so the orchestrator does not kill it during
the first load.

The CPU forward pass is serialized under a lock (open_clip/torch are not
guaranteed thread-safe); this is fine for a CPU dev/CI service. Production
scales by running more replicas (ADR-013), each loading the same model.

## Dev / CI small model

Dev and CI use **`ViT-B-32` / `openai`** (the small 512-d model) — the same
"small model in dev" pattern as bge-small for TEI (ADR-005) — so the stack fits
a constrained dev VM. Production can size up (`CLIP_MODEL`/`CLIP_PRETRAINED` +
matching `CLIP_DIM`) in M4/M5.

## Development

```sh
python3 -m venv .venv
./.venv/bin/pip install -r requirements-dev.txt   # pytest + ruff only (NO torch)
./.venv/bin/ruff check clip tests
./.venv/bin/ruff format --check clip tests
./.venv/bin/pytest -q
```

Unit tests run against a **deterministic fake encoder** (`tests/conftest.py`)
and the stdlib HTTP layer — they never import torch / open_clip or download a
model. They cover routing, JSON shapes, base64 decode, batching/limits, error
cases, `/health`, embedding length (`== CLIP_DIM`) and L2-normalization.

### Real-model smoke (integration, not a unit test)

The real model is too heavy for unit tests. To exercise the actual encoder:

```sh
# Install runtime deps incl. the torch CPU wheel (large download):
./.venv/bin/pip install --extra-index-url https://download.pytorch.org/whl/cpu \
    -r requirements.txt
# Run the service (first run downloads ViT-B-32/openai weights):
PYTHONPATH=. ./.venv/bin/python -m clip.main &
# Wait for /health to be 200, then:
curl -s localhost:9800/health
curl -s localhost:9800/embed/text -d '{"inputs":["a red bicycle"]}'
B64=$(base64 < some_image.png | tr -d '\n')
curl -s localhost:9800/embed/image -d "{\"images_b64\":[\"$B64\"]}"
# A text query and an image of the same thing should have high cosine
# similarity (dot product of the two unit vectors close to 1).
```

## Docker

```sh
docker build -f services/clip/Dockerfile services/clip   # context is services/clip
docker run --rm -p 127.0.0.1:9800:9800 \
    -v clip-cache:/home/nonroot/.cache asker-clip
```

The image installs the **torch CPU wheel** from the PyTorch CPU index. The
container runs as non-root. Health check:
`["CMD", "python", "-m", "clip.main", "-healthcheck"]` (self-probes
`/health`).
