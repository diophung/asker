# Search quality and M5 Pro validation — 2026-10-01

Asker is running at **http://localhost:13002** with the new Docker allocation.
All six Apple-lane containers are healthy, with zero restarts or OOM kills.
Login, real indexing/search, source labels and browser rendering were verified.
Docker is configured for 32 GB and reports 33,598,169,088 bytes (~31.29 GiB);
all 16 previously running containers were recovered after its restart.

The warm, uncached **one-request path meets the 5-second latency target on this
36-document synthetic corpus**. This is not Gmail/Calendar parity or a
mailbox-scale qualification. Reranking remains off in the committed defaults:
it fails the strict strongest-baseline relevance gate. The running private
profile makes it available only when a request explicitly opts in.

## Diagnosis and implementation

Asker is personal mail/calendar/files/chat/media search. Its actual path is
React → OIDC gateway → Go query service → tenant-scoped Vespa streaming search,
with Redis and text embeddings. The original Mac deployment needs a separate
PC, while the ordinary dev deployment emulates a Linux embedding container.
The new Apple lane retains Vespa and runs resident inference natively on MPS.

| Finding | Correction | Source |
| --- | --- | --- |
| Broad intent words changed ordinary content searches into schedules | Narrow intent rules; explicit source/date controls take precedence | [intent](../services/query/intent.go), [scope](../services/query/scope.go) |
| Dense retrieval ignored quoted phrases and exclusions | Hard typed clauses on every arm; standalone letter-prefix identifiers are exact constraints | [understanding](../services/query/understand.go), [YQL](../services/query/vespa.go) |
| Calendar dates used creation time or missed ongoing events | Occurrence-time half-open overlap; history stays searchable; live Vespa NOT(EQ) syntax verified | [scope](../services/query/scope.go), [calendar ranking](../services/query/personalize.go) |
| Fusion/reranking depended on personalization and truncated later pages | Independent RRF/opt-in reranking; preserve candidate tail on a common ordinal scale | [pipeline](../services/query/server.go), [reranking](../services/query/rerank_model.go) |
| Serial independent calls and no complete-query budget | Parallel embedding/retrieval; 4.5 s query and 4.8 s gateway budgets; context-aware Redis | [query](../services/query/server.go), [gateway](../services/gateway/routes.go) |
| Obsolete browser work continued and response metadata became stale | Cancellation and request guards; snapshot accepted metadata; render-frame completion telemetry | [browser](../web/src/v2/SearchApp.tsx) |
| Browser retries could reuse stale results; unknown connectors were labeled Drive | Browser searches bypass response caches; truthful generic provenance labels | [transport](../web/src/v2/backend.ts) |
| Failures/cache/no-op reranking distorted evaluation | Complete-body timing, all attempts in denominators, hard execution/constraint gates | [evaluation](../tools/eval/README.md) |
| Native residency/device/model identity was unverified | Pinned resident encoder/reranker, actual MPS identities, bounded admission and diagnostics | [native service](../services/local-inference/README.md) |

The first live development run exposed a calendar YQL `!=` parse error. Its
failed report remains in `apple64-dev-baselines/`; the correction uses negated
equality and was verified against real Vespa in keyword/hybrid/vector arms.
Standalone `AX-48271` now returns only its matching document, rather than a
correct first hit followed by unrelated vector candidates. Ordinary prose and
standalone dates do not become identifier constraints.

## Hardware, models and frozen configuration

- Apple M5 Pro, Mac17,8, **64 GiB unified memory**, macOS arm64.
- Python 3.12.12, SentenceTransformers 5.1.0, PyTorch 2.13.0, NumPy 1.26.4.
- Encoder `BAAI/bge-m3@5617a9f61b028005a4858fdac845db406aefb181`:
  actual `mps:0`/`torch.float32`, 1,024 dimensions, L2, 8,192 tokens, batch 8,
  six CPU threads, resident, one inference slot.
- Reranker `BAAI/bge-reranker-v2-m3@953dc6f6f85a1b2dbfca4c34a2796e7dde08d41e`:
  actual MPS/fp32, 512 tokens, batch 16, resident, one inference slot.
- Query candidate cap 100; rerank depth 30; passage cap 1,024 characters;
  embedding budget 1.5 s; rerank budget 1 s. No generative model on ordinary search.
- Six Linux/arm64 search containers; named project `asker-apple`; loopback ports
  18080/18081/18082/19072/13002; native model ports 18083/19900.

Before holdout, 299 source/config files, the private opt-in profile, model
fingerprints and unchanged acceptance criteria were hashed into local
`qualification-freeze-20261001.json`. Source-manifest SHA-256:
`56dd5c37329a12614215287b4319163db8e672b206d3d44f7c67394aa1565a0b`.
Corpus manifest SHA-256:
`7bae734f310030e7621d71ae074ac39f02f76c3ca1ead0d9a1acb7c9515f732f`.
Holdout results were not used to tune code, models, labels or thresholds.

## Before/after development relevance

