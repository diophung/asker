# tools/load — k6 load + freshness + soak suites (M5)

The M5 scale/SLO verification layer. Three artifacts, on top of the wave-0
metrics and `tools/synthgen`:

| File | What |
|------|------|
| `query-load.js` | k6 query suite: a **stepped ramping-arrival-rate** profile that climbs request rate stage by stage to find the cluster's max, querying the gateway `/v1/search` for known rare tokens (`qzx…`) minted by `synthgen`. Thresholds enforce the **P90 ≤ 5000 ms** SLO and a small failed-request bound. Also runs the **soak** (a single fixed sub-max stage held for `SOAK_DURATION`). |
| `ingest-load.js` | k6 ingest-freshness harness: sustained uploads through the **full** pipeline (`/v1/upload` → connector-hub → Kafka → ingest → enrich → index → Vespa), stopwatching each doc's **edit→searchable** latency by polling `/v1/search`, asserting **P90 < 1800 s** (the 30-min freshness SLA). |
| `run-load.sh` | Orchestrator: seed a corpus via `synthgen`, mint a token, run the k6 suites, collect summary JSON, print a numbered **PASS/FAIL** + the headline numbers, and (opt-in) run the **2-hour soak** asserting zero failed requests and **no new dead-letters** (the authoritative zero-data-loss signal). |

The SLOs come from spec §2.7 / `docs/architecture.md` §5:
**query P90 ≤ 5000 ms (hard), ingest freshness ≤ 30 min, zero data loss.**

## Prerequisites

- **k6** on `PATH` (or `K6=/path/to/k6`). Install:
  <https://grafana.com/docs/k6/latest/set-up/install-k6/>.
- The dev stack up (`make dev-up`) or a deployed cluster reachable at
  `BASE_URL`/`KEYCLOAK_URL`/`VESPA_URL`.
- `go` (the orchestrator seeds via `go run ./tools/synthgen`), `python3`, `curl`.

## Run locally (small — fits the 8GB dev VM)

```sh
# Everything: seed a tiny corpus, token, query suite (10→100 RPS), ingest suite.
bash tools/load/run-load.sh
```

Defaults are deliberately small (20 tenants × 50 docs; query 10→100 RPS in
30 s holds; ingest 4 VUs for 5 m). Headline output:

```
== Headline numbers ==
  query P90 latency:            <ms> (SLO 5000 ms)
  max sustained RPS at P90<SLO: <RPS>
  ingest freshness P90:         <s> (SLO 1800 s)
```

Run one suite at a time directly (handy while iterating):

```sh
# Query only — supply a token or let setup() do the Keycloak password grant.
BASE_URL=http://localhost:8080 RPS_MAX=60 \
  k6 run tools/load/query-load.js

# Ingest/freshness only.
BASE_URL=http://localhost:8080 INGEST_VUS=2 INGEST_DURATION=2m \
  k6 run tools/load/ingest-load.js
```

## Run in CI / at scale (full)

Scale the corpus and the ramp via env. Point at the deployed stack (or a
port-forwarded gateway/keycloak/vespa) and a Prometheus-scrapeable index-writer
`/metrics` for the soak's data-loss check:

```sh
SYNTH_TENANTS=2000 SYNTH_DOCS_PER_TENANT=500 \
RPS_START=50 RPS_MAX=2000 RPS_STEP=50 STAGE_HOLD=60s \
INGEST_VUS=16 INGEST_DURATION=20m \
BASE_URL=https://gw.internal KEYCLOAK_URL=https://kc.internal \
VESPA_URL=http://vespa:8082 \
INDEX_WRITER_METRICS=http://index-writer:9701/metrics \
  bash tools/load/run-load.sh
```

The corpus is **deterministic** (same `SYNTH_SEED` + params ⇒ byte-identical),
so the query suite re-derives the rare-token range (`qzx00000000…`) without
observing the corpus. `RARE_TOKEN_COUNT` is auto-derived from the seed params
(`tenants × docs/tenant × rare_rate`) unless you pin it.

