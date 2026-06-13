# Prometheus alert + recording rules — moved into the chart

The committed SLO rule groups now live in the Helm chart at
**`deploy/helm/asker/files/observability/prometheus/rules/*.rules.yml`** (single
source of truth), so the chart can embed them via `.Files.Glob` (Helm cannot read
files outside the chart directory). `../prometheus.yml` references them via
`rule_files: /etc/prometheus/rules/*.rules.yml`; the compose overlay
(`deploy/compose/docker-compose.observability.yml`) mounts that same chart
directory read-only, and the Helm chart reads it into a ConfigMap (see
`deploy/helm/asker/templates/observability/prometheus.yaml`) — so the alerts are
identical across compose and Kubernetes.

The SLO alerts: query P90 > 5 s, ingest freshness > 30 min, dead-letter rate > 0
(zero-data-loss), cache-hit collapse, per-attempt error rate, 5xx ratio, service
down. They use the exact metric names + labels documented in ADR-017 (e.g.
`asker_query_search_duration_milliseconds_bucket`,
`asker_index_doc_age_seconds_bucket`, `asker_pipeline_deadletter_total`).
