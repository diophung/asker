# Search quality and M5 Pro validation — 2026-10-04

Asker is running at **http://localhost:13002** on an M5 Pro with 64 GiB unified
memory and Docker configured for 32 GB. All six containers are healthy and both
pinned native models are resident on MPS. Normal warm uncached browser search
completed with **p95 67.2 ms over 104 attempts**. Bounded inference queues removed
the observed embedding errors and keyword fallback at concurrency 2/4.

Search quality remains below the acceptance contract. The final conservative
correction passes development but **fails frozen regression and the first
untouched holdout against dense**; the separate 10,000-document probe also finds
lookup-ranking misses. Optional reranking remains disabled by default; ordinary
search retains hybrid without reranking. These measurements establish the
recorded local execution and latency, not Gmail/Calendar parity, representative
mailbox relevance, or 48 GB hardware performance.

This report uses development reports and aggregates/measurements from the frozen
regression and first holdout reports; it does not inspect their query or evidence
label files. Reports generated on October 5 UTC
belong to the October 4 America/Los_Angeles session. Earlier v1 holdout evidence
was consumed on October 1; all v1 splits are now regression evidence. V2 has
**147 synthetic documents, 38 development tasks, 39 regression tasks, and 40
holdout tasks**, with nine slices and complete-body evidence. The holdout stayed
sealed until the source/configuration freeze and its first recorded run. Labels are
engineering judgments; independent adjudication is pending. Repetition adds
HTTP samples, not independently judged tasks. A relevant source ID does not
prove that the returned preview or reranker input contains its decisive evidence.

The [sanitized aggregate snapshots](evidence/apple-search-2026-10-04.json) and
[published freeze record](evidence/apple-search-freeze-2026-10-04.json) preserve
reviewable evidence without private runtime state. These October 4 measurements
precede publication onto October 5 `origin/main`; the publication review preserves
main's URL safety and security headers and runs checks on the merged source.
Those checks do not requalify the historical browser/model timings.

## Diagnosis and implemented corrections

These source changes explain the intended behavior; the development comparison
does not isolate each change's causal contribution.

| Observed risk | Correction and source | Boundary |
|---|---|---|
| Coarse diversity scoring could demote distinct answers sharing a sender or document type. | [MMR ranking](../services/query/personalize.go) now measures title/snippet token overlap; sender/type can strengthen actual overlap. | Content overlap is a heuristic, not a guarantee that every requested need is covered. |
| Short result-card previews could discard the evidence useful to the cross-encoder. | [Vespa summaries](../services/query/vespa.go) retain separate authorized passages; [rerank input](../services/query/rerank_model.go) prioritizes highlighted body/chunk text and applies a configured input cap. | Richer bounded passages still need not include every decisive part of a long body. |
| Time words embedded in names or quoted titles could become unintended date filters. | [Temporal parsing](../services/query/temporal.go) masks quoted text, checks Unicode word boundaries and preserves UTF-8 offsets; explicit request bounds retain precedence. | Supported phrases remain a bounded parser vocabulary. |
| Multiple misspellings could hurt model matching even when retrieval found the right documents. | [Model query correction](../services/query/rerank_query.go), enabled only for unconstrained soft queries by [query execution](../services/query/server.go), uses unambiguous one-edit words supported by at least two distinct authorized head titles. A third correction requires an unchanged query term anchoring the same head. | It changes only cross-encoder input. Retrieval, hard predicates and displayed input remain literal; proper names, IDs, quoted text and constrained queries are protected. |
| Immediate native-service busy responses caused dense errors and hybrid fallback under modest concurrency. | Bounded FIFO admission in [embedding](../services/local-inference/local_inference/admission.py) and [reranking](../services/reranker/reranker/admission.py) allows brief waiting and removes expired tickets before inference starts. Caller timeout headers only shorten local budgets. | Already-started MPS work is not preempted; finite queues cannot guarantee sustained capacity. |
| Native processes tied to an invoking shell could disappear when that shell ended. | [Checkout-scoped launchd jobs](../tools/local/native_service.py) and the [Apple launcher](../tools/local/apple.sh) persist across terminal exits, restart unexpected failures and validate ownership before stopping jobs. | These are user login-session jobs; a new login still requires `native-up`. |
| A cached miss could outlive a direct index update. | Apple [64 GB](../deploy/compose/.env.apple64.example) and [48 GB](../deploy/compose/.env.apple48.example) profiles disable query response caching; [browser searches](../web/src/v2/backend.ts) send `Cache-Control: no-cache`, with [gateway propagation](../services/gateway/search.go). | This removes that response-cache delay; it does not validate ingestion freshness or source ACL revocation. |
| Expired sessions and long searches could leave the UI appearing signed in or loading indefinitely. | [Authenticated fetch](../web/src/v2/backend.ts) invalidates the matching session on 401; [auth](../web/src/v2/auth.ts) owns refresh and rejects stale completion; [search UI](../web/src/v2/SearchApp.tsx) clears stale results and provides sign-in/retry recovery with a five-second browser deadline. | Browser timing includes token refresh and decode. Aborting at the deadline does not prove successful usable results within it. |

