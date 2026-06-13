# synthgen — synthetic corpus generator (M5 scale/load/soak)

`synthgen` generates a **realistic, deterministic, mixed-type** synthetic corpus
across many tenants and loads it into Asker for M5 scale/load/soak testing. It is
a dev/CI tool only and never runs in production.

The defining property is determinism: **same `--seed` + same params ⇒
byte-identical corpus** (ids, titles, bodies, types, timestamps, rare tokens,
isolation markers), the same idea as `tools/fake-gmail/server/seed.go`. So the
query-load suite can assert *exact* hit counts against a corpus it never had to
observe — it re-derives the rare tokens and isolation markers from the seed.

## Why two layers (and why the tests need no live stack)

- **Generation layer** (`corpus.go`, `plan.go`): pure, no I/O. Produces
  `GenDoc`s and a dry-run `Plan`. This is what the unit tests exercise in-memory.
- **Feed layer** (`feed.go`): the `Feeder` interface (`vespaFeeder`,
  `gatewayFeeder`, and a test `fakeFeeder`). The generator never touches the
  network directly, so tests run with `fakeFeeder` and need no Vespa/gateway.
- **Runner** (`run.go`): generates one tenant at a time (low memory, fully
  deterministic) and fans the feed I/O across `--concurrency` workers, with
  progress reporting, a final summary, and checkpoint/resume.

## Targets

| `--target`        | What it does | Tenancy |
|-------------------|--------------|---------|
| `vespa-direct`    | POSTs each doc straight to Vespa `document/v1` into the tenant's streaming group `group/<tenant_id>`, byte-compatible with the index-writer's feed shape. Bypasses Kafka — for raw multi-tenant index-fill at scale. | **Multi-tenant** (each doc carries its own group). |
| `gateway-upload`  | POSTs each doc to the gateway's authed `/v1/upload` as a multipart file, exercising the **full** async pipeline (connector-hub → Kafka → ingest → enrich → index). | **Single-tenant** — `tenant_id` derives from the OIDC token (ADR-002), never the body, so all docs land in the token-owner's tenant. |

`tenant_id` is never read from a request body. `vespa-direct` writes each doc
into `group/<tenant_id>` (the streaming group **is** the tenant id); the
generated tenant ids are namespaced `synthgen-NNNNNNNN`.

## Realistic content

- **Doc-type mix** with weights: `--doc-types EMAIL:5,FILE:3,IMAGE:1,...`
  (names are the `Document` proto `DocType` enum: `EMAIL`, `FILE`,
  `CHAT_MESSAGE`, `CALENDAR_EVENT`, `WIKI_PAGE`, `TICKET`, `IMAGE`, `VIDEO`,
  `AUDIO`).
- **Per-tenant doc skew** (`--min-docs`/`--max-docs`/`--skew-large-fraction`):
  most tenants are small, a configurable fraction are large — the realistic
  long-tail shape. `--docs-per-tenant N` pins a uniform count instead.
- **Varied** title/body length and timestamps spread over the two years before
  a fixed anchor (so date-filter assertions can use absolute bounds).
- **`DocID = sha256(connector_id + ":" + source_native_id)`** — the platform
  convention (`connectors/sdk.DocID`), so re-feeds/deletes converge.
- **Rare tokens** (`--rare-token-rate`): a fraction of docs carry a unique
  `qzxNNNNNNNN` marker (the seed.go style). Tokens are the contiguous global
  ordinals `RareToken(0)..RareToken(R-1)`, each in **exactly one doc in exactly
  one tenant**, so a query-load suite can assert exact hit counts.
- **Isolation markers**: every doc of tenant *i* carries `IsolationMarker(seed,
  i)` (in body **and** `metadata.isolation`) and no other tenant's marker, so a
  leakage check can search one tenant's marker as another tenant and demand
  zero hits.

## Cardinality note (M5)

Generated tenant ids / doc ids are **data**, never metric labels — with up to
10M tenants a `tenant_id` label would explode Prometheus cardinality. synthgen
emits no metrics; it prints throughput/summary to stderr.

## Flags (all also settable via `SYNTHGEN_*` env; flags win)