### The 2-hour soak (zero data loss)

```sh
SOAK=true SOAK_RPS=20 SOAK_DURATION=2h \
INDEX_WRITER_METRICS=http://index-writer:9701/metrics \
  bash tools/load/run-load.sh
```

The soak runs the query suite at a fixed sub-max rate for `SOAK_DURATION`,
asserting **0 failed requests** AND that `asker_pipeline_deadletter_total`
(summed across `origin_topic`, scraped from the index-writer `/metrics`) did
**not rise** — the authoritative zero-data-loss exit signal. If the runner
cannot reach `INDEX_WRITER_METRICS`, the data-loss check reports
INCONCLUSIVE (set the var to a reachable endpoint, e.g. a `kubectl port-forward`
to the index-writer health port `:9701`).

## How the "max sustained RPS at P90<5s" read-out works

`query-load.js` tags every request with its stage's target rate (`rps:NN`) and
registers a **non-aborting tagged threshold** per stage, which makes k6 emit a
per-stage sub-metric `http_req_duration{rps:NN}` (with its own `p(90)`) into the
`handleSummary` JSON. `run-load.sh` reads that JSON and reports the **highest
`rps` stage whose `p(90)` stayed < the SLO**. The aggregate
`http_req_duration` p(90) and `http_req_failed` thresholds are the PASS/FAIL
gate; the per-stage thresholds are loose-bound and exist only to materialize the
curve. (If a sink does not surface tagged sub-metrics, the orchestrator falls
back to reporting `aggregate-p90-ok`.)

## Extrapolation math → 50K TPS (the capacity hook)

This suite measures **max sustained query TPS at P90 < 5 s on the test cluster
of N nodes**. The capacity doc (`docs/capacity.md`, owned by the capacity-ci
builder) extrapolates to the 50K-TPS target. The intended hook:

```
per_node_tps      = max_sustained_tps / N_query_nodes        (measured here)
nodes_for_50k_tps = ceil(50000 / per_node_tps / headroom)    (headroom ~0.7)
```

Because the architecture is **streaming-mode, per-tenant** (no query spans
tenants; near-zero per-tenant cost at rest — `CLAUDE.md`), query throughput
scales ~linearly with stateless query/gateway replicas and Vespa content
groups, so the linear projection holds until a shared dependency (Vespa
container CPU, Redis, TEI for vector arms) saturates — which the stepped curve
surfaces as the stage where P90 crosses the SLO. Feed the measured
`max sustained RPS at P90<SLO` and `N` into `docs/capacity.md`.

## Environment variables

### `run-load.sh`

| Var | Default | Meaning |
|-----|---------|---------|
| `BASE_URL` / `GATEWAY_URL` | `http://localhost:8080` | gateway base URL |
| `KEYCLOAK_URL` | `http://localhost:8081` | Keycloak (password grant) |
| `VESPA_URL` | `http://localhost:8082` | Vespa document/v1 (synthgen seed) |
| `KC_USER` / `KC_PASS` | `alice` / `password123` | dev user for the token |
| `TOKEN` | (empty) | supply a bearer token to skip the grant |
| `INDEX_WRITER_METRICS` | `http://localhost:9701/metrics` | deadletter scrape (soak) |
| `K6` | `k6` | k6 binary |
| `RUN_QUERY` / `RUN_INGEST` | `true` / `true` | which suites to run |
| `SOAK` | `false` | run the soak instead of the stepped suites |
| `SEED_CORPUS` | `true` | seed via synthgen before running |
| `SYNTH_TENANTS` | `20` | seed tenants |
| `SYNTH_DOCS_PER_TENANT` | `50` | seed docs/tenant |
| `SYNTH_RARE_RATE` | `1.0` | rare-token rate (1.0 ⇒ every doc) |
| `SYNTH_SEED` | `1` | deterministic seed |
| `RPS_START`/`RPS_MAX`/`RPS_STEP` | `10`/`100`/`10` | stepped ramp |
| `STAGE_HOLD` | `30s` | hold per stage |
| `MODE` | `hybrid` | search mode (hybrid\|keyword\|vector) |
| `P90_SLO_MS` | `5000` | the hard query SLO |
| `FAIL_RATE_MAX` | `0.01` | failed-request bound |
| `INGEST_VUS` | `4` | concurrent upload VUs |
| `INGEST_DURATION` | `5m` | ingest suite duration |
| `FRESHNESS_SLO_MS` | `1800000` | 30-min freshness SLA |
| `SOAK_RPS` | `20` | fixed soak rate |
| `SOAK_DURATION` | `2h` | soak duration |
| `SOAK_FAIL_RATE_MAX` | `0.001` | k6 failed-rate gate for the soak (strict 0-failure is asserted separately from the summary) |
| `RARE_TOKEN_COUNT` | (derived) | rare-token sample size for the query suite |
| `OUTDIR` / `KEEP_OUTDIR` | (mktemp) | summary/log artifacts dir |

