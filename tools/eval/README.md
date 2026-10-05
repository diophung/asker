# Asker retrieval evaluation

This harness sends real `GET /v1/search` requests and measures ranked relevance,
complete-response latency, and search-task outcomes. **Pre-rerank head recall**
is measured only when complete actual head IDs arrive in trusted debug telemetry.
Recall over the full eligible retrieval population remains unmeasured: neither
a final hit list nor a bounded reranker head establishes that quantity. It compares `lexical`,
`dense`, `hybrid`, and explicitly requested `hybrid_rerank` on identical labeled
queries. Labels are engineering judgments until independently adjudicated.
Passing a small synthetic suite does not establish Google parity or performance
on a large personal corpus.

## What is measured

- Recall@k, nDCG@k, and MRR@k over positive judged tasks; failed requests score
  zero. No-match tasks have a separate task outcome, not invented relevance.
- Task success@k requires a relevant hit, every explicitly required need, and
  no forbidden document anywhere in the returned results. No-match success
  requires an empty result. Exact-item success@1 has its own denominator.
- HTTP p50/p95/p99 through **complete body read and JSON decode**, including
  failed requests. Browser rendering and normal token acquisition are outside
  this measurement; verify the browser separately before claiming the full
  user interaction meets its target.
- Attempted/expected/scored counts, errors, cache hits, degradation, constraint
  violations, requested/applied reranking, missing execution telemetry, and
  per-query measurements identified by stable IDs.
- Pre-rerank head recall with measured/unmeasured positive-sample denominators.
  Evaluation explicitly requests `debug=1`; that response overhead is included
  in latency. The server emits bounded telemetry only for active reranking.
  Complete ordered IDs are required (`scope=pre_rerank_head`, `complete=true`,
  unique IDs, length=depth, count≥depth). Missing, partial, malformed or
  oversized telemetry stays unmeasured. A complete empty head measures zero
  recall on positive tasks; no-match tasks are excluded. Actual candidate IDs
  are not retained in reports. Partial measurement is not a whole-run claim.

The default request sends `Cache-Control: no-cache`. The gateway/query service
must bypass both cache lookup and insertion. Reports retain unexpected cache
hits; they fail uncached qualification. Run a cached experiment separately with
`-no-cache=false`, and keep that result distinct from warm uncached latency.

## Qualification gate

By default the candidate must:

- Complete every expected task and slice with no errors, degraded execution,
  forbidden results, or unfulfilled/unverified requested reranking.
- Match or improve each slice's nDCG and task success, and strictly improve
  overall nDCG against the baseline. An explicitly recorded `-allow-equal`
  permits equality for a saturated baseline; it does not prove improvement.
- Achieve p95 ≤5000 ms, task success ≥0.90, exact-item success@1 ≥0.98,
  and all-needs coverage ≥0.95 on explicitly labeled multi-need tasks.
- Supply at least 100 candidate latency samples and at least one exact task
  and one explicitly labeled multi-need task. Missing eligible task groups fail
  rather than silently skipping their criterion.

Unsupported, missing, empty, or skipped qualification fails closed with exit
status 1, while preserving the report. Invalid inputs exit 2. Criteria and the
full run configuration are saved with the report; freeze them before holdout.
Threshold flags support a different predeclared contract. Do not relax them
because a holdout failed.

## Local-v1 retained as regression evidence

The v1 holdout was consumed on 2026-10-01. All v1 fixture bytes remain unchanged,
but **every v1 split is now regression evidence**, including the file named
`holdout.jsonl`. It cannot establish unseen-task generalization again. V2 adds a
more discriminating benchmark; its first holdout was also consumed on
2026-10-04. Preserve both versions and use a new sealed benchmark for another
unseen-task qualification claim.

`fixtures/local-v1/` contains 36 deterministic synthetic documents across mail,
calendar, files, and chat, with positive/hard-negative pairs and **13 queries per
split**. The tasks cover identifiers, filenames, keywords, paraphrases, Spanish,
typos, source/sender/event-date filters, exclusions, multiple needs, and no-match
cases. Calendar event creation and occurrence dates deliberately differ.

