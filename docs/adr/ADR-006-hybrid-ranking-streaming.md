# ADR-006: Hybrid ranking in Vespa streaming mode — single-pass linear blend

## Status

Accepted (M1).

## Context

The spec prescribes hybrid ranking as "BM25 + closeness() fused with reciprocal rank fusion
or learned linear blend, then optional cross-encoder rerank of top-50." Asker runs Vespa in
**streaming mode** (the architecture's load-bearing choice for 10M tenants), and streaming
mode maintains no inverted index and therefore no corpus statistics: **`bm25` is unavailable**
(noted at M0 close — the M0 schema already fell back to `nativeRank`). RRF needs two ranked
lists fused either in a second phase or in the query service; a learned blend needs training
data; a cross-encoder needs a model server and ~300ms of budget — and at M1 there are zero
relevance measurements to justify any of that machinery.

## Decision

M1 ranks with a **single-pass linear blend in the Vespa rank profile**:

```
score = w_text * nativeRank(title, body, chunk_texts) + w_vector * closeness(embedding)
```

- `nativeRank` over title/body/chunk text is the keyword signal (the streaming-mode stand-in
  for BM25).
- `closeness` is computed against the chunk-mapped embedding tensor (one vector per chunk;
  the best-matching chunk dominates), at the dimension configured per ADR-005.
- The YQL combines both retrievers: `userQuery() OR nearestNeighbor(embedding, q)`, always
  inside the tenant's streaming group selector (`streaming.groupname`), so a document is a
  candidate if either signal finds it and the blend orders the union.
- Initial weights are hand-picked constants in the rank profile; **RRF and cross-encoder
  rerank are deferred to M5**, to be adopted only with relevance/latency measurements from the
  load and quality suites.
- Degradation ladder (spec §2.6), as implemented at M1: the rerank rung is trivially absent;
  under pressure or when TEI fails, the query service drops the vector clause and runs
  keyword-only (`mode=keyword` forces this); a search request **never fails closed** because
  an enrichment dependency is down.

## Consequences

- Keyword quality is `nativeRank`, not BM25 — weaker on term-frequency saturation and
  document-length normalization. This is a property of streaming mode itself, not laziness;
  it applies to prod too. Relevance work in M5 measures whether it matters for personal-corpus
  sizes.
- Hand-picked weights are unprincipled by construction. That is the point: they are a
  placeholder cheap enough to delete, and M5's measurements decide between tuned weights, RRF,
  and rerank.
- Single-pass scoring keeps the query path simple (one Vespa round trip) and the latency
  budget intact; there is no second-phase model dependency to operate at M1.
- `mode=hybrid|keyword` in the search API is honest about what ran; the `degraded` response
  field reports when vector search was dropped so tests and the UI can tell blended results
  from keyword-only ones.
- When rerank arrives (M5), it slots in after Vespa retrieval without changing this contract;
  the ladder gains its first rung instead of being rebuilt.
