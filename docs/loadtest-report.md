# Load & soak test report

> **This file is a TEMPLATE.** The CI load workflow (`.github/workflows/load.yml`) fills it from a
> real run and uploads the filled copy as an artifact; a committed run replaces the placeholders
> below. The capacity formulas it references live in [`capacity.md`](capacity.md); the metric and
> SLO definitions in [ADR-017](adr/ADR-017-slo-observability.md).
>
> Two flavors of every number appear here: **`<fill: …>`** placeholders the CI run substitutes,
> and a fully **filled ILLUSTRATIVE EXAMPLE** in the boxed section at the end so the structure is
> unambiguous. **The illustrative numbers are a SAMPLE, not a real run** — do not cite them as
> results.

## Run metadata

| Field | Value |
| :--- | :--- |
| Date (UTC) | `<fill: run date>` |
| Git SHA | `<fill: commit>` |
| Workflow run | `<fill: actions run URL>` |
| Cluster / runner | `<fill: e.g. compose on ubuntu-latest 16GB, or kind profile>` |
| Embedding model / dim | `BAAI/bge-small-en-v1.5` / 384 (CI profile; prod is bge-m3 / 1024) |
| k6 version | `<fill: k6 version>` |
| `SOAK_DURATION` input | `<fill: e.g. 10m (CI default); 2h on-demand>` |

## 1. Test scale (from the synthgen summary)

Source: `tools/synthgen` summary (the `synthgen plan`/`RunResult` output — tenants, total docs,
est. feed bytes, rare-token range, doc-type mix). The rare-token range and isolation markers are
re-derivable from the seed, so the query suite asserts exact hit counts.

| Quantity | Value |
| :--- | ---: |
| Seed | `<fill: SYNTHGEN_SEED>` |
| Tenants | `<fill: synthgen tenants>` |
| Total docs | `<fill: synthgen total docs>` |
| Est. feed bytes | `<fill: synthgen est. feed bytes>` |
| Resulting Vespa chunks | `<fill: indexed chunk count>` |
| Bytes / chunk (text+attr) | `<fill: feed bytes / chunk count → feeds capacity.md §3>` |
| Rare tokens (range) | `<fill: RareToken(first)..RareToken(last)>` |
| Doc-type mix | `<fill: EMAIL/FILE/CHAT_MESSAGE/... counts>` |

## 2. Query results (k6 query suite, stepped to cluster max)

The query suite steps RPS up while watching `asker_query_search_duration_milliseconds`. The
**max sustained RPS with P90 < 5000ms** is the headline (and `measured_TPS_per_query_node` once
normalized per node). Exact-hit assertions use the synthgen rare tokens; isolation assertions use
the per-tenant markers (a cross-tenant marker search must return 0 — the sacred invariant).

| Metric | Value | SLO |
| :--- | ---: | :--- |
| Max sustained RPS at P90 < 5s | `<fill>` | — |
| P50 latency | `<fill> ms` | ≤ 800 ms (design target) |
| **P90 latency** | `<fill> ms` | **≤ 5000 ms (HARD)** |
| P99 latency | `<fill> ms` | — |
| Cache hit rate at peak | `<fill> %` | — (sizes TEI pool) |
| Degradation events during run | `<fill> (rung breakdown)` | should be ~0 when healthy |
| Failed requests | `<fill>` | 0 beyond retry |
| Exact-hit assertions (rare tokens) | `<fill: N/N passed>` | all pass |
| Cross-tenant isolation (markers) | `<fill: N/N returned 0 foreign hits>` | all 0 |

Latency by mode/cache (from the histogram labels `mode`, `cache`, `degraded`):

| Mode | Cache | P50 | P90 | P99 |
| :--- | :--- | ---: | ---: | ---: |
| hybrid | miss | `<fill>` | `<fill>` | `<fill>` |
| hybrid | hit | `<fill>` | `<fill>` | `<fill>` |
| keyword | miss | `<fill>` | `<fill>` | `<fill>` |

## 3. Ingest freshness (k6 ingest suite, under load)

Prove a source edit becomes searchable within 30 min while ingest runs at load. In-cluster proxy:
`asker_index_doc_age_seconds` (P90 + fraction within the 1800 s bucket). Whole-path: the suite
stopwatches a tagged edit feed → successful query.

| Metric | Value | SLO |
| :--- | ---: | :--- |
| Sustained ingest rate | `<fill> chunks/s` | — (sizes ingest/enrich/index-writer) |
| **Doc-age P90 (in-cluster proxy)** | `<fill> s` | **≤ 1800 s (30 min)** |
| Fraction within 1800 s bucket | `<fill> %` | → 100% |
| **Whole-path edit→searchable (stopwatch)** | `<fill> s` | **≤ 1800 s** |
| Per-attempt handler error rate | `<fill> %` | low; retried (not a dead-letter) |

## 4. Soak result (2h, zero data loss)

The soak holds a steady mixed query+ingest load. Exit criterion: **zero data loss**, signalled by
`asker_pipeline_deadletter_total` (the authoritative quarantine counter) staying at **0**.

> **CI runs a SHORT soak** (`SOAK_DURATION` default, e.g. 10m) to keep the pipeline cheap; the
> **real 2-hour soak is run on demand** (`workflow_dispatch` with `SOAK_DURATION=2h`). State the
> actual duration of this run.

