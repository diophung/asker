# Asker retrieval eval harness

Measures **retrieval quality** (Recall@k, nDCG@k, MRR) and **latency** (p50/p95)
for four pipelines — `lexical`, `dense`, `hybrid`, and `hybrid_rerank` — against
a labeled golden set, and enforces a **quality gate**. This is the ground truth
the hybrid-search upgrade is measured against: nothing ships that doesn't clear
the gate.

> Location note: the harness lives under `tools/` (not a top-level `eval/`) so
> the Makefile's `GO_PKGS` builds, vets, lints, and unit-tests it like the rest
> of the Go tree. Run it with `make eval`.

## What it does

The runner drives the **live gateway** REST API (`GET /v1/search`), so it scores
the real end-to-end pipeline (query understanding → retrieval → fusion → rerank)
exactly as a user hits it. It is engine-agnostic — it only reads the public
search response — so the *same* command baselines today's pipeline and, later,
the reranked one. For each golden query it computes Recall@k / nDCG@k / MRR per
**slice** (`exact`, `keyword`, `semantic`, `multilingual`) and overall, plus
per-pipeline latency percentiles, and writes a committed report to `reports/`.

`hybrid_rerank` is declared but marked **n/a** until the cross-encoder rerank
rung lands (Phase 1); until then the gate is *skipped* (a baseline run of
`lexical`/`dense`/`hybrid` still produces numbers). When the reranker ships, flip
`Supported` in `standardPipelines()` and the gate activates automatically.

## The quality gate

`hybrid_rerank` must **match or beat `hybrid` on nDCG@k for every slice** and
**strictly improve overall nDCG@k**. Per-slice enforcement is the point: it
catches a change that lifts the average while quietly regressing exact-match
(filenames, identifiers, error codes) — the classic failure mode of leaning too
hard on vectors. `make eval` exits non-zero when a supported gate fails, so it
doubles as a CI signal.

## Running it

1. Bring up and seed a dev stack (see the repo README "Try it" section — seed
   fake Gmail, or `make synthgen-load` for a synthetic corpus).
2. Build a golden set (below) at `tools/eval/golden/golden.jsonl`.
3. Run:

```sh
make eval                                   # defaults: k=10, gateway :8080, Keycloak :8081
make eval EVAL_ARGS="-k 10 -limit 20"       # pass flags through
go run ./tools/eval -golden tools/eval/golden/golden.jsonl -gateway http://localhost:8080
```

Tokens: by default the runner mints a dev token per `tenant` via the Keycloak
password grant (`username == tenant`, password `password123`). For a non-dev
setup, pass `-tokens tokens.json` mapping `tenant -> bearer token` (gitignored).

## The golden set

JSONL, one record per line; blank lines and `#`-comments are ignored. See
[`golden/golden.example.jsonl`](golden/golden.example.jsonl).

```json
{"id":"kw-budget-01","query":"quarterly budget review","tenant":"alice","slice":"keyword","relevant":["<doc_id>"]}
{"id":"sem-due-01","query":"when is the project due","tenant":"alice","slice":"semantic","gains":{"<doc_id_a>":3,"<doc_id_b>":1}}
```

- `relevant` — binary relevance (gain 1). `gains` — graded relevance (`doc_id ->
  gain`); at least one relevant doc is required.
- `tenant` — which dev user's token to search as (its verified claims become the
  Vespa tenant group).
- `slice` — `exact` | `keyword` | `semantic` | `multilingual`.

### Getting real `doc_id`s

Doc ids are `sdk.DocID(connector_id, source_native_id)` (a hash), so you can't
guess them — read them back from the corpus you seeded. Two supported paths:

- **synthgen (no private data).** Seed a deterministic corpus into your own
  tenant with `make synthgen-load SYNTHGEN_ARGS="--tenant-id-override <your-sub>
  --seed 1 --dry-run=false"`. Every `qzx%08d` rare token is planted in exactly
  one doc, giving a ready-made `exact` slice. A `golden generate --from-synthgen`
  subcommand that emits these pairs automatically is the next follow-up; until
  then, query each `qzx…` token once and record the returned `doc_id`.
- **your real corpus (curated).** Run candidate queries against the seeded
  stack, and for each keep the `doc_id`(s) you judge relevant. The prompt's
  suggested flow — have a local LLM propose `query → doc` candidates, then curate
  them by hand — produces the `semantic`/`multilingual` slices. Keep 60–100 pairs
  across slices for a stable signal.

The real `golden/golden.jsonl` is **gitignored** (it can contain private
queries); the committed `*.example.jsonl` is the template.

## Latency

The harness reports end-to-end p50/p95 per pipeline (client round-trip). Per-
*stage* p50/p95 (embed / retrieve / fuse / rerank) comes from the query
service's own metrics; the rerank stage will emit its own timing when it lands,
so the 300 ms rerank budget is checked at the source. Note the query service
caches results ~60 s, so re-running the same golden set understates cold latency
— reseed or vary the corpus to measure cold.

## Files

| file | role |
|---|---|
| `metrics.go` | Recall@k, nDCG@k, reciprocal rank, percentiles (pure, unit-tested) |
| `golden.go` | golden-set record + JSONL loader + validation |
| `searcher.go` | `Searcher` interface + live `GatewaySearcher` + the four pipelines |
| `runner.go` | run × score × aggregate (per slice + overall) + quality gate |
| `report.go` | console table + committed markdown/JSON report |
| `main.go` | CLI + token resolution (Keycloak password grant / tokens file) |
