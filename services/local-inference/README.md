# Native embeddings for the Apple search lane

This host-native service replaces emulated Linux TEI for the Apple text-search
lane. It keeps BGE-M3 resident and exposes the same `POST /embed` array-of-vectors
contract used by both `services/query` and `services/enrich`. The default model
is `BAAI/bge-m3`, 1,024 dimensions with L2 normalization. A different model,
revision or truncation recipe requires an explicit indexing/evaluation decision;
matching dimensions alone does not make two embedding spaces compatible.

`LOCAL_EMBED_DEVICE=auto` chooses available MPS, otherwise CPU. An explicit
`mps` fails readiness when MPS is unavailable. CPU thread count, encode batch
size, per-input/total text, request bytes and active connections are bounded.
Only one inference may run at a time. At most three additional validated requests
wait in FIFO order for up to 600 ms; full or expired admission returns 503.
The queue is finite and callers retain their own deadline and retry policy.
Disconnecting a caller does not preempt an already-running PyTorch kernel.

`LOCAL_EMBED_ADMISSION_WAIT_MS` accepts 0..1000 (default 600), and
`LOCAL_EMBED_MAX_QUEUE` accepts 0..16 (default 3 pending requests). Zero disables waiting.
Body reads finish before inference admission and have an absolute 500 ms limit
(`LOCAL_EMBED_BODY_TIMEOUT_MS`, 1..1000); byte progress does not reset this limit.
`LOCAL_EMBED_SOCKET_TIMEOUT_SECONDS` bounds other socket/idle work (default 5,
at most 30). Existing byte/text/batch and active-connection caps still apply.
An optional `X-Request-Timeout-Ms` header (1..30000) supplies the caller's remaining
stage budget and can only shorten body/admission deadlines. An expired queued
request does not begin inference. Inference itself cannot be preempted.

Readiness requires model load, dimension verification and a real warm-up encode.
`GET /health` and `/identity` report the actual device, model revision when
available, configuration fingerprint and warm-up time. Missing revision metadata
is explicitly unverified. `LOCAL_EMBED_REVISION` can pin an immutable HF commit.
Identity also samples actual MPS tensor/driver allocator counters after warm-up,
encode and identity reads. Current/largest sampled values exclude CPU RSS,
other processes and whole-system peaks; unavailable counters are null, not zero.
The HTTP identity includes separate `admission` depth, accepted/rejected counts
and sampled wait totals; these counters do not enter the model fingerprint.
`/embed` returns elapsed time, queue-wait time and fingerprint in HTTP headers;
no text or request targets are logged.
There is no text/result cache, provider key, agent generation or cloud inference.

See [the Apple run guide](../../docs/apple-silicon.md) for setup and isolated
ports. Starting an uncached model fetches weights from Hugging Face; it does not
send your input text to a remote inference provider. After models are cached,
set `HF_HUB_OFFLINE=1` for an offline run.

Tests require no model or runtime installation:

```sh
cd services/local-inference
PYTHONPATH=. python3 -m unittest discover -s tests -v
```

The tests exercise the real HTTP contract with a fake encoder, request bounds,
FIFO burst admission, queue capacity/expiry, absolute body deadlines,
busy/loading responses, accelerator selection and slot release after failure.
They do not establish BGE-M3 latency, search quality or MPS throughput.