| Flag | Env | Default | Meaning |
|------|-----|---------|---------|
| `--tenants` | `SYNTHGEN_TENANTS` | `100` | number of tenants |
| `--docs-per-tenant` | `SYNTHGEN_DOCS_PER_TENANT` | `0` | uniform docs/tenant (0 ⇒ use min/max skew) |
| `--min-docs` / `--max-docs` | `SYNTHGEN_MIN_DOCS` / `_MAX_DOCS` | `5` / `500` | per-tenant doc range (skew) |
| `--skew-large-fraction` | `SYNTHGEN_SKEW_LARGE_FRACTION` | `0.1` | fraction of large tenants |
| `--doc-types` | `SYNTHGEN_DOC_TYPES` | `EMAIL:5,FILE:3,CHAT_MESSAGE:2,IMAGE:1,WIKI_PAGE:1,CALENDAR_EVENT:1` | weighted mix |
| `--seed` | `SYNTHGEN_SEED` | `1` | deterministic seed |
| `--rare-token-rate` | `SYNTHGEN_RARE_TOKEN_RATE` | `0.01` | fraction of docs with a `qzx` token |
| `--target` | `SYNTHGEN_TARGET` | `vespa-direct` | `vespa-direct` \| `gateway-upload` |
| `--vespa-url` | `SYNTHGEN_VESPA_URL` | `http://localhost:8082` | Vespa document/v1 base URL |
| `--gateway-url` | `SYNTHGEN_GATEWAY_URL` | `http://localhost:8080` | gateway base URL |
| `--token` | `SYNTHGEN_TOKEN` | (empty) | OIDC bearer (gateway-upload) |
| `--concurrency` | `SYNTHGEN_CONCURRENCY` | `8` | concurrent feed workers |
| `--dry-run` | `SYNTHGEN_DRY_RUN` | **`true`** | print the plan + estimated bytes and generate NOTHING |
| `--checkpoint` | `SYNTHGEN_CHECKPOINT` | (empty) | checkpoint file for `--resume` |
| `--request-timeout` | `SYNTHGEN_REQUEST_TIMEOUT` | `30s` | per-request HTTP timeout |
| `--progress-every` | `SYNTHGEN_PROGRESS_EVERY` | `5s` | progress interval (0 disables) |

> **`--dry-run` defaults to `true`** so the tool never feeds a stack by
> accident; pass `--dry-run=false` to actually generate and load.

## `--dry-run`: plan a TB-scale corpus on the 8GB dev VM

`--dry-run` prints the plan (tenants, total docs, **estimated bytes**, the exact
rare-token range, the doc-type mix, a sample doc, the isolation-marker bounds)
and generates **nothing** — so a "100K tenants, TB-scale" plan is demonstrable
on the small dev VM in ~2s:

```sh
go run ./tools/synthgen \
  --tenants 100000 --min-docs 10 --max-docs 8000 \
  --skew-large-fraction 0.05 --rare-token-rate 0.001 --dry-run
# == synthgen plan (DRY RUN — nothing generated) ==
#   tenants:         100000
#   total docs:      ~221,000,000
#   est. feed bytes: ~215 GiB
#   rare tokens:     ~221,000 (qzx00000000..)
#   ...
```

(For literal TB, scale `--max-docs` / `--tenants` up; the estimate is linear.)

## Tiny real run

```sh
# vespa-direct, multi-tenant (needs a running Vespa or a fake on :8082)
go run ./tools/synthgen \
  --tenants 5 --docs-per-tenant 10 --rare-token-rate 0.2 \
  --target vespa-direct --vespa-url http://localhost:8082 \
  --checkpoint /tmp/synth.ckpt --dry-run=false

# gateway-upload, full pipeline, single tenant
TOKEN=$(...password grant...)  # see tools/e2e/m1-e2e.sh fetch_token
go run ./tools/synthgen \
  --tenants 1 --docs-per-tenant 100 \
  --target gateway-upload --gateway-url http://localhost:8080 --token "$TOKEN" \
  --dry-run=false
```

## Resume / checkpoint

With `--checkpoint PATH`, synthgen saves progress (atomically) at each **tenant
boundary**: the last completed tenant index and the global rare-token base. A
restart resumes at the next tenant and **reproduces the same rare-token
ordinals** (the index is append-only across tenants). Resuming against a
checkpoint built with a different spec is rejected (the corpus would diverge).
`Ctrl-C`/`SIGTERM` stops cleanly with the checkpoint saved.

## Tests

`go test ./tools/synthgen/...` — table-driven, no live stack:
- determinism (same seed ⇒ identical corpus; different seed ⇒ different);
- doc-type mix holds within tolerance; only configured types appear;
- rare-token rate + corpus-wide uniqueness + contiguous ordinals;
- isolation markers correct (own marker present, no foreign marker leaks,
  pairwise distinct);
- `DocID` matches the platform `sdk.DocID` convention;
- per-tenant skew distribution; uniform `--docs-per-tenant`;
- dry-run plan counts are exact and a 100K-tenant plan is cheap;
- checkpoint round-trip + resume reproduces rare tokens, no re-feed, spec-
  mismatch rejection;
- the runner feeds the whole corpus via `fakeFeeder`, honoring concurrency and
  context cancellation.

## Proposed Makefile target

`synthgen` does not edit the Makefile. Proposed targets (see the M5 issues):

```make
synthgen-dry: ## Print a TB-scale synthgen plan (generates nothing)
	go run ./tools/synthgen --tenants $${TENANTS:-100000} \
	  --min-docs $${MIN_DOCS:-10} --max-docs $${MAX_DOCS:-8000} \
	  --rare-token-rate $${RARE:-0.001} --dry-run

synthgen-load: ## Generate+load a synthetic corpus (override flags via env)
	go run ./tools/synthgen --dry-run=false $$SYNTHGEN_ARGS
```
