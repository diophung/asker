# Carol capacity probe

`capacity.py` prepares 10,000 reproducible short synthetic EMAIL, FILE and
CHAT_MESSAGE documents and 100 distinct hybrid search tasks. Every document has
a unique text and an `asker-capacity-v1-` ID. Actual embeddings come from native
BGE-M3 revision `5617a9f61b028005a4858fdac845db406aefb181`, MPS/fp32, 1024 dimensions,
L2 normalization and 8192 maximum tokens. Embedding HTTP batches contain eight
texts; the served model's actual batch size is recorded. No random vectors or
shared topic vectors are substituted.

This is a capacity experiment, not a judged relevance benchmark or Google parity
claim. Forty tasks have constructed identifier lookups; sixty topic/paraphrase,
source-filter and project-context tasks are unjudged. HTTP measurements exclude
browser rendering. Direct Vespa feeding bypasses connectors, ingest and full ACL
qualification. The generator reads no development, regression or holdout files.

Preparation is offline:

```sh
services/local-inference/.venv/bin/python tools/local/capacity.py prepare
```

HTTP actions require a bearer token file with mode 0600 and Carol's exact verified
tenant from `GET /v1/me`. The gateway must verify `carol@example.com`, and the
subject must equal the tenant, proving this is the dedicated personal tenant.
Endpoints are explicit-port loopback origins; proxy routing and redirects are
disabled. Token values and returned titles/snippets/foreign IDs are never printed
or recorded. Artifacts stay inside ignored `.venv/asker-state` with private
permissions. Token creation remains an operator action; the tool does not log in.

After other benchmarks finish, use the same arguments for every stage:

```sh
capacity_args=(--tenant 'CAROL_VERIFIED_TENANT' \
  --token-file services/local-inference/.venv/asker-state/carol.token)

# Actual 32-document probe, no feed. The report estimates only bulk embedding time.
services/local-inference/.venv/bin/python tools/local/capacity.py calibrate "${capacity_args[@]}"

# Embed every unique text and validate all vectors before enabling any upsert.
services/local-inference/.venv/bin/python tools/local/capacity.py embed "${capacity_args[@]}"

# Explicit writes, at most 8 simultaneous upserts, only generated IDs in Carol's group.
services/local-inference/.venv/bin/python tools/local/capacity.py feed --write "${capacity_args[@]}"

# 100 tasks, normal hybrid, no cache/retries; optional --concurrency 2 or 4.
services/local-inference/.venv/bin/python tools/local/capacity.py run "${capacity_args[@]}"
```

Add `--count 1000` to every stage for the smaller experiment if 10,000 embeddings
take too long. IDs/texts for the first 1000 are identical across counts. After
feeding 10,000, a later 1000-document feed leaves the remaining documents intact;
it does not create a 1000-document corpus. Other documents are never removed or
updated. Use one seed/corpus at a time in Carol's tenant; foreign returned IDs
fail the capacity gate without being logged.

`--rerank` requests the optional cross-encoder separately. Runs retain every
attempt's latency, cache/execution provenance and failure labels. Cached,
degraded, empty, foreign-ID or missing constructed-lookup responses fail the
HTTP capacity gate. Reports include p50/p95/p99 over all attempts and require
p95 at most 5000 ms with no failures. The feed report must match the exact corpus,
verified tenant, document count and actual model fingerprint. These results remain separate from the judged
quality harness and browser qualification. No services are started, stopped or
resized by this tool.

Tests require only Python and use generated synthetic data and fake boundaries:

```sh
services/local-inference/.venv/bin/python -m unittest discover \
  -s tools/local -p 'test_capacity.py' -v
```
