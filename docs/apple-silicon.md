# Standalone search on Apple Silicon

The Apple lane runs the complete text-search path on one Mac: host-native
SentenceTransformers BGE-M3 embeddings, an optional instance of the existing
native MPS reranker, and six isolated Docker services (Vespa, Redis, Keycloak,
query, gateway and web). The Linux TEI container and remote PC are absent. CLIP
is disabled explicitly for this mail/calendar/text lane. Connector sync, uploads,
media enrichment, purchasing tools and live provider credentials are outside
this search-only deployment. Use synthetic evaluation data, not copied mailbox
content, for reproducible evidence.

The base dev stack and the two-host Mac/PC deployment remain separate. The Apple
Compose file must be run alone. It creates its own volumes/network/project and
does not stop, resize or clear any other stack. The default project is
`asker-apple`; changing the profile keeps that project's index and therefore
requires the same model/dimension/recipe. Both profiles use BGE-M3, 1,024 text
dimensions and 8,192 maximum model tokens. This preserves the intended BGE-M3
space; exact TEI/native numerical parity is still an empirical check.

`QUERY_CACHE_NAMESPACE` supplies the operator's corpus/deployment version
prefix; change that prefix for a new corpus. Before Compose startup, the launcher
reads actual native `/identity` responses, validates ready/resident state and
64-digit hexadecimal fingerprints, then appends the encoder fingerprint and
optional reranker fingerprint (or `off`) to the prefix. It exports this binding
after verifying the reranker's actual model, pinned revision, device, precision,
batch size and maximum length against the requested profile. Reusing a process
with different reranker settings requires stopping that managed process first.
The binding is passed to Compose without printing the namespace. Model/revision/recipe overrides
therefore cannot reuse results from the earlier runtime identity. Invalid or
unready identity blocks startup rather than silently using an old namespace.
Runtime rank settings and resolved calendar scopes also enter the cache hash.
Both Apple profiles set `QUERY_RESULT_CACHE_ENABLED=false`, so API calls also
read the current index without a response-cache TTL. Other deployments retain
their existing 60-second cache policy unless configured otherwise. Connector
propagation and full source ACL revocation remain separate qualification gaps. Starting Compose
directly bypasses automatic identity binding; use the launcher for this policy.

| Profile | Native encode batch | Native CPU threads | Optional rerank batch | Planning macOS/app headroom |
| --- | ---: | ---: | ---: | ---: |
| `apple48` | 4 | 4 | 8 | at least 12 GB |
| `apple64` | 8 | 6 | 16 | at least 16 GB |

These are conservative configuration proposals. Headroom is planning guidance,
not an enforced reservation or measured residency. The six containers have a combined limit below 6 GiB,
including Vespa's 4 GiB ceiling; there must be at least 6 GiB of observed free
Docker capacity before `up`, excluding containers already owned by this Apple
project so that repeated startup does not charge its own footprint twice.
Native models use host unified memory outside the Docker VM. Preserve room for
macOS, other apps and model buffers. The initial shared 8 GiB Docker VM, with
other stacks consuming about 6 GiB, blocked full-stack startup. On October 1,
2026, the operator changed Docker Desktop's configured memory to 32 GB;
`docker info` then reported 33,598,169,088 bytes (about 31.29 GiB). All 16 prior
containers were recovered, and the six Apple containers became healthy. This
memory change was a human-controlled action because Docker Desktop can restart
unrelated containers. The launcher continues to check available room and never
changes Docker Desktop settings; `native-up` permits native-only verification.

## Setup and run

Use macOS arm64, Python 3.12 or 3.13 and `uv`. From the repository root:

```sh
tools/local/apple.sh setup apple48
tools/local/apple.sh up apple48
```

`setup` installs pinned native dependencies into
`services/local-inference/.venv`. `up` first builds the three app images serially,
so setup followed by up works on a fresh checkout. The separate `build` action
can prebuild these images without starting models or services. Then `up`
starts resident native models, waits for a real encoder warm-up, starts isolated
infra, deploys the existing Vespa package, then starts the search services. First
native startup may download model weights. It never silently substitutes a
smaller embedding model or a different dimension. Later startup can set
`HF_HUB_OFFLINE=1` after weights are cached.

Native processes run as checkout-scoped jobs in the logged-in user's `launchd`
domain. They survive the launching terminal and restart on unexpected process
exit. The generated job files remain in ignored lane state; no system daemon or
login item is installed. Run `native-up` again after a new login. `native-down`
removes those jobs, waits for owned processes to stop, and preserves weights and
indexed volumes. The six containers use `restart: unless-stopped`, so a Docker
restart can recover them. Changing a live profile requires `native-down` first;
the launcher validates model identity and admission settings before adoption.

| Surface | Default URL |
| --- | --- |
| Browser | `http://localhost:13002` |
| Authenticated gateway | `http://localhost:18080` |
| Local dev Keycloak | `http://localhost:18081` |
| Vespa query/feed | `http://127.0.0.1:18082` |
| Vespa config | `http://127.0.0.1:19072` |
| Native embeddings | `http://127.0.0.1:18083/identity` |
| Optional native reranker | `http://127.0.0.1:19900/health` |

Local dev users remain `alice`/`password123` and `bob`/`password123`. Generated
realm redirect origins and nginx proxy host use the isolated ports; the original
realm and nginx files are not rewritten. All state/logs/PIDs/generated config are
inside ignored `.venv/asker-state`. Do not commit private environment files.

For changes, copy the chosen example into an ignored `.env.local` file and set
`ASKER_APPLE_ENV_FILE` to its absolute path. This is trusted shell configuration.
Enable optional reranking with `ASKER_APPLE_RERANK_ENABLED=true`, then restart
only this lane. The existing reranker package is reused without a second model
implementation. Its MPS path currently uses fp32; accelerator/precision speed
claims require measurement. A caller must also request `rerank=1`.

