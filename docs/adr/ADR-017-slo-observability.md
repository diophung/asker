# ADR-017: M5 SLO verification & observability — low-cardinality metrics, scrape-based, opt-in

## Status

Accepted (M5). Builds on ADR-014 (cloud-agnostic Helm) and ADR-016 (default-deny NetworkPolicy);
operationalizes the latency budget and degradation ladder in architecture.md §5 and the scale
model in [`docs/capacity.md`](../capacity.md).

## Context

M0–M4 made the system *run* and *deploy*; M5 must *prove* it meets its SLOs at scale and keep
proving it. The spec's M5 line is: "k6 load suites for query (stepped to the cluster's max, with
documented extrapolation math to 50K TPS) and ingest (prove 30-min freshness under load); …
Grafana SLO dashboards + alert rules. *Exit: load report committed showing P90 latency and
freshness SLAs met at test scale, with the scaling math to target scale; 2-hour soak with zero
data loss.*"

The hard SLOs are: **query P90 ≤ 5000 ms** end-to-end (P50 ≤ 800 ms design target); **ingest
freshness ≤ 30 min** (source edit → searchable); **zero data loss** (the soak exit signal). The
M4 architecture is otherwise hostile to naive observability in one specific way that dominates
every decision here: **the data is ~10M strictly-private per-tenant corpora** (architecture.md
§1), and tenancy is sacred — nothing in the observability layer may weaken isolation or explode
in cost with the tenant count.

Open questions: what metric set proves the SLOs without per-tenant cardinality blowing up
Prometheus; push vs. scrape given the default-deny NetworkPolicy (ADR-016); how to express
end-to-end freshness when no single component sees the whole path; how to make "zero data loss"
a measurable signal; how the degradation ladder (architecture.md §5) shows up in metrics; and how
to ship all of this **without breaking the 8GB dev VM** that `make dev-up` already barely fits.

Wave 0 already instrumented the services with OTel metrics exposed in Prometheus format. This ADR
records the decisions those metrics embody and the layer (dashboards, alerts, load suites, CI)
built on top.

## Decision

### 1. Low-cardinality metrics only — NO tenant_id / doc_id / query labels

The metric set carries **only low-cardinality labels** (mode, degraded, cache, result, stage,
topic, doc_type, HTTP method/status-class). There is deliberately **no `tenant_id`, `doc_id`, or
query-text label.** With up to 10M tenants, a `tenant_id` label would create up to 10M time series
per metric — a Prometheus cardinality explosion that would dwarf the data it measures and, worse,
would put tenant identifiers into a shared monitoring system, eroding the isolation that is the
product's whole point. Per-tenant usage accounting, when needed, belongs in the control-plane
(Postgres, queryable per tenant on demand), **not** in fleet metrics. Generated tenant/doc ids
from `tools/synthgen` are likewise **data, never labels**.

The canonical set (Prometheus names after OTel suffixing):

- `asker_query_search_duration_milliseconds` — histogram; labels `mode`
  (hybrid|keyword|vector), `degraded` (none|keyword-only|clip-unavailable|both), `cache`
  (hit|miss). Buckets (ms): 5,10,25,50,100,250,500,1000,2500,5000,10000,+Inf — the **5000 ms
  bucket is the hard-SLO line**. P90 = `histogram_quantile(0.9, sum by(le)(rate(..._bucket[…])))`.
- `asker_query_cache_requests_total` — counter; label `result` (hit|miss). Cache hit rate sizes
  the TEI pool (capacity.md §2).
- `asker_query_degradation_events_total` — counter; label `rung` (keyword-only|clip-unavailable).
- `asker_pipeline_records_total` — counter; labels `stage`, `topic`, `result` (ok|error); the
  **index-writer** stage additionally carries `doc_type` (it has the typed `Document` at index
  time; the ingest stage emits only `stage`/`topic`/`result`). `result=error` is **per-attempt**
  (kafkautil retries 3×), the handler attempt-fail rate — **not** a data-loss signal.
- `asker_pipeline_stage_duration_milliseconds` — histogram; labels `stage`, `topic`.
- `asker_index_doc_age_seconds` — histogram; labels `stage`, `doc_type`. Buckets (s):
  1,5,10,30,60,300,600,1800,3600,21600,86400,+Inf — the **1800 s bucket is the 30-min SLA line**.
- `asker_pipeline_deadletter_total` — counter; label `origin_topic`. The **authoritative
  zero-data-loss signal**.
- `http_server_requests_total` / `http_server_request_duration_seconds` — RED metrics, otelhttp
  semconv labels (method, status-class).
- `go_*` / `process_*` — standard runtime collectors.

### 2. Prometheus SCRAPE (pull), not push — `/metrics` on each service's health port

Each instrumented service serves `GET /metrics` on its **health port** — gateway `:8080`,
query `:9201`, ingest `:9501`, index-writer `:9701` (control-plane `:9101` and connector-hub
`:9301` are out of scope for now). Prometheus scrapes these; nothing pushes. Pull is chosen
because it gives Prometheus liveness-by-scrape (a service that stops being scrapeable is itself a
signal), needs no per-service push credentials, and matches the K8s-native ecosystem. The cost is
that under the ADR-016 **default-deny NetworkPolicy**, a Prometheus → health-port **ingress allow
per scraped service is required** (Prometheus is a new in-cluster peer the M4 matrix did not
include); this is wired in the observability templates and noted for the integrator to fold into
the ADR-016 allow matrix. Compose service DNS equals the bare service name; in K8s the Service
names match.

### 3. End-to-end freshness via a doc-age proxy — and its honest limitation