## Unchanged acceptance contract

Qualification requires k=10, limit=20, uncached requests, at least 100 latency
samples, and concurrency 1. The candidate must achieve p95 ≤5,000 ms, task
success ≥90%, exact success@1 ≥98%, and all-needs coverage ≥95%. It must strictly
improve overall nDCG against the frozen baseline, with no per-slice nDCG or task
regression. Both compared pipelines must complete without errors, cache hits,
degradation, constraint violations, or missing/unfulfilled requested reranking.
Equality is not improvement. V2 rejects altered criteria or task lists; only the
seeded tenant token label may change. C2/c4 are separate load observations.

V2 manifest SHA-256:
`8966dd3d0f7317e6b3140cd1763ef812799c1eecfd8f8e0db3eda176560f07aa`.

## Measured development before/after

The before run used October 1 application images; the intermediate after run
used the October 4 changes, bounded inference waiting and candidate telemetry.
Both record M5 Pro Mac17,8, 64 GiB unified memory, Docker 32 GB, and pinned
BGE-M3/reranker models running natively on real MPS in fp32. These are measured
comparisons, **not a causally controlled timing experiment**: application and
admission settings changed, debug response overhead was added, runs occurred at
different times, and v2 repeats changed from three to eight. API latency covers
complete body read and decode; browser rendering and token access are separate.

| V2 mode | Before nDCG@10 | Intermediate after | Task success before → after | All-needs before → after | API p95 before → after |
|---|---:|---:|---:|---:|---:|
| Lexical | 0.7820 | 0.7809 | 84.21% → 84.21% | 100% → 100% | 8.906 → 15.245 ms |
| Dense | 0.8843 | 0.8872 | 94.74% → 94.74% | 66.67% → 66.67% | 52.582 → 46.301 ms |
| Hybrid | 0.8398 | 0.8671 | 92.11% → 97.37% | 66.67% → 100% | 72.466 → 44.657 ms |
| Hybrid + rerank | 0.8513 | 0.8909 | 89.47% → 97.37% | 33.33% → 100% | 353.059 → 844.928 ms |

Before: 38 tasks ×3 =114 requests per mode. Intermediate after: 38×8 =304.
Both reports show zero errors, cache hits, degradation and forbidden hits.
All six exact-item tasks succeeded at rank 1. The intermediate reranker improves
overall nDCG but regresses the typo slice: **0.4623 versus dense 0.6872**, so its
strict gate fails. Reranking remains a candidate capability, not a qualified
new default on this intermediate evidence. The final correction is reported
separately below.