- `dev.jsonl`: tuning inputs and results may be inspected.
- `regression.jsonl`: fixed regression checks after changes.
- `holdout.jsonl`: run only after implementation/configuration/criteria freeze;
  never use results to tune. A reused holdout becomes regression evidence.
- `documents.jsonl`: synthetic feed inputs with stable fixture IDs.
- `manifest.json`: version, label provenance, scope, and SHA-256 fingerprints.

`fixtures/build_fixture.py` deterministically reproduces the authored fixture.
Do not regenerate labels to fit retrieval results. Changes require a new
benchmark version and fresh holdout. The suite does not provide complete ACL,
index-freshness, live-source, or large-corpus qualification.

## Seed into an already running local stack

`seed_fixture.py` never starts services and defaults to a read-only dry-run. It
supports the existing TEI-compatible native `POST /embed` endpoint and direct
Vespa document APIs. It refuses remote endpoints, credentials in URLs, missing
ports, redirects, and environment HTTP proxy routing.

For writes, the **explicit tenant must equal the identity verified by the
running gateway's authenticated `GET /v1/me`**. An unsigned JWT decode is never
used to authorize a tenant. Only prefixed `asker-eval-v1-*` IDs are upserted;
existing real documents are not deleted. All embeddings must validate against
the requested 384- or 1024-dimensional deployed schema before any document is
written. Direct feeding validates retrieval, not connector ingestion.

```sh
python3 tools/eval/seed_fixture.py --dimension 1024

# Private token file contains the bearer token, without a "Bearer " prefix.
# Obtain the verified tenant from the authenticated local /v1/me response.
python3 tools/eval/seed_fixture.py --write \
  --gateway http://localhost:18080 \
  --embedding http://localhost:18083 \
  --vespa http://localhost:18082 \
  --dimension 1024 --tenant '<verified-tenant-id>' \
  --principal alice --token-file /private/path/eval-token.txt
```

Alternatively set `ASKER_EVAL_TOKEN` in the environment. Never commit tokens.
`--principal` is only the golden-file token label; it cannot select the tenant
being written. Resolved split files and `seed-summary.json` are written under
`tools/eval/reports/local-v1-seeded/` by default.

## Run and retain reports

```sh
# Development: inspect these results to diagnose failures.
go run ./tools/eval \
  -golden tools/eval/reports/local-v1-seeded/dev.jsonl \
  -gateway http://localhost:18080 \
  -oidc http://localhost:18081/realms/asker/protocol/openid-connect/token \
  -corpus-manifest tools/eval/fixtures/local-v1/manifest.json \
  -hardware 'observed chip and memory' \
  -model 'observed model IDs, revisions, quantization and runtime' \
  -repeats 8 -concurrency 1 -out tools/eval/reports/dev
```

Regression after implementation changes (same unchanged criteria):

```sh
go run ./tools/eval \
  -golden tools/eval/reports/local-v1-seeded/regression.jsonl \
  -gateway http://localhost:18080 \
  -oidc http://localhost:18081/realms/asker/protocol/openid-connect/token \
  -corpus-manifest tools/eval/fixtures/local-v1/manifest.json \
  -hardware 'observed chip and memory' -model 'observed model IDs/revisions/runtime' \
  -repeats 8 -concurrency 1 -out tools/eval/reports/regression
```

The following command reproduces the retained v1 split named `holdout`; it is
regression evidence now. For a new benchmark, freeze implementation,
model/configuration and acceptance thresholds before its first holdout command,
and preserve that first report, including failures:

```sh
go run ./tools/eval \
  -golden tools/eval/reports/local-v1-seeded/holdout.jsonl \
  -gateway http://localhost:18080 \
  -oidc http://localhost:18081/realms/asker/protocol/openid-connect/token \
  -corpus-manifest tools/eval/fixtures/local-v1/manifest.json \
  -hardware 'observed chip and memory' -model 'observed model IDs/revisions/runtime' \
  -repeats 8 -concurrency 1 -out tools/eval/reports/holdout
```

