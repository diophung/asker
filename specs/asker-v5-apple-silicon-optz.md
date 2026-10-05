> User-supplied implementation prompt, preserved as requirements context.
> Asker in this repository is personal mail/calendar/file/chat search. The
> conditional `agentic-search` paths and procurement examples below refer to a
> separate application and do not redefine this repository's domain. Current
> implementation and measured limits are in [the validation report](../docs/search-quality-validation-2026-10-04.md).

You are a world renowned, industry expert, a distinguished search engineer and Apple Silicon inference specialist. Improve Asker’s search quality and responsiveness through concrete implementation and measured validation.

MISSION

Make Asker feel as dependable and intuitive as searching Gmail or Google Calendar: users should find the right information despite imperfect wording, while exact identifiers, exclusions, and explicit filters remain reliable.

Target deployment: MacBook Pro M5 Pro with either 48GB or 64GB unified memory.

Target response time: approximately 4–5 seconds. Treat p95 ≤5 seconds as the acceptance target for the complete normal search interaction after startup, including fresh queries without whole-response cache hits.

Inspect, implement, test, and report results. Continue beyond recommendations when repository access permits implementation. Ask only when a missing decision materially blocks progress.

1. ESTABLISH THE CURRENT SYSTEM

Identify Asker’s actual entry points, searchable entities, data sources, query pipeline, models, indexes, filtering rules, and user flows. Do not infer the application’s domain from its name.

Read applicable AGENTS.md instructions. Preserve existing work and repository conventions.

If working in the agentic-search repository, inspect:
- src/README.md
- src/v4/ and src/v5/
- src/v5/ARCHITECTURE.md
- src/v5/VALIDATION.md
- src/v5/LOCAL_MODELS.md
- src/v5/data/README.md
- src/tools/search.py
- src/services/embedding-shim/
- Solr schema/configuration
- specs/procurement-mcp-specs-v1.md

Confirm which components belong to Asker before editing them.

Record a reproducible baseline for relevance, complete response latency, and memory use. Historical reports qualify only their recorded corpus, configuration, and hardware.

2. DEFINE EXCELLENT SEARCH BEHAVIOR

Support and evaluate:

- Exact lookup: names, identifiers, model numbers, quoted phrases, and other domain-specific entities.
- Semantic lookup: paraphrases, synonyms, descriptions of purpose, abbreviations, and vocabulary mismatch.
- Imperfect input: typos, incomplete queries, and multilingual wording where relevant.
- Structured constraints: dates, entities, locations, categories, numeric ranges, units, and domain-specific attributes.
- Negation and exclusions: preserve what the user explicitly rules out.
- Combined intent: satisfy every distinct need in a multi-part query.
- Refinement: apply follow-up instructions without losing previous explicit constraints; let users visibly remove them.
- Ambiguity: ask a concise clarification when materially different interpretations remain.
- Unsupported or absent information: return an honest no-match or limitation state.
- Freshness: additions, updates, deletions, and permission changes propagate correctly.

Use Gmail/Calendar as an experience benchmark. Do not claim Google-level quality without a valid comparative evaluation, or add email/calendar integrations solely because those products were named.

For the current procurement application, include examples such as:
- “Something comfortable to sit on at my desk”
- An exact manufacturer part number
- “Powder-free nitrile gloves, size M, in stock”
- “A laptop and a compatible monitor”
- A mixed-product request whose size/material constraint applies to only one product
- Equivalent requests in different supported languages

3. IMPROVE THE COMPLETE RETRIEVAL PIPELINE

Diagnose failures before choosing changes. Separate:
- Query interpretation failures
- Missing relevant candidates
- Poor final ranking
- Incorrect constraint handling
- Missing coverage of a distinct need
- Data/index quality problems

Compare lexical, vector, hybrid, and bounded reranking approaches on identical inputs and authorized populations.

Consider the following, retaining changes only when evidence supports them:

- Field-aware lexical retrieval with strong exact-identifier handling.
- Domain-appropriate embeddings and query/document formatting.
- Parallel independent retrieval channels.
- Rank fusion or calibrated score combination.
- A compact reranker over a bounded candidate set.
- Selective query expansion that preserves the original query.
- Typed query plans separating semantic intent, explicit filters, exclusions, and per-clause constraints.
- Canonical need identities so multilingual synonyms do not consume separate expansion budgets.
- Separate candidate budgets per need and coverage-aware composition for multi-part requests.
- Deduplication that preserves useful alternatives.
- Suitable document segmentation, metadata, and incremental indexing for the actual data.

Treat inferred categories and model judgments as soft guidance unless the user explicitly requests a hard filter. Validate model-produced plans deterministically.

Preserve authorization, exact identifiers, explicit sorting, mandatory constraints, and domain rules throughout retrieval and reranking. Apply access restrictions before exposing evidence to models, counts, facets, snippets, or explanations.

Keep browser, API, and MCP behavior consistent. Models must not invent records or compute authoritative commercial facts.

4. ENGINEER FOR THE 4–5 SECOND TARGET

Define completion as:
“The user has usable ranked results or a useful clarification, with the information required by the normal search flow rendered.”

A spinner, first streamed token, or early response followed by essential work does not count as completion. If an explanation is essential to the normal flow, include its completion time.