On the smaller v1 development set, intermediate rerank nDCG rises from 0.9632
to 1.0000 against dense 0.9888, passing that development comparison. Each mode
completed 104 requests over 13 tasks with zero errors/fallback. This saturated,
previously used fixture cannot substitute for fresh v2 qualification.

The intermediate v2 report measures pre-rerank **head** recall 0.9722 on all
288 positive samples with complete head telemetry; 16 no-match samples are
excluded. Reranking actually executed on 280/304 requests; complete empty heads
are measured zero recall on positive tasks and require no model call. Head
population/count and depth describe the authorized input before optional
inference. Full retrieval-population recall remains unmeasured. Absent, partial
or malformed telemetry cannot be inferred from final results.

## Frozen development, regression and first holdout results

All four modes run each task three times at concurrency 1 under the unchanged
contract. Development has **114 requests per mode over 38 distinct tasks**;
regression has **117 over 39**; first holdout has **120 over 40**. Task and needs
counts below use distinct tasks whose repeats agree, rather than treating each
HTTP request as an independently judged case. nDCG uses 108 repeated positive
measurements over 36 distinct tasks in each split; two/three/four no-match tasks
respectively are excluded from it.

| Split | Mode | nDCG@10 | Task success | All-needs | API p95 | API p99 |
|---|---|---:|---:|---:|---:|---:|
| Development | Lexical | 0.79480 | 33/38 | 3/3 | 11.269 ms | 11.914 ms |
| Development | Dense | 0.90109 | 37/38 | 2/3 | 43.707 ms | 55.051 ms |
| Development | Hybrid | 0.88458 | 38/38 | 3/3 | 44.321 ms | 51.119 ms |
| Development | Hybrid + rerank | 0.93745 | 38/38 | 3/3 | 810.063 ms | 848.073 ms |
| Regression | Lexical | 0.77986 | 33/39 | 3/3 | 11.053 ms | 11.991 ms |
| Regression | Dense | 0.87430 | 39/39 | 3/3 | 42.480 ms | 115.886 ms |
| Regression | Hybrid | 0.83844 | 38/39 | 3/3 | 47.196 ms | 49.564 ms |
| Regression | Hybrid + rerank | 0.88673 | 39/39 | 3/3 | 853.046 ms | 899.501 ms |
| First holdout | Lexical | 0.77437 | 34/40 | 3/3 | 11.033 ms | 14.204 ms |
| First holdout | Dense | 0.89785 | 40/40 | 3/3 | 44.131 ms | 48.144 ms |
| First holdout | Hybrid | 0.84378 | 40/40 | 3/3 | 44.896 ms | 51.454 ms |
| First holdout | Hybrid + rerank | 0.88239 | 40/40 | 3/3 | 792.401 ms | 859.093 ms |

Every mode records zero errors, cache hits, degradation, missing execution
telemetry and constraint violations. Exact success@1 is **6/6 distinct tasks**
for every mode in each split (18 repeated samples). All-needs uses **three
distinct tasks** (nine samples). These small, curated engineering judgments do
not establish population rates of 98% exact success or 95% needs coverage.

The frozen comparison retains dense, selected on development as the strongest
non-candidate baseline. Development passes, but the strict no-regression rule
rejects both subsequent splits:

| Split / failing metric | Dense | Hybrid + rerank | Gate |
|---|---:|---:|---|
| Development overall nDCG | 0.901086742 | 0.937452625 | PASS; no slice/task regression |
| Regression combined-needs nDCG | 0.852914031 | 0.852620926 | FAIL; even this small decline violates the frozen rule |
| Regression typo nDCG | 0.753953169 | 0.666666667 | FAIL |
| First holdout combined-needs nDCG | 0.908371256 | 0.840982752 | FAIL |
| First holdout typo nDCG | 0.876976585 | 0.500000000 | FAIL |
| First holdout overall nDCG | 0.897854841 | 0.882392635 | FAIL |

