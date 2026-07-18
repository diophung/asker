# reranker service

A cross-encoder reranker over HTTP (Phase 1 of the hybrid-search upgrade). Given
a query and a batch of candidate documents, it returns one relevance score per
document. The query service calls it to reorder the top fused retrieval
candidates before the final ordering (`QUERY_RERANK_ENABLED`, request opt-in via
`?rerank=1`). Structured exactly like `services/clip`.

## Model

Default **`BAAI/bge-reranker-v2-m3`** (Apache-2.0, 568M) — an encoder
cross-encoder that reliably lands in the ~300 ms / 50-candidate budget on Apple
Silicon with MPS/fp16, and the most battle-tested runtime story. Swap via
`RERANKER_MODEL` (e.g. a `Qwen3-Reranker` for a quality-leaning toggle; expect
higher latency). A model swap needs no schema change — the reranker only returns
a scalar per pair.

## API

```
POST /rerank   {"query": "...", "documents": ["...", ...]}
                 -> 200 {"scores": [float, ...]}   # one per document, INPUT order
GET  /health   -> 200 {"status":"ok"} once the model is loaded, 503 while loading
```

Higher score = more relevant. The caller (query service) rewrites each
candidate's retrieval score with this relevance and reorders; a non-200 or a
timeout degrades to the fused order (`degraded="rerank-unavailable"`), never an
error.

## Configuration (env)

| var | default | notes |
|---|---|---|
| `RERANKER_MODEL` | `BAAI/bge-reranker-v2-m3` | any sentence-transformers CrossEncoder |
| `RERANKER_DEVICE` | `auto` | `auto`\|`cpu`\|`cuda`\|`mps` — auto prefers cuda, then mps (Apple GPU), then cpu |
| `RERANKER_PRECISION` | `auto` | fp16 only ever on cuda |
| `RERANKER_PORT` | `:9900` | `:9900` or `host:9900` |
| `RERANKER_MAX_DOCUMENTS` | `200` | per-request cap (413 above it) |
| `RERANKER_MAX_QUERY_CHARS` / `RERANKER_MAX_DOC_CHARS` | `4000` / `8000` | server-side truncation |
| `RERANKER_BATCH_SIZE` | `32` | cross-encoder predict batch |
| `RERANKER_MAX_LENGTH` | `512` | cross-encoder token truncation |

An explicit `cuda`/`mps` that is not actually available fails the model load (so
`/health` stays 503) rather than silently serving on CPU; `auto` falls back.

## Apple Silicon (the M5 Max)

`RERANKER_DEVICE=mps` runs the cross-encoder on the Apple GPU via torch's MPS
backend (shipped in the default CPU wheel — no CUDA needed). This is the native,
fast path for local/offline use. The two-host deploy runs it on the RTX host
with `cuda` (build with `--build-arg TORCH_INDEX_URL=.../cu126`).

## Tests

`pytest` runs against a deterministic fake scorer and the stdlib HTTP layer — no
torch / sentence-transformers / model download (unit tests must stay light).

```sh
cd services/reranker
python -m venv .venv && . .venv/bin/activate
pip install -r requirements-dev.txt
pytest
ruff check .
```

A real-model smoke (loads bge-reranker-v2-m3 and scores a batch) is an
integration check, not a unit test — run it against a built image or a venv with
`requirements.txt` installed.
