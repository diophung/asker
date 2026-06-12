# Capacity model

> **Status: preliminary — to be finalized with measurements in M5.** The numbers below are the
> planning model from the spec (§2.7). M5 replaces the assumptions with measured per-node
> throughput from k6 load runs and derives concrete node counts for 10PB / 50K TPS.

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
  is a key quantity to **measure in M5** with the synthetic corpus generator.

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

## Topology plan

- **Vespa:** shard content clusters by tenant hash; size node groups so a hot tenant's
  streaming query touches exactly one group. Streaming-mode scan throughput per node — the
  number that sets the fleet size for both the 10PB corpus and the 50K TPS / P90 ≤ 5s SLOs —
  is the single most important M5 measurement.
- **Kafka:** ≥ 512 partitions on the `docs.*` topics, keyed by `tenant_id`, so consumer groups
  can scale ingest horizontally while preserving per-tenant ordering.
- **Everything else** (gateway, query, ingest, enrich, index writers, connector hub) is
  stateless and scales on HPA. TEI scales as its own pool sized to embedding QPS (query-time
  embeds at 50K TPS + ingest-time embeds within the 30-minute freshness SLA).

## To finalize in M5

- Measured per-node Vespa streaming query throughput and latency vs tenant corpus size.
- Measured ingest pipeline throughput (docs/sec per worker) and freshness under load.
- Measured text-extraction and chunk-size ratios on the synthetic corpus.
- int8 vs bfloat16 recall/latency tradeoff.
- Concrete node counts and cost model for 10PB / 50K TPS, with extrapolation math from the
  test cluster's scale.