The observed `apple64` startup on October 1, 2026 used a private
`.env.apple64-rerank.local` override for the opt-in comparison. Native embeddings
and the reranker both reached ready/resident state on MPS with fp32 parameters.
The committed `apple48` and `apple64` profiles still disable reranking by
default. Healthy startup establishes availability; end-to-end latency and
relevance qualification remain separate checks.

The [October 4 validation report](search-quality-validation-2026-10-04.md)
records the current run: actual M5 Pro 64 GiB, Docker configured at 32 GB, all
six services healthy, both native models resident on MPS, and normal warm
uncached browser p95 67.2 ms over 104 attempts. Short indexing contention and
normal hybrid at C2/C4 complete without fallback. The first untouched v2 holdout
and all six 10,000-document capacity quality gates fail, so the browser keeps
reranking off (`ASKER_APPLE_RERANK_DEFAULT=0`). Backend opt-in capability for
ablation does not enable the browser default. Actual 48 GB hardware remains
unverified. Startup with cached weights took 34.008 seconds separately from
warm query latency; final sampled memory shows zero swap growth without an
exact whole-application peak claim.

The native listeners bind loopback. Containers address them through Docker
Desktop's `host.docker.internal`. Verify this host reachability on your Docker
Desktop installation before claiming working semantic search. Do not expose
unauthenticated model endpoints to a LAN to work around networking problems.

```sh
tools/local/apple.sh status apple48
tools/local/apple.sh native-up apple48   # actual native inference, without Docker startup
tools/local/apple.sh config apple48      # resolve config, no service startup
tools/local/apple.sh down apple48        # only this project/processes; retain volumes
```

Changing an embedding revision, normalization or truncation recipe requires a
new explicit corpus qualification and possibly a reindex. `/identity` reports
the requested/resolved revision and fingerprint. Pin the resolved immutable
revision in the private profile before freezing an experiment.

## Qualification

The warm normal-search target is p95 at most 5 seconds, measured from browser
submission to usable authorized results/clarification, for fresh queries at one
interactive request. `QUERY_SEARCH_TIMEOUT=4.5s` leaves transport/rendering room;
embedding has a 1.5s stage bound. The October 4 profiles give optional reranking
a 3.5s stage bound within that same total deadline. Native admission retains one active inference
and at most three FIFO waiters: embedding waits at most 600ms and the optional
reranker at most 2500ms, shortened by the caller's remaining stage budget.
Bodies have an absolute 500ms read bound. The browser cancels superseded work
and stops stalled searches after 5s, including token acquisition and decoding.
A timeout or keyword fallback is not a completed semantic pipeline. Existing contractual
p90 checks remain separate. No first-token/streaming or whole-response cache
shortcut qualifies the target.

Record startup and first-request times separately from warm uncached and cached
results. Run representative mail/calendar queries and hard negatives, exact
identifiers, multilingual requests, temporal ambiguity, tenant isolation and
held-out task relevance. Capture p50/p95/p99, failures, degradation, cache state,
actual model/device/revision, peak whole-system memory, pressure/swap delta and
browser timing. Include sustained queries while background indexing is active,
with indexing concurrency throttled, to expose contention. The native model
service has one inference slot and a finite FIFO wait before rejecting overload; it
does not preempt an active encode after a disconnected HTTP caller.

The existing k6 suite measures rare-token workload and p90, not representative
semantic or browser quality. `docs/loadtest-report.md` contains a template and
illustrative sample numbers, not an M5 Pro 48/64 GB result. Contract tests with
fake models establish wire/admission behavior only. Report available-hardware
measurements and unavailable-profile estimates separately. Enable a new default
only after relevance, correctness and latency gates pass; preserve the earlier
default when any gate fails.

Run the read-only memory sampler in a separate terminal during HTTP or browser
evaluation. It reuses the native benchmark's system snapshot helpers, scopes
Docker stats to this project, finds model listener PIDs without recording command
lines, and reads only loopback model identities. No query text, tokens, environment
or provider responses are logged. Output is JSONL inside ignored
`services/local-inference/.venv/asker-state`; Ctrl-C writes a sampled summary.
The JSONL file contains one `start` record, individual `sample` records and a
final `summary` record, distinguished by `kind`. Parse each line as JSON.

```sh
services/local-inference/.venv/bin/python tools/local/sample_apple_memory.py \
  --duration 300 --interval 5 --rerank-url http://127.0.0.1:19900
```

Omit `--rerank-url` when that process is disabled. Model RSS excludes unaccounted
Metal allocations; encoder identity exposes actual MPS allocator readings.
Both model identities now expose sampled allocator counters, with unavailable
values left null. These counters overlap with
process/system accounting and must not be added to produce a memory total.
The summary records sampled maxima and swap growth, not an allocation peak or a
search qualification. `memory_pressure`, `vm_stat` and swap include other apps;
compare before/after samples rather than assigning pre-existing swap to Asker.

The local `memory-20261001T223925Z.jsonl` run recorded 37 samples over 181.627
seconds. Its final `summary` reported these sampled maxima:

| Reading | Observed value |
| --- | ---: |
| Apple project container memory sum | 2,986,197,645 bytes |
| Embedding process RSS | 546,016 KiB |
| Embedding MPS driver allocations | 3,182,100,480 bytes |
| Reranker process RSS | 520,688 KiB |
| System swap growth during sampling | 0 MiB |

These readings retain the scopes above; reranker MPS allocations were unavailable
in its identity. The summary's `searchQualified` is `false` because memory
sampling does not establish the search response-time target.