### `query-load.js` (also settable directly)

`BASE_URL`, `TOKEN` | (`KC_URL`,`KC_REALM`,`KC_CLIENT`,`KC_USER`,`KC_PASS`),
`MODE`, `QUERY_LIMIT`, `RARE_TOKEN_COUNT`, `RARE_TOKEN_BASE`, `RPS_START`,
`RPS_MAX`, `RPS_STEP`, `STAGE_HOLD`, `RAMP`, `PRE_VUS`, `MAX_VUS`, `P90_SLO_MS`,
`FAIL_RATE_MAX`, `CHECK_EXACT_HITS`, `SOAK`, `SOAK_RPS`, `SOAK_DURATION`,
`K6_SUMMARY_PATH`.

### `ingest-load.js`

`BASE_URL`, `TOKEN` | `KC_*`, `INGEST_VUS`, `INGEST_DURATION`,
`FRESHNESS_SLO_MS`, `POLL_INTERVAL_MS`, `POLL_TIMEOUT_MS`, `UPLOAD_FAIL_MAX`,
`STRICT_TIMEOUTS`, `RUN_ID`, `K6_SUMMARY_PATH`.

## Why these design choices

- **Open model (`ramping-arrival-rate`), not closed VUs:** an open model keeps
  launching iterations at the target rate even as the system slows, which is
  what reveals the throughput-vs-latency cliff. A closed VU model throttles
  itself when responses lag and hides the max.
- **Query path uses `vespa-direct` seeding; ingest path uses `gateway-upload`:**
  the query suite needs a large multi-tenant corpus fast (vespa-direct bypasses
  Kafka), while freshness must traverse the **whole** async pipeline (only the
  upload path does).
- **k6 for freshness, not bash:** k6 gives sustained concurrency, a P90 Trend,
  threshold-as-PASS/FAIL, and a machine-readable summary in one tool, and polls
  in the same VU that uploaded. The bash fallback (`synthgen --target
  gateway-upload` + `curl` polling, mirroring `tools/e2e/m1-e2e.sh wait_hits`)
  drives the same path but is noisier to measure; we chose k6 for the cleaner
  measurement.
- **Determinism:** the corpus is reproducible from the seed, so the query suite
  asserts against tokens it never had to observe (`CHECK_EXACT_HITS=true`
  upgrades that to a hit-count correctness gate under load).
- **Low-cardinality metrics only:** the suites NEVER label by tenant/doc/query
  (10M tenants would explode Prometheus cardinality — see the M5 shared facts);
  the only request tags are `mode` and the stage `rps`.

## Proposed Makefile targets (the integrator applies these — see the M5 issues)

```make
load-query: ## k6 query suite, stepped to cluster max (P90<5s SLO)
	RUN_INGEST=false bash tools/load/run-load.sh

load-ingest: ## k6 ingest freshness suite (edit->searchable P90<30min)
	RUN_QUERY=false bash tools/load/run-load.sh

soak: ## 2-hour soak: fixed-rate query, zero failed + no new deadletters
	SOAK=true bash tools/load/run-load.sh
```