| Metric | Value | Exit criterion |
| :--- | ---: | :--- |
| Soak duration | `<fill>` | 2h for the formal exit (CI default short) |
| Total queries | `<fill>` | — |
| Failed requests | `<fill>` | 0 beyond retry |
| P90 latency over soak | `<fill> ms` | ≤ 5000 ms held |
| **Dead-letter count (`increase(asker_pipeline_deadletter_total[soak])`)** | `<fill>` | **== 0** |
| Memory/CPU drift (leak check) | `<fill>` | stable, no monotonic climb |

## 5. Extrapolation to 50K TPS / 10PB (capacity.md formulas)

Plug the measured constants from §2–§3 into [`capacity.md`](capacity.md). Show the arithmetic.

```
measured_TPS_per_query_node      = <fill from §2 / node count>
measured_streaming_TPS_per_node  = <fill from §2 Vespa node count>
measured_embeds_per_TEI_node     = <fill>
usable_bytes_per_content_node    = <fill>
bytes_per_chunk (attr+text)      = <fill from §1>

query_nodes  = ceil(50000 / measured_TPS_per_query_node) * 1.5      = <fill>
tei_nodes    = ceil((50000*(1-cache_hit) + ingest_embed_QPS) / measured_embeds_per_TEI_node) * repl = <fill>
vespa_storage    = ceil(2T*(2048+bytes_per_chunk) / usable_bytes_per_content_node) * 2 = <fill>
vespa_throughput = ceil(50000 / measured_streaming_TPS_per_node) * 2                   = <fill>
vespa_content_nodes = max(vespa_storage, vespa_throughput)                             = <fill>
kafka_partitions = max(512, ceil(peak_chunks_per_s / measured_partition_throughput))   = <fill>
```

| Target tier | Extrapolated count | Bound by |
| :--- | ---: | :--- |
| Query nodes | `<fill>` | TPS |
| TEI nodes | `<fill>` | embed QPS |
| Vespa content nodes | `<fill>` | `<fill: storage or throughput>` |
| Kafka partitions / brokers | `<fill>` / `<fill>` | partition throughput / durability |

## 6. PASS / FAIL vs the M5 exit criterion

> Exit (spec §5 / MILESTONES.md): *load report committed showing P90 latency and freshness SLAs
> met at test scale, with the scaling math to target scale; 2-hour soak with zero data loss.*

| Criterion | Result |
| :--- | :--- |
| P90 query latency ≤ 5000 ms at test scale | `<PASS/FAIL>` |
| Freshness ≤ 30 min under load | `<PASS/FAIL>` |
| Scaling math to 50K TPS / 10PB present | `<PASS/FAIL>` |
| Soak: zero data loss (dead-letter == 0) | `<PASS/FAIL>` |
| Cross-tenant isolation held throughout | `<PASS/FAIL>` |
| **Overall** | `<PASS/FAIL>` |

---

## ILLUSTRATIVE EXAMPLE — SAMPLE NUMBERS, NOT A REAL RUN

> The block below is a fully filled report with **made-up** numbers, included only so the shape is
> concrete. **Do not cite these as results.** A real CI run overwrites the sections above.

**Run:** 2026-06-13 · SHA `abc1234` · compose on ubuntu-latest (16GB) · bge-small/384 · k6 v0.50 ·
`SOAK_DURATION=10m`.

**1. Scale:** seed 1 · 1,000 tenants · 250,000 docs · ~245 MiB feed · ~310,000 chunks · ~820
B/chunk · rare tokens `qzx00000000..qzx00002499` · mix EMAIL 113k / FILE 68k / CHAT 45k / IMAGE
23k / WIKI 23k / CAL 23k.

**2. Query:** max sustained **920 RPS** at P90 < 5s · P50 **210 ms** (≤800 ✓) · **P90 1,840 ms**
(≤5000 ✓) · P99 3,950 ms · cache hit 38% · 0 degradation events · 0 failed · 2,500/2,500 exact-hit
✓ · 1,000/1,000 isolation searches returned 0 foreign hits ✓.

**3. Freshness:** ingest 1,400 chunks/s · doc-age **P90 96 s** (≤1800 ✓) · 100% within 1800 s ·
whole-path edit→searchable **41 s** (≤1800 ✓) · per-attempt error 0.2% (retried).

**4. Soak (10m sample):** 552,000 queries · 0 failed · P90 1,910 ms held · **dead-letter count 0**
(✓ zero data loss) · memory flat.

**5. Extrapolation** (with `measured_TPS_per_query_node=920` on a 1-node query tier,
`measured_streaming_TPS_per_node=900`, `measured_embeds_per_TEI_node=1,500`,
`usable_bytes_per_content_node=4TB`, `bytes_per_chunk=820`, cache_hit=0.38, ingest_embed=1,400/s):

```
query_nodes      = ceil(50000/920)*1.5      = ceil(54.3)*1.5 = 55*1.5  = 83
tei_nodes        = ceil((50000*0.62 + 1400)/1500)*1.5 = ceil(21.6)*1.5 = 22*1.5 = 33
vespa_storage    = ceil(2T*(2048+820)/4TB)*2 = ceil(1434)*2 = 1434*2   = 2,868
vespa_throughput = ceil(50000/900)*2         = ceil(55.6)*2 = 56*2     = 112
vespa_content    = max(2868, 112)            = 2,868  (storage-bound)
kafka_partitions = max(512, ceil(1400/peak)) = 512    (floor dominates at test rate)
```

**6. PASS/FAIL (sample):** P90 ✓ · freshness ✓ · scaling math ✓ · soak zero-loss ✓ · isolation
✓ · **Overall PASS** (note: the formal exit requires the real 2-hour soak, not the 10m sample).
