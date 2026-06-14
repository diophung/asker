# Capacity model

> **Status: finalized in M5.** The planning assumptions and storage math below come from the
> spec (§2.7). M5 adds the **scaling model** — the explicit formulas that turn a *measured*
> per-node throughput into concrete node counts for the targets (50K query TPS, 10PB raw / the
> derived chunk and vector counts). The measured constants are produced by the CI load/soak run
> and recorded in [`loadtest-report.md`](loadtest-report.md); every place a measured number is
> required is marked **`<measured in CI: see loadtest-report.md>`**, so the math here is complete
> and only the constants are pending an actual run. The SLO/observability decisions behind the
> metrics this model relies on are recorded in
> [ADR-017](adr/ADR-017-slo-observability.md).

## Load assumptions

| Quantity | Value | Source |
| :--- | ---: | :--- |
| Registered tenants | 10M | hard requirement |
| Concurrently active | ~5% ≈ 500K | spec assumption |
| Peak query throughput | 50K TPS | hard requirement |
| Raw data across all tenants | up to 10PB | hard requirement |
| Tenant size distribution | median: a few GB; power user: a few TB | spec assumption |

## Data volume chain

```
10PB raw  ──(~8–12% extractable text)──▶  ~1PB text  ──(chunking)──▶  ~2T chunks
```

- Most raw bytes are media, attachments, and container formats; only ~8–12% survives text
  extraction.
- Chunking policy is ~512-token chunks with 64-token overlap; the ~2T figure implies an
  average of ~500 bytes of text per chunk. The effective text-per-chunk ratio (overlap,
  dedupe, short-document skew — chat messages and calendar events are far below 512 tokens)
  is a key quantity **measured in M5** with the synthetic corpus generator (`tools/synthgen`):
  divide the synthgen summary's total feed bytes by the resulting Vespa chunk count.

## Embedding storage (bge-m3, 1024-dim)

Per-vector cost by Vespa cell type, and totals at the chunk-count scale points:

| Cell type | Bytes/vector | Per 1B chunks | At ~2T chunks |
| :--- | ---: | ---: | ---: |
| float (fp32) | 4,096 | ~4TB | ~8PB |
| bfloat16 / fp16 | 2,048 | ~2TB | ~4PB |
| int8 | 1,024 | ~1TB | ~2PB |