Combined-needs and typo slices each contain three distinct positive tasks, nine
repeated samples. Finding the required documents within k=10 can still succeed
while ordering them worse: the candidate's 100% task/needs success does not erase
these ranking failures. Default hybrid without reranking is retained; this does
not claim that hybrid is the strongest relevance lane in these reports. No code,
thresholds or labels were retuned after viewing regression or first holdout.

Reranking executes on **108/114 development, 108/117 regression and 108/120
holdout requests**. The remaining six/nine/twelve no-match samples have empty
heads and need no inference; unfulfilled requested reranking is zero. Each split
has complete authorized pre-rerank head recall **1.0000 on 108/108 positive
samples, representing 36/36 distinct positive tasks**; none has unknown head
telemetry. This remains head recall, not retrieval-population recall@50.

The operator's [freeze record](evidence/apple-search-freeze-2026-10-04.json) records 213 runtime
source files and the pinned models, active configuration and unchanged criteria.
Its SHA-256 is
`0b262d38aed4ae31dff88153cfbac6aab347b3e15ec0394d2fe5c3d1399a8bc4`.
The report records real MPS/fp32 on the 64 GiB machine, native embedding queue
3/wait 600 ms, reranker queue 3/wait 2,500 ms, candidate cap 100, model head 30,
input cap 1,024 characters, stage budget 3.5 seconds and whole-request budget
4.5 seconds. It also records an isolated 10,000-document capacity tenant indexed
on the shared stack; these runs do not establish that tenant's retrieval quality
or sustained load performance. The first holdout result is preserved as a failure
and is now consumed evidence; future tuned changes require a fresh sealed set.

## Concurrent execution

The October 4 load probes repeat the same 13 v1 development tasks eight times
per mode. The old short paths were often errors or fallback, so their lower
latency does not qualify semantic execution.

| Load | Old dense errors /104 | Old hybrid fallback /104 | New dense errors /104 | New hybrid fallback /104 | Hybrid p95 old → new |
|---|---:|---:|---:|---:|---:|
| C2 | 78 | 64 | 0 | 0 | 31.216 → 41.700 ms |
| C4 | 97 | 88 | 0 | 0 | 31.553 → 75.928 ms |

New dense and hybrid completed every request without cache hits, constraint
violations or degradation. Their load comparison still fails the relevance
criteria: hybrid typo nDCG is 0.6309 versus dense 1.0000, and its overall nDCG is
0.9664 versus 0.9888. Zero fallback establishes the measured execution change;
it does not establish all release gates or sustained capacity.

## Sustained search and indexing contention

The final frozen implementation ran 38 development tasks 14 times per mode at
concurrency 1: **532 requests per mode, 1,596 total**, all uncached. There were
zero errors, fallback, cache hits, constraint violations or unfulfilled requested
reranks. The sum of measured HTTP durations is 255.928 seconds; it is not a wall
clock soak duration. The largest attempt, 1,944.952 ms, is retained. Repeated
development success does not overturn the failed regression/holdout gates.

| Mode | API p50 | API p95 | API p99 |
|---|---:|---:|---:|
| Dense | 33.751 ms | 42.306 ms | 46.505 ms |
| Hybrid | 33.011 ms | 43.180 ms | 47.346 ms |
| Hybrid + rerank | 541.582 ms | 857.444 ms | 922.451 ms |

During actual batch-eight embedding of the capacity corpus, a separate probe
completed 114 dense and 114 hybrid requests. Dense p95/p99 were 113.118/120.019
ms; hybrid 110.861/113.002 ms, with zero errors/fallback/cache/constraint violations.
This probe preceded the final model-query/temporal correction and is separate
from quiet qualification. Its hybrid relevance comparison still fails against
dense. Short synthetic indexing batches do not qualify long documents, higher
backfill concurrency or all-day thermal behavior.

## Actual 10,000-document capacity experiment

