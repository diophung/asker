# reranker service

A cross-encoder reranker over HTTP (Phase 1 of the hybrid-search upgrade). Given
a query and a batch of candidate documents, it returns one relevance score per
document. The query service calls it to reorder the top fused retrieval
candidates before the final ordering (`QUERY_RERANK_ENABLED`, request opt-in via
`?rerank=1`). Structured exactly like `services/clip`.

## Model

Default **`BAAI/bge-reranker-v2-m3`** (Apache-2.0, 568M), an encoder cross-encoder.
Latency depends on hardware, candidate count/length, batching and contention;
this repository does not establish a 300 ms Apple benchmark. MPS uses fp32.
Swap via `RERANKER_MODEL` only after measuring relevance and latency. A model
swap needs no index schema change because the reranker returns a scalar per pair.
The Apple profiles pin immutable revision
`953dc6f6f85a1b2dbfca4c34a2796e7dde08d41e`, verified against the
[official model metadata](https://huggingface.co/api/models/BAAI/bge-reranker-v2-m3)
on October 1, 2026.

## API

```
POST /rerank   {"query": "...", "documents": ["...", ...]}
                 -> 200 {"scores": [float, ...]}   # one per document, INPUT order
GET  /health   -> 200 after load + actual warm-up prediction; 503 while loading
GET  /identity -> same readiness plus model/device/precision/revision/fingerprint
```

Higher score = more relevant. The caller (query service) rewrites each
candidate's retrieval score with this relevance and reorders; a non-200 or a
timeout degrades to the fused order (`degraded="rerank-unavailable"`), never an
error.

One inference runs at a time. At most three validated requests wait in FIFO order
for up to 300 ms; a full queue or expired admission returns 503. Body reads happen
before model admission and have an absolute 500 ms deadline, so a stalled or
trickled body cannot hold the model slot. Connection threads, body bytes, total
input characters and socket/idle time remain bounded separately; health reads
do not acquire the model slot. An optional `X-Request-Timeout-Ms` header (1..30000)
supplies the caller's remaining stage budget and only shortens body/admission
deadlines. Expired queued work never begins scoring. A disconnected caller
cannot preempt an already-running PyTorch kernel.
Invalid/non-finite scores are rejected. Revision metadata unavailable from the
loaded model is reported unknown, not as confirmed execution provenance.

HTTP identity has separate `admission` queue depth and accepted/rejected/wait
counters. Successful calls return `X-Rerank-Queue-Wait-Ms`. Identity also samples
actual MPS tensor/driver allocator counters after warm-up, scoring and identity
reads. These are process samples, exclude CPU RSS/other processes/whole-system
peaks and may overlap other memory accounting; unavailable values are null.
Memory and admission counters do not change the model fingerprint. Access paths
and request content are omitted from logs.

## Configuration (env)

| var | default | notes |
|---|---|---|
| `RERANKER_MODEL` | `BAAI/bge-reranker-v2-m3` | any sentence-transformers CrossEncoder |
| `RERANKER_REVISION` | unset | optional immutable Hugging Face revision |
| `RERANKER_DEVICE` | `auto` | `auto`\|`cpu`\|`cuda`\|`mps` — auto prefers cuda, then mps (Apple GPU), then cpu |
| `RERANKER_PRECISION` | `auto` | fp16 only ever on cuda |
| `RERANKER_PORT` | `:9900` | `:9900` or `host:9900` |
| `RERANKER_MAX_DOCUMENTS` | `200` | per-request cap (413 above it) |
| `RERANKER_MAX_QUERY_CHARS` / `RERANKER_MAX_DOC_CHARS` | `4000` / `8000` | server-side truncation |
| `RERANKER_BATCH_SIZE` | `32` | cross-encoder predict batch |
| `RERANKER_MAX_LENGTH` | `512` | cross-encoder token truncation |
| `RERANKER_MAX_CONNECTIONS` | `16` | finite active request threads, at most 64 |
| `RERANKER_SOCKET_TIMEOUT_SECONDS` | `5` | socket body/idle timeout, at most 30 |
| `RERANKER_ADMISSION_WAIT_MS` | `300` | maximum FIFO wait, 0..3000; zero rejects busy work immediately; longer waits require a matching caller stage budget |
| `RERANKER_MAX_QUEUE` | `3` | finite pending inference requests, 0..16 |
| `RERANKER_BODY_TIMEOUT_MS` | `500` | absolute body deadline, 1..1000 |
| `RERANKER_MAX_TOTAL_CHARS` | `262144` | raw query plus document characters before truncation, at most 1048576 |

An explicit `cuda`/`mps` that is not actually available fails the model load (so
`/health` stays 503) rather than silently serving on CPU; `auto` falls back.

## Apple Silicon (the M5 Max)

`RERANKER_DEVICE=mps` runs the cross-encoder on the Apple GPU via torch's MPS
backend (shipped in the default CPU wheel — no CUDA needed). This is the native,
path for local/offline use; throughput must be measured. The two-host deploy runs it on the RTX host
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