(The spec's headline "≈4TB per 1B chunks" is the fp32 figure; fp16 halves it.) These are raw
vector bytes — redundancy (×2) and attribute overhead come on top. Plan of record:

- Use Vespa **bfloat16 or int8 cell types** for stored vectors.
- Use **paged attributes** so cold tenants' vectors live on disk, not RAM — consistent with
  streaming mode's "near-zero cost at rest" goal. Recall impact of int8 vs bfloat16 is an M5
  measurement.

## Scaling model

The whole point of M5 is that the fleet is **not** sized from guesses: a load test on a small,
known cluster yields a *measured per-node throughput* and *per-node usable capacity*, and the
formulas below extrapolate those linearly to the targets. Linear extrapolation is valid here
**because the architecture is shared-nothing per tenant** (§ architecture.md §1): no query spans
tenants, Vespa streaming groups are independent, and the stateless tiers hold no cross-request
state — so N× the nodes serves ~N× the load until a shared dependency (Kafka, the control-plane
DB) saturates. The extrapolation's headroom assumptions and the saturation caveat are restated
in the load report.

Notation: `ceil(x)` rounds up to a whole node; every count is then multiplied by a redundancy /
replication factor so a node loss does not breach the SLO.

### 1. Query tier (stateless: gateway, query) — sized by the 50K TPS / P90 ≤ 5s SLO

```
measured_TPS_per_query_node  = max sustained query RPS on ONE query node while P90 < 5000ms
                               <measured in CI: see loadtest-report.md>

query_nodes = ceil( target_TPS / measured_TPS_per_query_node ) * query_replication

  where target_TPS      = 50,000   (hard requirement)
        query_replication = headroom/HA multiplier (plan of record: 1.5 — survive the loss of
                            up to ~1/3 of the fleet, e.g. an AZ, without breaching P90)
```

The query node and the gateway node are sized **separately** (gateway does only authn + routing,
~20ms of the budget; query does understanding + embed-dispatch + Vespa fan-out + assembly), each
with its own `measured_TPS_per_*_node`. Both are HPA-driven (see §4) so the *number above is the
HPA `maxReplicas` ceiling and the capacity-planning figure*, not a fixed count.

**Worked example** (placeholder measured value): if `measured_TPS_per_query_node = 1,200` then

```
query_nodes = ceil(50000 / 1200) * 1.5 = ceil(41.7) * 1.5 = 42 * 1.5 = 63 query nodes
```

### 2. TEI embedding pool — sized by query-time + ingest-time embed QPS

Query-time embeds are on the critical path (one embed per uncached query). Ingest-time embeds
must keep up with the freshness SLA. Size the pool to the **sum**, discounted by the query cache
hit rate:

```
measured_embeds_per_TEI_node = max sustained embed QPS on one TEI node within its latency slice
                               (≤ 50ms of the budget for query embeds) <measured in CI: see loadtest-report.md>

query_embed_QPS  = target_TPS * (1 - cache_hit_rate)
                   cache_hit_rate = sum(rate(asker_query_cache_requests_total{result="hit"}[5m]))
                                  / sum(rate(asker_query_cache_requests_total[5m]))  <measured in CI>
ingest_embed_QPS = chunks_per_second_at_peak_ingest   <measured in CI: see loadtest-report.md>

tei_nodes = ceil( (query_embed_QPS + ingest_embed_QPS) / measured_embeds_per_TEI_node ) * tei_replication
```

### 3. Vespa content tier — sized by BOTH storage (10PB→vectors) and query throughput; take the max

Vespa is the one stateful tier sized by two independent constraints; provision the larger.

**(a) Storage-bound node count** — from the corpus byte budget:

```
stored_bytes = chunk_count * (vector_bytes_per_chunk + attribute_overhead_per_chunk + text_bytes_per_chunk)
   chunk_count               ≈ 2T            (data volume chain above)
   vector_bytes_per_chunk    = 2,048 (bfloat16)  or 1,024 (int8)   (embedding storage table)
   attribute_overhead+text   <measured in CI: see loadtest-report.md>  (Vespa attribute + summary store per chunk)

vespa_content_nodes_storage = ceil( stored_bytes / usable_bytes_per_content_node ) * vespa_redundancy
   usable_bytes_per_content_node = per-node disk usable for paged attributes + docstore
                                   <measured in CI: see loadtest-report.md>
   vespa_redundancy              = 2  (streaming-mode group redundancy; spec §2.7 "×2")
```

**(b) Throughput-bound node count** — from the streaming scan rate (the single most important
M5 measurement: how fast one node scans one tenant's group while holding P90 ≤ 5s):

```
measured_streaming_TPS_per_node = max sustained tenant-scoped streaming queries/s on one content
                                  node at the test corpus's per-tenant size, P90 < 5000ms
                                  <measured in CI: see loadtest-report.md>

vespa_content_nodes_throughput = ceil( target_TPS / measured_streaming_TPS_per_node ) * vespa_redundancy
```

**Provision:** `vespa_content_nodes = max(storage-bound, throughput-bound)`, grouped so a hot
tenant's streaming query touches **exactly one** content group (shard by tenant hash). Because
streaming-mode scan cost is proportional to *one tenant's* corpus (not the fleet's), the
throughput bound depends on the **per-tenant** size used in the load test — the report records
the per-tenant byte size the measurement was taken at, and the extrapolation flags that a fleet
of much larger power-user tenants shifts the throughput bound up.

**Worked example** (placeholder measured values): bfloat16, `usable_bytes_per_content_node = 4TB`,
`attribute_overhead+text ≈ 1,000 B/chunk`, `measured_streaming_TPS_per_node = 400`:

```
stored_bytes ≈ 2T * (2,048 + 1,000) ≈ 6.1 PB
storage-bound    = ceil(6.1PB / 4TB) * 2 = ceil(1525) * 2 = 1525 * 2 = 3,050 content nodes
throughput-bound = ceil(50000 / 400) * 2 = ceil(125)  * 2 = 125  * 2 =   250 content nodes
vespa_content_nodes = max(3050, 250) = 3,050 content nodes   (storage-bound dominates at 10PB)
```

### 4. Stateless tiers — HPA sizing (gateway, query, ingest, enrich, index-writer, connector-hub)

Every stateless service autoscales on CPU/memory (ADR-014 §6). The capacity model sets the HPA
`maxReplicas` from the same per-node throughput, and `minReplicas` for HA:

```
maxReplicas(svc) = ceil( peak_load(svc) / measured_per_replica_throughput(svc) ) * svc_replication
minReplicas(svc) = ceil( steady_load(svc) / measured_per_replica_throughput(svc) ), and ≥ 2 for HA
   peak_load(query, gateway)  = target_TPS = 50,000
   peak_load(ingest, enrich, index-writer) = peak chunks/s within the 30-min freshness SLA
                                             <measured in CI: see loadtest-report.md>
```

The ingest/enrich/index-writer trio is bounded downstream by the Kafka partition count (§5): a
consumer group cannot have more useful consumers than partitions, so `maxReplicas` for each is
capped at the partition count.

### 5. Kafka — partitions and brokers

```
partitions(docs.*)  ≥ 512    (spec §2.7, hard floor; keyed by tenant_id to preserve per-tenant
                              ordering while allowing ≤512-way consumer parallelism)
partitions          = max( 512, ceil( peak_chunks_per_second / measured_partition_throughput ) )
                      measured_partition_throughput <measured in CI: see loadtest-report.md>
brokers             = ceil( total_partitions * replication_factor / partitions_per_broker_budget )
                      replication_factor = 3 (durability; zero-data-loss exit criterion)
```

## Topology plan

- **Vespa:** shard content clusters by tenant hash; size node groups so a hot tenant's
  streaming query touches exactly one group (see §3). Streaming-mode scan throughput per node is
  the number that sets the throughput bound; usable bytes per node sets the storage bound;
  provision the larger.
- **Kafka:** ≥ 512 partitions on the `docs.*` topics, keyed by `tenant_id`, so consumer groups
  can scale ingest horizontally while preserving per-tenant ordering (§5).
- **Everything else** (gateway, query, ingest, enrich, index writers, connector hub) is
  stateless and scales on HPA (§4). TEI scales as its own pool sized to embedding QPS (§2).

## SLO → metric → dashboard traceability

Each SLO is *proven* by a specific Prometheus metric (the wave-0 instrumentation; names are
post-OTel-suffixing) and watched on a specific Grafana dashboard panel. See
[ADR-017](adr/ADR-017-slo-observability.md) for why the metric set carries **no** tenant/doc
labels (cardinality at 10M tenants).

| SLO (spec §2.7 / architecture.md §5) | Proving metric | Query | Dashboard |
| :--- | :--- | :--- | :--- |
| **Query latency P90 ≤ 5000 ms** (hard) | `asker_query_search_duration_milliseconds` (histogram) | `histogram_quantile(0.9, sum by(le)(rate(asker_query_search_duration_milliseconds_bucket[$__rate_interval])))` | SLO dashboard "Query latency P50/P90/P99" |
| Query latency P50 ≤ 800 ms (design target) | same | `histogram_quantile(0.5, …)` | same panel |
| **Ingest freshness ≤ 30 min** (source edit → searchable) | `asker_index_doc_age_seconds` (histogram; the 1800 s bucket is the SLA line) | P90 doc age at index time: `histogram_quantile(0.9, sum by(le)(rate(asker_index_doc_age_seconds_bucket[$__rate_interval])))`; fraction within SLA = `sum(rate(..._bucket{le="1800"}[$__rate_interval])) / sum(rate(..._count[$__rate_interval]))` | Freshness dashboard "Doc age (end-to-end)" |
| **Zero data loss** (soak exit) | `asker_pipeline_deadletter_total` (counter, the authoritative signal) | `sum(increase(asker_pipeline_deadletter_total[2h]))` must be **0** | Pipeline-health dashboard "Dead-letter rate" |
| Degradation as a first-class dimension | `asker_query_degradation_events_total` + the `degraded` label on the latency histogram | `sum by(rung)(rate(asker_query_degradation_events_total[5m]))` | SLO dashboard "Degradation ladder" |
| Cache effectiveness (sizes the TEI pool, §2) | `asker_query_cache_requests_total` | `sum(rate(...{result="hit"}[5m])) / sum(rate(...[5m]))` | SLO dashboard "Cache hit rate" |

> **Freshness-proxy limitation (ADR-017):** `asker_index_doc_age_seconds` measures
> `now − Document.created_at` *at index time*, which is an **in-cluster** proxy: it covers
> connector-emit → searchable, but not the time a source took to deliver the edit to the
> connector (webhook lag, the polling interval). The 30-min SLA is whole-path; the metric proves
> the part Asker controls. The load test additionally stopwatches a source edit through to a
> successful query for the whole-path number, recorded in the report.

## What M5 measures (the constants this model is pending)

- Measured per-node Vespa streaming query throughput and latency vs tenant corpus size.
- Measured ingest pipeline throughput (chunks/sec per worker) and freshness under load.
- Measured text-extraction and chunk-size ratios on the synthetic corpus.
- Measured query-node, gateway-node, and TEI-node sustained throughput at P90 < 5s.
- Usable bytes per content node and per-chunk attribute/text overhead.
- int8 vs bfloat16 recall/latency tradeoff.

All feed the formulas above; the filled values land in [`loadtest-report.md`](loadtest-report.md)
and the final node counts are recomputed there with the worked examples re-run on real numbers.