The corpus has 36 synthetic documents and 13 judged tasks per split.
**104 requests per mode mean 13 tasks repeated eight times**, not 104 independent
tasks. Eleven tasks have positive relevance; two test no-match behavior.
Engineering labels have not been independently adjudicated. Repeated project
variants limit benchmark discrimination and statistical independence.

Original HEAD `e08882de6e9b5c2358d3f75d81b2c7862a8ed9e4` was archived and built
without altering the working tree. Original query/gateway ran natively with
Go 1.26.4 against the same pinned native encoder, Vespa and corpus. An isolated
Redis allowed reads but rejected cache writes; observed cache hits were zero.
Original hybrid always attempted unavailable CLIP and disclosed degradation.
Current query/gateway run in Docker. Thus this is a controlled source comparison
on the new inference setup, not a recreation of the old emulated-TEI deployment
or a controlled compiler/transport speedup experiment. Temporary baseline
processes and Redis were stopped and removed.

| Mode | Original nDCG@10 | Changed nDCG@10 | Original tasks | Changed tasks | Original p95 | Changed p95 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Lexical | 0.7663 | 0.8572 | 12/13 | 13/13 | 15.062 ms | 6.975 ms |
| Dense | 0.8228 | 0.9927 | 9/13 | 13/13 | 35.273 ms | 26.707 ms |
| Hybrid | 0.8586 | 0.9495 | 9/13 | 13/13 | 34.891 ms | 24.105 ms |
| Hybrid + rerank | Unmeasured | 0.9967 | Unmeasured | 13/13 | Unmeasured | 278.792 ms |

All changed modes completed 104/104 with zero errors, cache hits, degradation or
forbidden hits. Exact success@1 was 2/2 unique cases; all-needs coverage was 1/1
explicit two-need task. Original dense/hybrid each exposed forbidden records on
two exclusion tasks (16 repeated violations); changed modes exposed none.
Original hybrid had 104/104 CLIP degradation, so its timing does not qualify
requested-path completion.

Final Recall@10 was 1.000 over the judged positives in every changed mode.
A supplementary `k=50`, one-repeat dev run also measured final Recall@50 1.000
in all modes, with no errors/degradation. It is not a latency gate (13 samples).
**Candidate recall before reranking remains unmeasured**; final recall cannot
establish the candidates supplied to a model or relevance of unjudged records.

Dense is the strongest overall development ranking baseline. Reranking's
semantic slice is **0.96394 versus dense 1.000**, so its strict gate fails despite
higher overall nDCG. No threshold was relaxed and no new default was enabled.
The existing hybrid default remains; it is not claimed to be the strongest lane.

## Regression and untouched holdout

All four modes completed 104/104 requests on each split, with no errors, cache
hits, degradation or forbidden hits; task/exact/need outcomes were all successful.
No-match requests correctly skipped inference: reranking actually executed on
88 nonempty responses out of 104 requests, with zero unfulfilled requests.

| Split | Lexical nDCG@10 | Dense | Hybrid | Hybrid + rerank | Hybrid p95 | Rerank p95 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Regression | 0.8572 | 0.9721 | 0.9632 | 0.9967 | 26.468 ms | 281.333 ms |
| Holdout | 0.8572 | 1.0000 | 0.9632 | 1.0000 | 24.688 ms | 279.580 ms |

Regression fails the same semantic-slice no-regression gate. Holdout fails
strict improvement because dense and rerank both saturate at 1.000. The first
reports are preserved. A new, independently reviewed, more discriminating
benchmark is needed; equality here does not establish improvement.

## Complete response and browser latency

The API harness measures through complete body read and JSON decode, including
errors. Browser timing additionally includes token access, network, decoding,
React results and the usable result animation frame. It starts in the submission
handler; it is not a photon/OS compositor timing measurement. Login/startup and
expired-session reauthentication are separate.

| Warm uncached probe | Samples | p50 | p95 | p99 |
| --- | ---: | ---: | ---: | ---: |
| Sustained hybrid, c1 | 520 | 22.259 ms | 27.022 ms | 29.189 ms |
| Sustained opt-in rerank, c1 | 520 | 52.392 ms | 280.378 ms | 292.278 ms |
| Browser normal hybrid, c1 | 104 | 34.200 ms | 49.100 ms | 107.600 ms |

Sustained API execution had zero errors, cache hits, fallback or violations.
The browser repeated eight development query texts 13 times, including exact,
semantic, Spanish, typo, multi-need and an absent quoted identifier. All rendered;
no fallback notices appeared, and every absent-identifier result was empty.
This browser probe does not cover every structured-filter/source-tab workflow.

Browser requests send `Cache-Control: no-cache`; the gateway bypasses lookup
and insertion. After the repeated browser identifier search, an ordinary API
control for the same request returned `cached=false`, then `cached=true` on its
second call, confirming the cached and uncached paths remain separate. The
private browser session expired during the long verification session; explicit
sign-out/sign-in restored it. Idle-expiry UX remains an area to improve.

