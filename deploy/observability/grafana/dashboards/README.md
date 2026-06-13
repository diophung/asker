# Grafana dashboard JSON — moved into the chart

The committed SLO dashboard JSON now lives in the Helm chart at
**`deploy/helm/asker/files/observability/grafana/dashboards/*.json`** (single
source of truth), so the chart can embed it via `.Files.Glob` (Helm cannot read
files outside the chart directory). The compose overlay
(`deploy/compose/docker-compose.observability.yml`) mounts that same chart
directory at `/var/lib/grafana/dashboards`, and the file provider in
`../provisioning/dashboards/dashboards.yml` loads it — so dashboards are
identical across compose and Kubernetes.

The dashboards: query latency P50/P90 vs the 5 s line, cache hit rate, degradation
ladder, ingest freshness vs the 30-min line, dead-letter / data-loss, per-stage
throughput + error rate, Go/process runtime.

Every dashboard selects the Prometheus datasource by **uid `prometheus`** (see
`../provisioning/datasources/datasource.yml`) and uses ONLY the metric names +
low-cardinality labels documented in ADR-017.