The isolated, verified Carol tenant contains **10,000 unique short synthetic
EMAIL, FILE and CHAT_MESSAGE documents**, across 20 topics. Every text received
an actual pinned BGE-M3 MPS embedding; no random or shared topic vectors were
substituted. Embedding 1,250 batches of eight took **122.345 seconds** (batch p95
109.517 ms); feeding the index took **11.739 seconds**. Direct feed bypasses
connectors and is not a mailbox-ingestion benchmark. The separate
[capacity tool and commands](../tools/local/capacity.md) preserve corpus/vector
fingerprints and verify the dedicated tenant before writes.

Each run submits 100 distinct requests once, with no cache or retries. Forty
are constructed lookup tasks; sixty semantic/filter/context tasks are unjudged.
All HTTP responses were successful transports, but **all six capacity gates
fail** because of lookup misses or disclosed rerank fallback.

| Mode | Concurrency | API p50 | API p95 | API p99 | Failed attempts /100 | Lookup misses | Rerank fallback |
|---|---:|---:|---:|---:|---:|---:|---:|
| Hybrid | 1 | 58.005 ms | 65.600 ms | 80.556 ms | 3 | 3 | 0 |
| Hybrid | 2 | 69.497 ms | 88.634 ms | 89.735 ms | 4 | 4 | 0 |
| Hybrid | 4 | 77.755 ms | 105.670 ms | 124.305 ms | 4 | 4 | 0 |
| Hybrid + rerank | 1 | 782.629 ms | 904.302 ms | 924.313 ms | 3 | 3 | 0 |
| Hybrid + rerank | 2 | 1,202.462 ms | 1,933.504 ms | 2,036.901 ms | 2 | 2 | 0 |
| Hybrid + rerank | 4 | 2,076.566 ms | 3,463.178 ms | 3,561.478 ms | 9 | 2 | 7 |

All 20 marker-only tasks succeed; failures occur when descriptive text surrounds
an unquoted identifier. Normal C1 lookup success is **37/40**, below a 98%
lookup target, though this experiment measures presence@10 rather than the
judged suite's exact@1 contract. Hybrid fallback is zero at every concurrency.
Optional C4 executes reranking on 93/100 requests; seven requests fall back.
Their two failure labels describe the same seven attempts and are not additional
failures. Successful HTTP status and latency alone do not qualify the pipeline.

A read-only post-evaluation diagnostic confirms that all seven expected targets
are present in the complete 30-document pre-rerank head. Normal-search misses
016/031/046/091 enter at fused ranks 20/18/20/20, and optional reranking moves
them to final ranks 9/7/4/2. Optional-search misses 056/071/081 enter at rank 1
but fall outside the final ten. Telemetry cannot separate cross-encoder demotion
from subsequent personalization/MMR. Unquoted `capref1xNNNNNN` identifiers with
prose take ordinary fusion through [query interpretation](../services/query/understand.go)
and [RRF](../services/query/rrf.go), without guaranteed exact-evidence priority.
These are final-ranking failures, not absent model candidates. No source,
configuration or labels changed after diagnosis; these diagnostic requests are
not added to qualification samples.

## Browser completion, startup and live correctness

The browser measured **104 normal hybrid searches**, eight development query
texts repeated 13 times on the all-source route. Timing starts at submission
and ends after results commit and the next animation frame, including token
access, HTTP and body decoding. p50/p95/p99 were **58.5/67.2/76.8 ms**; the first
attempt was 447 ms and remains in the sample. Errors and visible fallback were
zero. This is warm, uncached default search without cross-encoder inference;
structured golden filters are not applied in this text-only browser timing
probe. The earlier 104-attempt browser p95 was 60.4 ms over different v1 query
texts, so these observations do not establish a causal browser speedup.

The live browser also displayed the expired-session recovery state, signed back
in while preserving its query, returned a truthful zero-result state, and
applied then visibly removed the Files filter without losing the exact query.
An [actual result screenshot](assets/apple-search-2026-10-04.png)
is preserved. Representative development/capacity examples:

| Query | Observed outcome and scope |
|---|---|
| `"Kestrel Operations Handbook rev7.pdf"` | One matching FILE in the live browser; source-filter removal preserves the query. |
| `Kestrel proof that the invoice has been paid` | Judged development candidate success; submitted in normal browser timing. |
| `Kestrel: find the signed decision and the operating response procedure` | Both needs covered by the final candidate on development; no population-coverage claim. |
| `"ABSENT-KE-20417-F93A"` | Honest empty state in the live browser and judged development. |
| `find inventory count capref1x001616` | Normal capacity C1 loses the target from the top ten; diagnostic rerank places it at rank 9. |
| `find inventory count capref1x005656` | Optional capacity C1 loses the target from the top ten despite its pre-rerank rank 1. |

Restarting only owned native jobs with cached weights took 1.695 seconds to stop
and **34.008 seconds to start and warm**; Docker/UI remained running. This is
separate startup evidence, not 4–5 second query latency or first-install timing.
The jobs now run under launchd with PPID 1; container restart policies are
`unless-stopped`. The final read-only check found all six containers healthy,
browser HTTP 200, both models resident and queues idle.

Direct synthetic index checks, with no cache-bypass header, observed additions
in 5.189 ms, updates in 9.244 ms and deletions in 4.128 ms. The one temporary
test document was removed and its 404 verified. Forged tenant headers/query
parameters cannot expose the foreign document in final results or complete
candidate-head telemetry; unauthenticated spoofing returns 401. This verifies
the prescribed local tenant boundary and direct-feed freshness only. Provider
sync, within-tenant source ACL changes and revocation remain unqualified.

## Hardware, model identity and sampled memory

Actual hardware is **Apple M5 Pro, Mac17,8, arm64, 64 GiB** (68,719,476,736 bytes).
Docker reports 33,598,169,088 bytes, approximately 31.29 GiB, for its configured
32 GB VM. No further Docker increase was needed. Native runtime is Python
3.12.12, SentenceTransformers 5.1.0, Torch 2.13.0 and NumPy 1.26.4. This text lane
uses no external inference or per-query generative model.

| Resident model | Pinned revision | Actual execution and bounds |
|---|---|---|
| BGE-M3 | `5617a9f61b028005a4858fdac845db406aefb181` | MPS `mps:0`, fp32; 1,024 dimensions, L2 normalization, max 8,192 tokens, batch 8; one active inference + three waiting, max wait 600 ms. |
| bge-reranker-v2-m3 | `953dc6f6f85a1b2dbfca4c34a2796e7dde08d41e` | MPS `mps:0`, fp32; max 512 tokens, batch 16; one active inference + three waiting, max wait 2,500 ms. |

The [Apple profiles](apple-silicon.md) document separate 48/64 GB settings, Vespa's
streaming tenant groups and container limits. Their OS/browser headroom is
planning guidance, not an enforced reservation. **Actual 48 GB hardware remains
unverified** and is not qualified by restricting this 64 GiB machine.

| Sample scope | Docker project sampled maximum | Embedding RSS maximum | Reranker RSS maximum | Swap growth | Pressure-free range |
|---|---:|---:|---:|---:|---:|
| Before: 13 samples /62.074 s | 3,096,161,810 B | 683,248 KiB | 684,368 KiB | 0 MiB | 58–68% |
| Capacity preparation: 31 /302.077 s | 3,015,807,334 B | 559,488 KiB | 722,112 KiB | 0 MiB | 49–61% |
| Final search window: 49 /482.084 s | 3,287,333,992 B | 541,408 KiB | 795,008 KiB | 0 MiB | 45–59% |