The first opt-in semantic rerank request after model startup took 1,403.229 ms
and fell back when the 1-second rerank stage expired. Its immediate successor
returned a busy fallback in 140.711 ms. These are preserved separately from the
warm runs. Model-ready warmup does not precompile every candidate/batch shape;
complete application cold-start time was not rigorously measured.

| Normal hybrid load | Requests | HTTP errors | Fallback | p95 |
| --- | ---: | ---: | ---: | ---: |
| Concurrency 2 | 312 | 0 | 216 | 23.226 ms |
| Concurrency 4 | 312 | 0 | 273 | 28.195 ms |

The encoder's single slot rejects excess work with 503 rather than queuing it
without bounds. Those responses fall back to lexical search; low latency does
not qualify concurrent semantic execution. Both load gates fail. Background
indexing contention and cancellation of already-started MPS work are unqualified.

## Memory and operational checks

During the 152.083-second final sampler run (31 samples), Apple containers reached
3,120,058,856 bytes (~2.91 GiB). Encoder RSS reached 184,736 KiB, encoder sampled
MPS tensor allocation 2,274,306,304 bytes, and driver allocation 3,182,116,864 bytes.
Reranker RSS reached 236,000 KiB; its allocator counters are unavailable.
**Do not sum RSS and driver counters**: accounting overlaps and these are partial
sampled measures. Total application peak/VM overhead and original-stack memory
are not established by these counters. System swap growth was 0 MiB; the system
already had about 6.7 GiB swapped. Memory-pressure free percentage ranged 42–54%.
Other desktop apps and a web rebuild were present, so system readings are not
causal attribution to Asker. No Apple container OOM or restart was observed.

An earlier 181.627-second full-stack sampler reached 2,986,197,645 container
bytes and also observed zero swap growth. Earlier native-only probes measured
104/104 MPS embedding requests at p95 26.420 ms, CPU six-thread comparison at
p95 52.061 ms, and 6,000/6,000 sustained MPS embeddings at p95 24.774 ms. These
are stage-only measurements, not complete search. Native-only c2/c4 overload
probes returned busy 503s and do not qualify concurrent inference.

Read-only boundary checks: no token → HTTP401; Alice finds her identifier; Bob
finds no Alice record even with forged tenant headers. This tests tenant routing,
not full per-source ACL revocation. A temporary synthetic record became visible,
updated and deleted through uncached searches in single-digit milliseconds;
cleanup retained the 36-document fixture. A cached pre-addition miss stayed
stale, showing the API's 60-second cache limitation. Ordinary browser search now
bypasses it. Connector ingestion and permission/deletion invalidation remain
unqualified.

Go race/vet/build/lint across the repository passed; final query race/vet/scoped
lint passed after live-YQL corrections. Web build/lint and **133 tests** passed.
Native embedding contract tests (14), reranker tests (49), seeder boundary tests
(7), memory sampler tests (5), Ruff, ShellCheck, Compose resolution and diff checks
passed. This evidence is local, not CI or production validation.

## Reproduce and inspect evidence

See the [Apple launch guide](apple-silicon.md) and
[evaluation guide](../tools/eval/README.md). Default local login is the dev account
`alice` / `password123`; use only this isolated non-production lane.

```sh
make apple-setup APPLE_PROFILE=apple64
make apple-up APPLE_PROFILE=apple64
# http://localhost:13002

# Opt-in capability profile used for comparison (defaults remain off):
ASKER_APPLE_ENV_FILE="$PWD/services/local-inference/.venv/asker-state/.env.apple64-rerank.local" \
  tools/local/apple.sh up apple64

services/local-inference/.venv/bin/python tools/local/sample_apple_memory.py \
  --duration 150 --interval 5 --rerank-url http://127.0.0.1:19900
```

The evaluation README gives token-safe seeding and full commands. Local raw
reports are ignored, not committed:

- `tools/eval/reports/apple64-original-head-dev/`: controlled starting revision.
- `apple64-dev-baselines/`: initial live failure retained.
- `apple64-dev-final/`, `apple64-regression/`, `apple64-holdout/`: frozen comparisons.
- `apple64-dev-recall50/`, `apple64-sustained-c1/`, `apple64-c2/`, `apple64-c4/`.
- `services/local-inference/.venv/asker-state/`: native stage probes, sampled
  memory JSONL, browser raw timing JSON, first rerank requests, freeze hashes,
  baseline binary manifest, startup logs and `search-verification.jpg`.

## Remaining targets

48 GiB profiles exist but actual 48 GiB hardware is unavailable; a constrained
64 GiB run would not qualify it. Neither profile meets every quality/operational
release gate. No Google comparison, independent relevance adjudication,
mailbox-scale index or continuous media/connector stack was run.

Natural-language intent/date parsing is chiefly English. `from:` still matches
participants, not a dedicated sender role. Unsupported syntax is disclosed.
No conversational refinement planner, clarification flow, guaranteed per-need
candidate allocation, location/numeric constraint system or full source-ACL
revocation mechanism was added. These limits must stay visible when evaluating
top-notch personal search beyond the small engineering fixture.