Freshness (source edit → searchable ≤ 30 min) has no single component that observes the whole
path, so it is proven with `asker_index_doc_age_seconds` = `now − Document.created_at` measured
**at index time** (the moment the document becomes searchable). The P90 of this histogram, and the
fraction landing in the ≤1800 s bucket, are the SLO panels. **Limitation, recorded so it cannot be
forgotten:** this is an **in-cluster** proxy — it covers connector-emit → searchable but *not* the
time a source took to deliver the edit to the connector (webhook lag, the polling interval). The
30-min SLA is whole-path; this metric proves the part Asker controls. The load suite additionally
stopwatches a tagged source edit through to a successful query for the whole-path number in the
report.

### 4. Zero data loss = the dead-letter counter, distinct from per-attempt errors

The soak exit criterion is "zero data loss." The authoritative signal is
`asker_pipeline_deadletter_total`: a record is only counted there once it has exhausted
kafkautil's retries and been quarantined to `docs.deadletter` (ADR-004) — i.e. *actually* dropped
from the live pipeline. `increase(asker_pipeline_deadletter_total[2h]) == 0` over the soak is the
pass condition. This is deliberately **not** `asker_pipeline_records_total{result="error"}`, which
counts *per-attempt* handler failures that retries recover; conflating the two would either hide
real loss or fail a healthy run on transient retries.

### 5. The degradation ladder is a first-class SLO dimension, not a hidden fallback

Architecture.md §5's ladder (drop rerank, then drop vector → keyword-only; never fail closed) is
made observable: every query records its `degraded` label on the latency histogram, and each
demotion increments `asker_query_degradation_events_total{rung}`. This makes "we met P90 by
silently degrading" *visible* — a spike in degradation events alongside healthy latency is itself
an alert, so the SLO is "fast **and** not silently degraded," not just "fast." The load suite
exercises the ladder explicitly (force TEI/clip down) and the report records the rung breakdown.

### 6. Observability is OPT-IN — it must not break the 8GB dev VM or the default render

The default `make dev-up` stack already barely fits the 8GB dev VM (PROGRESS.md). Therefore:

- **Compose:** Prometheus/Grafana ship in a **separate** `docker-compose.observability.yml`,
  **not** started by default — run with `-f` both files (or a profile). Both are memory-capped
  small. Dev Grafana admin credentials are **dev-only**, loudly marked, bound to 127.0.0.1.
- **Helm:** the prom/grafana templates are gated behind `observability.enabled` (**default
  `false`**), so the default chart render is byte-unchanged from M4 and the kubeconform/helm-lint
  surface only grows when the feature is turned on.
- **Cloud-agnostic (ADR-014):** no provider-specific resources; Grafana datasource is the
  in-cluster Prometheus (`uid "prometheus"`, `url http://prometheus:9090`); dashboards reference
  it by uid only.

### 7. The full load + soak run in CI (heavy → not on every PR)

The k6 query/ingest suites (`tools/load/`), seeded by `tools/synthgen`, and a parameterized soak
run in a **dedicated workflow** (`.github/workflows/load.yml`) on **`workflow_dispatch` + a nightly
schedule** — never on every PR (it brings up the full stack and is expensive). It mirrors ci.yml's
e2e jobs (TEI `bge-small`, `EMBEDDING_DIM=384`), seeds the corpus, runs the suites, runs a
**short** soak by default (`SOAK_DURATION` input) with the note that the **real 2-hour soak is run
on demand**, and uploads the k6 summaries + the filled [`loadtest-report.md`](../loadtest-report.md)
as artifacts. The committed report (a real run) is the M5 exit artifact.

## Consequences

- **Cardinality is bounded by design.** Every metric's series count is the product of a handful of
  low-cardinality labels, independent of the 10M tenants — Prometheus stays cheap at any tenant
  scale, and no tenant identifier ever enters the monitoring system. The price: **no per-tenant
  drill-down in Grafana**; per-tenant questions go to the control-plane DB, on demand, behind the
  same tenancy chokepoint as everything else. Accepted.
- **A new in-cluster peer (Prometheus) widens the ADR-016 allow graph.** Default-deny means
  Prometheus cannot scrape until each service's health port has a Prometheus→port ingress allow.
  This is additive to the ADR-016 matrix; the integrator reconciles the per-service allow into the
  netpol templates. Forgetting it shows up immediately as a `down` target, not as silent loss.
- **Freshness is proven for the in-cluster path only by the metric; whole-path needs the
  stopwatch.** The doc-age proxy cannot see source→connector latency; the report's stopwatch
  closes that gap for the test, and the limitation travels with the metric definition.
- **"Zero data loss" is a crisp, single counter** rather than an inference from error rates —
  cheap to alert on and unambiguous at the soak boundary.
- **Degradation can no longer hide.** Meeting P90 by degrading is visible and alertable; the SLO
  is dual (fast and not silently degraded).
- **The default footprint is unchanged.** Opt-in compose file + `observability.enabled=false`
  default mean the 8GB dev VM and the default Helm render are untouched until observability is
  deliberately enabled; CI's heavy load/soak lives in its own dispatch/nightly workflow, off the
  PR path.
- **The scale model is now measurement-driven.** capacity.md's formulas consume exactly these
  metrics (search-duration histogram → `measured_TPS_per_*_node`; doc-age histogram → freshness;
  cache-requests → TEI sizing), so the extrapolation to 50K TPS / 10PB is reproducible from a CI
  run rather than asserted. The constants are filled in loadtest-report.md.
- **Interface additions for the integrator:** the `observability.enabled` Helm value (default
  false) and the gated prom/grafana templates; `deploy/compose/docker-compose.observability.yml`
  (opt-in); the Prometheus→health-port netpol allow per scraped service (fold into ADR-016's
  matrix); and the `tools/load/` k6 suites + `.github/workflows/load.yml` (this ADR's CI run). The
  exact owned-file layout is fixed in the M5 wave-1 plan.
