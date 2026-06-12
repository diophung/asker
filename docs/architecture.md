# Asker system architecture

This document describes the target architecture decided in the product spec
([`specs/asker-v1-personal-search-engine.md`](../specs/asker-v1-personal-search-engine.md) §2).
The architecture is fixed; deviations are recorded as ADRs in [`docs/adr/`](adr/). As of M0,
only the foundation exists (platform libraries, dev infrastructure stack, gateway authn) —
sections below describe where each piece lands in the milestone plan.

## 1. The central insight: multi-tenancy IS the problem

Asker's hard requirements read like a web-scale search engine — 10PB raw data, 10M users,
50K TPS — but the data is *not* one corpus. It is ~10M small, strictly private corpora: the
median user owns a few GB, a power user a few TB. No query ever spans tenants.

Everything follows from this:

- **Don't build one global index.** Per-tenant inverted indexes and ANN graphs for 10M mostly
  idle tenants would be ruinously expensive to maintain at rest. Vespa streaming mode stores
  documents grouped by tenant and scans a single tenant's group at query time — near-zero
  per-tenant cost at rest, and scan cost proportional to one user's corpus, not the fleet's.
- **Route by tenant everywhere.** Kafka topics are keyed by `tenant_id`, Vespa documents are
  grouped by `tenant_id`, and every query carries a tenant group selector.
- **Make isolation structural, not disciplinary.** `tenant_id` derives only from the verified
  OIDC token (see [ADR-002](adr/ADR-002-tenant-id-derivation.md)), and `platform/tenancy` is
  the single library through which a data-access context can be constructed. Code that forgets
  the tenant does not compile against the data-access APIs; it cannot "accidentally" query
  globally.

## 2. Logical diagram

```
 Sources ──OAuth──▶ ┌─────────────────────────────────────────────┐
 (Gmail, Slack, …)  │ CONNECTOR HUB                        (M1+)  │
                    │ scheduler · sync-state store · token vault  │
                    └──────────────┬──────────────────────────────┘
                                   │ canonical Documents
                                   ▼
                       Kafka  docs.raw  (keyed by tenant_id)
                                   ▼
                    ┌──────────────────────────────┐
                    │ INGEST WORKERS         (M1)  │  parse · dedupe · chunk
                    └──────────────┬───────────────┘
                                   ▼  Kafka  docs.chunked
                    ┌──────────────────────────────┐
                    │ ENRICH WORKERS         (M1)  │  text embeddings via TEI;
                    └──────────────┬───────────────┘  CLIP + OCR, Whisper (M3)
                                   ▼  Kafka  docs.enriched
                    ┌──────────────────────────────┐
                    │ INDEX WRITERS ──▶ VESPA (M1) │  streaming-mode content
                    └──────────────────────────────┘  clusters, grouped by tenant

 User ──▶ Gateway (authn, rate-limit) ──▶ Query Service (M1) ──▶ Vespa (tenant-scoped)
              (M0: authn only)                  │                     │
                                                ▼                     ▼
                                          Redis (cache)     blended BM25 + vector
                                                             + cross-encoder rerank

 Blobs: MinIO/S3 (originals, thumbnails)     Metadata/control plane: PostgreSQL
 AuthN: Keycloak (OIDC)                      Observability: OTel → Prometheus/Grafana/Tempo/Loki
```

All ingestion is asynchronous and event-driven; the query path is synchronous and
latency-budgeted (§5). Every stage is stateless except Vespa, Kafka, Postgres, and MinIO.

## 3. Technology decisions