The final window spans heldout/sustained search and early capacity runs, not
every later concurrency probe. Sampled current MPS driver maxima are
3,184,197,632 bytes for embedding and 4,274,765,824 for reranking. Model-reported
largest-sampled counters may predate the capture. Separately, `vmmap` reported
native process footprints of 3.7 GiB and 4.9 GiB; they are diagnostics, not an
exact total application peak. RSS and MPS counters overlap and must not be
added; Docker reporting is rounded and host VM overhead/shared buffers are not
fully accounted. System pressure/swap include other apps. Different workloads
and capture windows prevent a causal before/after memory claim. These samples
show no observed swap growth, not guaranteed headroom under arbitrary workloads.

## Strongest-baseline freeze workflow

The CLI defaults to baseline `hybrid` and candidate `hybrid_rerank`; those
names do not automatically select the strongest lane. After the conservative
correction, rerun **all four modes on development only** under the same model,
corpus, hardware, cache and request settings. Select the highest overall nDCG
among fully executed non-candidate baselines, retaining slice and task results.
The final development report selects dense. A baseline can remain a useful
comparison even when it misses
an absolute candidate threshold, as dense currently does for all-needs coverage.

```sh
go run ./tools/eval \
  -golden tools/eval/reports/local-v2-seeded/dev.jsonl \
  -gateway http://localhost:18080 \
  -oidc http://localhost:18081/realms/asker/protocol/openid-connect/token \
  -corpus-manifest tools/eval/fixtures/local-v2/manifest.json \
  -pipelines lexical,dense,hybrid,hybrid_rerank \
  -baseline dense -candidate hybrid_rerank \
  -k 10 -limit 20 -repeats 3 -concurrency 1 \
  -no-cache=true -allow-equal=false -max-p95-ms 5000 \
  -min-task-success .90 -min-exact-success .98 \
  -min-need-coverage .95 -min-latency-samples 100 \
  -hardware 'Apple M5 Pro Mac17,8 64 GiB; Docker 32 GB' \
  -model 'BGE-M3 5617a9f; reranker-v2-m3 953dc6f; native MPS fp32' \
  -out tools/eval/reports/oct4-final-v2-dev
```

Freeze the selected pipeline names, source/container/eval-binary hashes, model
revisions, active configuration and unchanged fixture/criteria hashes before
regression and the first holdout run. Preserve first reports, including failures.
Do not choose a weaker baseline after seeing holdout, enable `-allow-equal`, drop
failing tasks, or rewrite labels. The evaluator compares pipelines within one
backend run; it does not perform a statistical or causal test between historical
application images. Browser, startup, memory and capacity outcomes need their
own evidence. Actual **48 GB M5 Pro hardware remains unverified**; limiting a
64 GiB machine does not qualify it.

## Remaining evidence gaps

- **Authorization freshness:** within-tenant source ACL changes and revocation
  timing have not been validated. Passing document constraints is not that proof.
- **Query understanding:** ambiguity handling and follow-up UX remain unqualified;
  conservative typo/temporal parsing does not provide a clarification dialogue.
- **Retrieval and evidence:** full retrieval-population recall@50 is unmeasured.
  Head recall and source-ID labels do not prove complete-body evidence is exposed
  in the final preview or bounded model input.
- **Labels and hardware:** independent label adjudication and actual 48 GB M5 Pro
  measurements remain pending.
- **Memory:** exact whole-application peak memory has not been measured. Native
  processes, Docker guests, host VM overhead and shared memory require careful
  accounting to avoid omission or double counting.

## Verification and reproducibility

Run the existing private Apple64 ablation profile with cached local weights:

```sh
ASKER_APPLE_ENV_FILE="$PWD/services/local-inference/.venv/asker-state/.env.apple64-rerank.local" \
  HF_HUB_OFFLINE=1 tools/local/apple.sh up apple64
```

