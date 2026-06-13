# Milestones

Asker is built strictly milestone-by-milestone. Each milestone ends with running software and
passing tests before the next begins. Source of truth for scope:
[`specs/asker-v1-personal-search-engine.md`](specs/asker-v1-personal-search-engine.md) §5.

| Milestone | Name | Status |
| :--- | :--- | :--- |
| M0 | Foundations (skeleton that runs) | **Complete** (2026-06-12) |
| M1 | Vertical slice | **Functionally complete** (2026-06-13; full-scale e2e runs in CI — see PROGRESS.md) |
| M2 | Connector framework + breadth | **In progress** |
| M3 | Media pipeline | Not started |
| M4 | Production deployment | Not started |
| M5 | Scale & SLO verification | Not started |
| M6 | Hardening | Not started |

## M0 — Foundations (skeleton that runs)

Monorepo scaffold; platform libraries (`platform/proto`, `platform/tenancy`,
`platform/telemetry`, `platform/config`); Docker Compose stack with Redpanda, Postgres, MinIO,
Redis, Keycloak, Vespa, and TEI all healthy; CI green; hello-world gateway with OIDC login.

**Exit criteria:** `make dev-up && make e2e-smoke` passes.

## M1 — Vertical slice

Upload connector + Gmail connector (full + incremental sync, webhook via Gmail watch/pub-sub
shim, polling fallback) → ingest → chunk → embed → Vespa streaming index → query service with
hybrid search → web UI with results, snippets, highlights, and filters. Tombstone/delete flow
works end to end.

**Exit criteria:** e2e test ingests 10K synthetic emails for 3 tenants; queries return correct,
tenant-isolated, hybrid-ranked results; a doc edited at the source is searchable in under
30 minutes; the cross-tenant leakage test suite passes (this suite is sacred — it attempts
every API with mismatched tenant tokens and must prove isolation).

## M2 — Connector framework + breadth

Finalize the Connector SDK (in-process + gRPC plugin), then implement: Outlook mail, Google
Drive (with ACL ingestion), S3, Google/Outlook calendar, iCal, Slack (Events API webhook +
backfill), Confluence, Jira, WhatsApp export-file importer, and the iMessage local agent
(Go CLI reading `chat.db`, uploading via API). Connector management UI: connect, OAuth, sync
status, errors.

**Exit criteria:** contract tests pass for every connector against recorded fixtures; a
"build a connector in <1 day" tutorial in docs, proven by implementing MS Teams using only the
public SDK.

## M3 — Media pipeline

Image OCR + CLIP embeddings (text→image search works); audio/video → faster-whisper transcripts
with timestamp-anchored chunks (search hits deep-link to the moment); thumbnails; ffmpeg
keyframe extraction.

**Exit criteria:** e2e: search a spoken phrase, get the video at the right timestamp.

## M4 — Production deployment

Helm charts for everything (cloud-agnostic: no provider-specific resources; storage via
StorageClass, ingress via ingress-class), Strimzi Kafka, Vespa on K8s with multi-group content
clusters, HPA on stateless services, PodDisruptionBudgets, default-deny network policies, Vault
integration, TLS, blue/green deploy docs, backup/restore runbooks for Postgres/Vespa/MinIO.

**Exit criteria:** full stack deploys to a kind/k3d cluster in CI; chaos test (kill any one
pod) shows no failed queries beyond retry.

## M5 — Scale & SLO verification

Synthetic data generator (realistic corpora: 100K tenants, mixed doc types, configurable to TB
scale); k6 load suites for query (stepped to the cluster's max, with documented extrapolation
math to 50K TPS) and ingest (prove 30-minute freshness under load); query result cache;
degradation ladder implemented and tested; `docs/capacity.md` finalized with measured per-node
throughput → node counts for 10PB / 50K TPS. Grafana SLO dashboards + alert rules.

**Exit criteria:** load report committed showing P90 latency and freshness SLAs met at test
scale, with the scaling math to target scale; 2-hour soak test with zero data loss.

## M6 — Hardening

Pen-test-style security review checklist executed (authz matrix, SSRF in connector fetchers,
token vault, injection); GDPR delete drill; quota/abuse controls; admin console; complete
documentation; runbooks for the top-10 failure modes.

**Exit criteria:** ship-review doc signed off; a new engineer can deploy and operate from docs
alone.