For separate warm uncached latency/load reports, repeat the development command
with `-repeats 24 -concurrency 2 -out tools/eval/reports/latency-c2`, then with
`-repeats 24 -concurrency 4 -out tools/eval/reports/latency-c4`. The c1 command
above remains the 5-second acceptance target; report c2/c4 outcomes separately.
Keep model warmup, startup/first request, cached runs, and sustained runs in
separate artifacts. Do not count a degraded response as execution of a requested
reranker: `rerank_requested` means intent, while `rerank_applied` must reflect
actual per-request execution. Empty no-match results need no reranker call;
nonempty requested results with missing/false execution fail the gate.

The existing `make eval EVAL_ARGS="..."` target also works. Defaults use the
ordinary gateway/Keycloak ports 8080/8081; the example uses isolated local ports.
The password grant resolves `tenant` as the dev username and refreshes expiring
access tokens for sustained runs. A private
`-tokens /private/path/tokens.json` maps those labels to bearer tokens instead.

13 independent judged tasks repeated eight times yield 104 latency samples per
pipeline, **not 104 independent search tasks**. Repeating a tiny corpus is not a
scale benchmark. Warm the required model separately and record startup/first
request measurements separately; this harness does not hide warmup samples.
Use `-concurrency 2` and `-concurrency 4` in separate reports. `-pipelines` selects
a comma-separated subset; the configured gate baseline/candidate must exist.
`-split` optionally selects a declared split in a combined golden file.

The CLI verifies all frozen local-v1 file hashes before a run when its manifest
is supplied. Reports include original golden-file SHA-256, optional corpus-manifest SHA-256,
OS/architecture/Go version, operator-supplied hardware/model identity, criteria,
aggregates, and raw measurements without tokens or query text. Hardware/model
flags record observations; the harness does not independently prove their
identity. Successful service tests do not qualify either 48GB/64GB hardware
configuration or sustained thermal/memory behavior.

## Golden JSONL format

The original `id`, `query`, `tenant`, `slice`, `relevant`, `gains`, `mode_hint`,
and `note` fields remain compatible. Blank lines and `#` comments are accepted.
Optional extensions are:

```json
{"id":"final","query":"release checklist -draft","tenant":"alice","slice":"negation","split":"dev","relevant":["final-doc"],"forbidden":["draft-doc"],"filters":{"types":"FILE"}}
{"id":"absent","query":"\"missing-identifier\"","tenant":"alice","slice":"no_match","no_match":true}
{"id":"two-needs","query":"approval and recovery","tenant":"alice","slice":"multi_need","relevant":["approval","recovery"],"required":[["approval"],["recovery"]]}
```

`filters` allow only `types`, `participant`, `from`, and `to`; they become
explicit REST parameters. `required` lists per-need alternatives. `forbidden`
IDs must not also have positive relevance. `no_match` must not require positive
hits. Gains are finite nonnegative numbers. Unknown fields, duplicate record
IDs, contradictory labels, unsupported filters, and invalid splits fail loudly.

## Local tests

```sh
go test -race ./tools/eval
go vet ./tools/eval
python3 -m unittest discover -s tools/eval -p 'test_*.py' -v
```

Tests exercise complete-body timing, truncated responses, cache/execution
telemetry, unavailable/incomplete/error gates, repeated concurrent denominators,
constraint/no-match outcomes, frozen fixture integrity, tenant verification,
redirect guards, tensor dimensions, and scoped direct feeding through a local
HTTP test server. They do not run the live stack.

## Frozen local-v2 qualification

`fixtures/local-v2/` preserves the more discriminating October 4 benchmark. Its
first holdout was consumed on **2026-10-04**: development passed, while regression
and first holdout failed the unchanged gate against dense. All v2 splits are now
retained regression evidence; none can establish unseen-task generalization
again. The fixture contains **147 synthetic documents and 38 development,
39 regression, and 40 originally sealed holdout tasks**. Every split covers all
nine slices. All splits search the same shared
corpus, so cross-topic items and deliberately hard distractors compete with
positives. The evidence covers mail, files, chat, and calendar; identifiers,
filenames, graded relevance, paraphrases, three non-English languages, spelling
errors, exclusion constraints, combined needs with alternative documents,
empty filter intersections, and event-occurrence dates distinct from creation.
These are engineering labels; independent domain adjudication remains pending.