| Decision | Choice | Rationale |
| :--- | :--- | :--- |
| Search / index | **Vespa, streaming search mode**, document groups keyed by `tenant_id` | Streaming mode is purpose-built for personal search: each query is scoped to one user's document group and scans raw text + vectors directly, with no per-user inverted-index or ANN structures to build and maintain. Per-tenant cost at rest is ~zero, which is what makes 10M tenants tractable. Hybrid ranking (BM25 + vector closeness, fused, with optional cross-encoder rerank) lands in M1. |
| Event backbone | **Kafka API** — Redpanda in dev compose, Apache Kafka (Strimzi) in K8s | Durable, replayable, partitioned-by-tenant transport between pipeline stages; at-least-once delivery paired with idempotent writers. Redpanda is a single binary, ideal for dev ([ADR-003](adr/ADR-003-dev-stack-deviations.md)); Strimzi is the proven prod operator. |
| Service languages | **Go** for services (gateway, query, connector-hub, ingest, control-plane); **Python** only for ML workers; **TypeScript + React** for web | Go gives small static binaries, cheap concurrency, and one toolchain for everything that isn't ML. Python is confined to enrichment, where the ML ecosystem lives. One language per concern keeps the build and on-call surface small. |
| Text embeddings | **BAAI/bge-m3** (multilingual dense, 1024-dim) served by **Hugging Face TEI** | One multilingual model avoids per-language routing; TEI is a purpose-built, batched, GPU/CPU inference server spoken to over HTTP, so embedding capacity scales independently of the pipeline workers. CI overrides the model to keep runs fast ([ADR-003](adr/ADR-003-dev-stack-deviations.md)). |
| Image / AV enrichment (M3) | OpenCLIP ViT-B/32 + Tesseract OCR; faster-whisper ASR; ffmpeg keyframes | Media becomes text + vectors and rejoins the standard pipeline. |
| Blob store | **MinIO** (dev) / any S3 API (prod) | Originals and thumbnails stored once behind the ubiquitous S3 API — no cloud lock-in. Per-tenant envelope encryption (per-tenant DEK wrapped by a KEK) arrives in M1; Vault holds the KEK in prod, with a file-based dev shim behind the same interface. |
| Control-plane DB | **PostgreSQL** | Tenants, connector configs, sync cursors, encrypted OAuth tokens, GDPR/delete jobs, quotas. Relational, transactional, boring — exactly right for low-volume control data. |
| Cache | **Redis** | Query result cache (short TTL, keyed by tenant + query hash), session data, token-bucket rate-limit counters. |
| AuthN/AuthZ | **Keycloak (OIDC)**; gateway validates JWTs | Standard OIDC keeps auth out of application code. `tenant_id` comes only from the verified token ([ADR-002](adr/ADR-002-tenant-id-derivation.md)); every internal RPC and Kafka message carries it; every Vespa query must include the tenant group selector, enforced via `platform/tenancy`. |
| APIs | External: REST+JSON (OpenAPI in repo); internal: gRPC, protos in `platform/proto` | Human-friendly at the edge, typed and contract-checked inside. |
| Observability | **OpenTelemetry** everywhere → Prometheus / Grafana / Tempo / Loki | RED metrics per service, exemplar traces, per-tenant usage metrics. In M0 `platform/telemetry` no-ops when no OTLP endpoint is configured, so dev runs clean without a collector. |

## 4. Canonical Document model

The Document is the contract every pipeline stage depends on; the protobuf source of truth
lives in `platform/proto`. Shape (abridged):

```
Document {
  tenant_id          // REQUIRED everywhere
  doc_id             // stable: hash(connector_id + source_native_id)
  connector_id       // "gmail", "slack", …
  source_native_id   // message id, file id, event id…
  type               // EMAIL | CHAT_MESSAGE | FILE | CALENDAR_EVENT | WIKI_PAGE |
                     // TICKET | IMAGE | VIDEO | AUDIO
  title
  body_text          // extracted/normalized text
  chunks[]           // chunk_id, text, embedding ref, char offsets
  metadata{}         // sender, channel, path, labels…
  participants[]     // people: name, email/handle (typed search facet)
  ts                 // created, modified, ingested
  acl                // for shared sources (Drive, Confluence): who may see it
  original           // BlobRef into the blob store
  version_etag       // idempotent upserts keyed by (doc_id, version_etag)
  tombstone          // deletions propagate as documents too
}
```

Chunking policy (applied by ingest workers from M1): ~512-token chunks with 64-token overlap,
structure-aware — chat messages are never split mid-message, emails split at quoted-reply
boundaries, calendar events are single chunks. Deletes and updates are first-class: connectors
emit tombstones, and index writers and blob GC honor them within the 30-minute freshness SLA.

The M0 Vespa schema indexes a deliberately minimal projection of this model (`doc_id`,
`connector_id`, `type`, `title`, `body`, `created_at`); chunks, embeddings, and the full field
set arrive with the M1 pipeline.

## 5. Query path and latency budget

Hard SLO: **P90 ≤ 5000ms** end-to-end; design target **P50 ≤ 800ms**. Per-stage budget:

| Stage | Budget |
| :--- | ---: |
| Gateway authn + route | 20ms |
| Cache check (Redis) | 5ms |
| Query understanding (rewrite; `from:alice`, date/type filter extraction) | 30ms |
| Embed query (TEI) | 50ms |
| Vespa hybrid retrieval, tenant-scoped | 200–800ms |
| Cross-encoder rerank of top-50 (skipped if over budget) | 300ms |
| Snippet / highlight assembly | 50ms |

Degradation ladder (implemented and load-tested in M5): under pressure, drop the rerank first,
then drop vector retrieval (keyword-only) — never fail closed. Rate limiting is per-tenant and
global at the gateway, token-bucket in Redis.

## 6. What exists in M0

- `platform/tenancy`, `platform/telemetry`, `platform/config`, `platform/proto` — the shared
  libraries everything else is written against.
- The full dev infrastructure stack in Docker Compose (Redpanda, Postgres, MinIO, Redis,
  Keycloak, Vespa, TEI), all health-gated so `make dev-up` only succeeds when ready.
- The Vespa application package (streaming-mode `doc` schema) and its deploy script.
- The gateway: OIDC JWT validation against Keycloak, tenant derivation via
  `platform/tenancy`, health endpoint, OTel-instrumented HTTP handling.
- CI scaffolding and an e2e smoke test (`make e2e-smoke`).

Everything else in this document is the agreed target, scheduled per
[MILESTONES.md](../MILESTONES.md).