For a fresh checkout, follow [setup/profile instructions](apple-silicon.md) and
copy an example to ignored private state. Backend rerank capability may be
enabled for explicit ablations; keep `ASKER_APPLE_RERANK_DEFAULT=0`. The normal
browser default is off because quality gates fail. No new default is qualified.
The evaluation command above reproduces development; preserved regression and
holdout reports establish this run's result. Repeating the consumed holdout
does not provide a new untouched test. For sustained C1 use repeats 14 and
pipelines `dense,hybrid,hybrid_rerank`; use the separate capacity tool for C2/C4.
The v2 CLI rejects changed frozen concurrency before any service calls.

Local checks passed: Go race/vet and scoped lint, 163 web tests/build/lint,
25 embedding and 72 reranker tests, 18 local-tool and 12 evaluation Python tests,
Ruff, ShellCheck, shell syntax and Compose resolution. Final query checks passed
after the last correction. Source/image/profile hashes remain unchanged from
the freeze. These are local results, not CI or live connector qualification.

On October 5, the publication worktree incorporated current `origin/main` and
preserved its security headers and URL safety. Build/vet/full race tests,
coverage gate/lint and 170 web tests/build/lint passed on merged source. A
[focused live API smoke](evidence/apple-search-pr-smoke-2026-10-05.json) rebuilt
query/gateway and verified 12 authenticated keyword/vector/hybrid searches over
two synthetic exact cases, tenant-spoof isolation and 200/401 security headers.
Three temporary services used separate gateway history storage and were removed;
the original six services and data were unchanged. This validates the merged
API behavior, not the historical latency/relevance benchmark. A lightweight
CI job now runs the existing encoder/reranker/evaluation/local-tool contracts
without Torch or model downloads; its 127 tests passed in a fresh pinned local
test environment. Hosted CI results are recorded on the PR separately.

## Artifacts used

Published aggregate evidence and the exact freeze record are linked above; the
synthetic [browser screenshot](assets/apple-search-2026-10-04.png) is also included
in this repository. The raw paths below describe retained operator-local files
and will not exist in a fresh clone.


Local generated reports are preserved and ignored by Git:

- Before: `tools/eval/reports/oct4-before-all-c1/`, `oct4-before-c2/`,
  `oct4-before-c4/`, and `oct4-v2-before-dev/`.
- Intermediate after: `oct4-v1-after-dev/`, `oct4-v1-after-c2/`,
  `oct4-v1-after-c4/`, and `oct4-v2-after-dev/`.
- Frozen conservative-correction development: `oct4-v2-frozen-dev/`, generated
  `2026-10-05T03:48:31Z`.
- Frozen regression: `oct4-v2-frozen-regression/`, generated
  `2026-10-05T03:50:14Z`.
- First untouched holdout: `oct4-v2-first-holdout/`, generated
  `2026-10-05T03:51:50Z`.
- Sustained: `oct4-v2-final-sustained/`; contention: `oct4-indexing-contention/`.
- Capacity: `services/local-inference/.venv/asker-state/capacity-v1-s1-n10000/`
  (manifest, actual embedding/feed summaries and all six `run-c*-r1-rerank*.json`
  reports). Document SHA-256:
  `3f315a94da4fc21eccf5fefc2bfbb9ff116b26454ed3a0c6527c6d2f78cdf1e1`;
  task SHA-256:
  `e99a62fcbf7c733673024e303b8457c3b51b35ffab41eae3c2641f7abb6791f4`.
- Native state: `oct4-browser-final-v2.json`, `oct4-native-startup.json`,
  `oct4-live-validation.json`, `oct4-capacity-head-diagnostic.json`,
  `oct4-before-memory.jsonl`, `oct4-capacity-memory.jsonl`,
  `oct4-final-search-memory.jsonl` and `oct4-final-*-vmmap.txt`, all under
  `services/local-inference/.venv/asker-state/`.

See the [evaluation contract](../tools/eval/README.md) and
[Apple runtime guide](apple-silicon.md). The runtime is verified and fast in the
recorded scopes; search-quality qualification remains failed. Regression/holdout
aggregates and measurements are reported without
opening their query or evidence label files.