The labels use **complete source bodies**. The separate `evidence.jsonl` records
exact source excerpts and offsets, including evidence past the first 1,000 body
characters. A label means the source answers the task; it does not mean a
returned preview shows that evidence. ID relevance scores cannot establish
snippet sufficiency, full-body input to a reranker, or answer faithfulness.
Direct seeding represents each body as one text chunk and embedding; it does
not validate connector ingestion, production chunking, ACL completeness,
large-corpus behavior, or Google parity. Pre-rerank head recall requires complete
actual head telemetry; final hits cannot supply that measurement. Full
retrieval-population recall is unmeasured.

For a future untouched benchmark, treat `holdout.jsonl`, **the holdout portion
of `evidence.jsonl`, and its builder as sealed label sources while tuning**.
Only inspect development/regression tasks before its first frozen run.
Structural validation may read all splits
to check schemas, IDs, counts, fingerprints and evidence offsets; its output
contains no query, judgment, or excerpt text. No retrieval run is part of that
validation.

`criteria.json` and the manifest freeze k=10, limit=20, p95 ≤5,000 ms, task
success ≥0.90, exact success@1 ≥0.98, all-needs coverage ≥0.95, at least 100
uncached request samples, and concurrency 1 qualification. Strict overall nDCG
improvement and no per-slice nDCG/task regression remain required; errors,
degradation, constraints, missing execution telemetry and unfulfilled requested
reranking fail qualification. Select the strongest eligible development baseline
and freeze the compared pipelines and model/configuration before the final
holdout run. The CLI rejects changed v2 criteria, incomplete/edited task lists,
and any resolved golden-file changes beyond the tenant token label. A diagnostic
run with changed cutoffs/load/equality is a different experiment and cannot use
the v2 manifest to claim frozen qualification.

```sh
# Read-only structure/evidence validation; no inference or service writes.
python3 tools/eval/validate_fixture.py
python3 tools/eval/seed_fixture.py --fixture tools/eval/fixtures/local-v2

# Explicitly verified local tenant; writes only asker-eval-v2-* documents.
python3 tools/eval/seed_fixture.py --fixture tools/eval/fixtures/local-v2 --write \
  --gateway http://localhost:18080 --embedding http://localhost:18083 \
  --vespa http://localhost:18082 --dimension 1024 \
  --tenant '<verified-tenant-id>' --principal alice \
  --token-file /private/path/eval-token.txt

# Development comparison: baseline/candidate are choices, not measured claims.
go run ./tools/eval \
  -golden tools/eval/reports/local-v2-seeded/dev.jsonl \
  -gateway http://localhost:18080 \
  -oidc http://localhost:18081/realms/asker/protocol/openid-connect/token \
  -corpus-manifest tools/eval/fixtures/local-v2/manifest.json \
  -baseline dense -candidate hybrid \
  -hardware 'observed chip and memory' -model 'observed IDs/revisions/runtime' \
  -repeats 3 -concurrency 1 -out tools/eval/reports/v2-dev
```

Repeat that command with `regression.jsonl` and `-out tools/eval/reports/v2-regression`
after changes. Repeating it with `holdout.jsonl` reproduces consumed v2 regression
evidence. For a future untouched version, freeze implementation, source, models,
configuration, pipeline selection and acceptance before its first holdout run;
retain that first report, including failures, and do not use it to tune. Reuse
requires a fresh benchmark for another unseen-task claim. Three repetitions produce
114/117/120 HTTP samples per pipeline; they remain 38/39/40 independent judged
tasks. This small fixture cannot qualify personal-corpus scale or thermal/load
behavior. Qualify browser rendering and observed 48GB/64GB hardware separately.

The version-specific seed output defaults to `reports/local-v2-seeded/`, without
overwriting v1 resolved files. Generated reports remain local under the eval
ignore rule. Existing report files are preserved; export deliberately selected,
reviewed and redacted evidence to a repository-visible documentation directory.
The deterministic v2 builder refuses to change
existing authored bytes: changes need a new version, never label rewrites fitted
to retrieval outcomes.