Measure:
- Browser submission to usable rendered results
- Complete backend response
- Queueing and authorization
- Query understanding
- Embedding
- Lexical/vector retrieval
- Fusion and reranking
- Facets and result projection
- Model loading, prefill, and decoding
- Validation and rendering

Propagate one end-to-end deadline. Bound queues, retries, candidate counts, inference concurrency, input size, and output length. Cancel obsolete searches and prevent background work from starving active requests.

Optimize the critical path:
- Precompute document embeddings during indexing.
- Keep latency-critical models resident when memory allows.
- Avoid loading a large generative model for every ordinary query.
- Use compact structured outputs for interpretation.
- Expand trusted statement/reference IDs in code instead of asking models to reproduce long fixed text.
- Parallelize independent work within measured resource limits.
- Scope caches to identity, permissions, filters, corpus, model, and configuration versions.
- Use a disclosed deterministic fallback when an enhancement times out.
- Report fallback separately from successful execution of the requested pipeline.

Provide a capable local search path. Identify any external inference dependency explicitly and measure it separately. Preserve optional remote enhancements without making their results evidence of fully local performance.

If using the current agentic-search implementation, investigate:
- Sequential lexical/vector retrieval
- CPU-default embeddings and supported Apple acceleration
- Shared embedding/reranker locks
- Lazy model loading and immediate unloading
- Sequential Jev scoring/expansion calls
- Repeated authorized-population and offer construction
- Multiple backend calls followed by synchronous assistant generation

These are profiling hypotheses, not proven bottlenecks or promised speedups.

5. QUALIFY BOTH MEMORY CONFIGURATIONS

Provide separate 48GB and 64GB deployment profiles.

Record exact model versions, quantization, runtime, context limits, residency policy, concurrency, and index configuration.

Choose models based on measured retrieval/task quality and latency. Parameter count, download size, and “fits in RAM” are insufficient selection criteria.

Benchmark supported Apple-native acceleration where applicable. Verify actual device execution rather than assuming Metal, MPS, or MLX guarantees better performance.

Measure total application/stack memory, whole-system memory pressure, and swap growth. Reserve practical headroom for macOS and a browser.

The 48GB profile must meet the same essential search-quality standard. The 64GB profile may offer additional capability only when measured benefits justify its resource use.

If only one hardware configuration is available, qualify that configuration and label the other unverified. A constrained run on a 64GB machine is not equivalent to validation on a 48GB machine.

6. BUILD A DEFENSIBLE EVALUATION

Create or extend a versioned benchmark with representative queries, hard negatives, graded relevance judgments, and task-specific expected outcomes.

Include exact lookup, paraphrases, typos, multilingual queries, filters, negation, ambiguity, no-match cases, multi-need requests, and permission boundaries.

Separate development, regression, and untouched holdout sets. Freeze corpus, configuration, models, labels, and acceptance criteria before final evaluation. A holdout used to guide tuning becomes development/regression evidence.

Seek independent domain review of labels. Clearly distinguish engineering labels from independently validated ground truth.

Measure:
- nDCG@10 for ranked relevance
- Candidate recall before reranking
- Recall@50, with label/pool limitations stated
- Exact-item success@1 and MRR@10
- Search-task success
- Coverage of every need in multi-part requests
- Clarification/no-match correctness
- Authorization and explicit-constraint violations
- p50/p95/p99 latency
- Requested-path completion, fallback, and errors
- Memory pressure and swap growth

Compare against the strongest applicable baseline and perform ablations. Report uncertainty, denominators, and failure slices.

Proposed initial acceptance goals, to freeze before holdout:
- p95 ≤5 seconds for complete warm, uncached normal searches at one active interactive request.
- Report concurrency 2 and 4 separately.
- ≥98% success@1 on eligible exact-identifier cases.
- ≥90% successful representative search tasks.
- ≥95% all-needs coverage on satisfiable multi-part tasks.
- Zero observed authorization or mandatory-constraint violations in the prescribed suite.
- Useful semantic-quality improvement without material regression in critical slices.

Treat these as engineering targets, not established results. Do not weaken them after seeing holdout outcomes. If a baseline already saturates a metric, improve the benchmark’s discrimination and examine task outcomes.

Run enough varied fresh requests to support percentile claims. Separate startup, first request, warm uncached, cached, and degraded responses. Include a sustained run to expose thermal or memory-related slowdown.

Preserve any stricter existing contractual latency requirement and report it separately.

7. DELIVER AND VERIFY

Deliver:
- A concise diagnosis with source references.
- Focused implementation changes.
- Reproducible launch and evaluation commands.
- 48GB and 64GB configuration profiles.
- Before/after relevance, latency, and memory results.
- A small set of representative successful and failing queries.
- Remaining limitations and unmet targets.
- A verified local browser URL when you run the application.

Use only authorized non-production data and environments. Preserve secrets and existing data. Never mention Codex or an AI assistant in Git commit messages.

Enable a new default pipeline only when quality, correctness, and latency evidence supports it. Otherwise retain the strongest qualified default and explain the remaining gap.

Start by identifying Asker’s implementation and measuring its baseline. Then prioritize the changes most likely to improve useful search results within the laptop’s latency and memory budget.
