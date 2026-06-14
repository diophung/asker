# Asker system architecture

This document describes the target architecture decided in the product spec
([`specs/asker-v1-personal-search-engine.md`](../specs/asker-v1-personal-search-engine.md) §2).
The architecture is fixed; deviations are recorded as ADRs in [`docs/adr/`](adr/). As of M1,
the vertical slice is real: the full ingestion pipeline (connector hub → Kafka → ingest →
enrich → index writer → Vespa), the query path (gateway → query service → hybrid retrieval),
the Gmail and upload connectors, and the web UI all run in the dev stack. §6 lists exactly
what exists and what is still scheduled for later milestones.

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
 (Gmail, upload;    │ CONNECTOR HUB                               │
  Slack… in M2)     │ scheduler · sync-state store · token vault  │
                    └──────────────┬──────────────────────────────┘
                                   │ canonical Documents
                                   ▼
                       Kafka  docs.raw  (keyed by tenant_id)
                                   ▼
                    ┌──────────────────────────────┐
                    │ INGEST WORKERS               │  parse · dedupe · chunk
                    └──────────────┬───────────────┘
                                   ▼  Kafka  docs.chunked
                    ┌──────────────────────────────┐
                    │ ENRICH WORKERS               │  text embeddings via TEI;
                    └──────────────┬───────────────┘  CLIP + OCR, Whisper (M3)
                                   ▼  Kafka  docs.enriched
                    ┌──────────────────────────────┐
                    │ INDEX WRITERS ──▶ VESPA      │  streaming-mode content
                    └──────────────────────────────┘  clusters, grouped by tenant

 User ──▶ Gateway (authn, rate-limit) ──▶ Query Service ──▶ Vespa (tenant-scoped)
                                                │                     │
                                                ▼                     ▼
                                          Redis (cache)     blended keyword + vector
                                                             (rerank arrives in M5)

 Blobs: MinIO/S3 (originals, thumbnails)     Metadata/control plane: PostgreSQL
 AuthN: Keycloak (OIDC)                      Observability: OTel → Prometheus/Grafana/Tempo/Loki
```

All ingestion is asynchronous and event-driven; the query path is synchronous and
latency-budgeted (§5). Every stage is stateless except Vespa, Kafka, Postgres, and MinIO.

## 3. Technology decisions

| Decision | Choice | Rationale |
| :--- | :--- | :--- |
| Search / index | **Vespa, streaming search mode**, document groups keyed by `tenant_id` | Streaming mode is purpose-built for personal search: each query is scoped to one user's document group and scans raw text + vectors directly, with no per-user inverted-index or ANN structures to build and maintain. Per-tenant cost at rest is ~zero, which is what makes 10M tenants tractable. Hybrid ranking landed in M1 as a single-pass linear blend of `nativeRank` + vector `closeness` ([ADR-006](adr/ADR-006-hybrid-ranking-streaming.md) — streaming mode has no BM25 corpus statistics; cross-encoder rerank is deferred to M5). |
| Event backbone | **Kafka API** — Redpanda in dev compose, Apache Kafka (Strimzi) in K8s | Durable, replayable, partitioned-by-tenant transport between pipeline stages; at-least-once delivery paired with idempotent writers. Redpanda is a single binary, ideal for dev ([ADR-003](adr/ADR-003-dev-stack-deviations.md)); Strimzi is the proven prod operator. |
| Service languages | **Go** for services (gateway, query, connector-hub, ingest, control-plane); **Python** only for ML workers; **TypeScript + React** for web | Go gives small static binaries, cheap concurrency, and one toolchain for everything that isn't ML. Python is confined to enrichment, where the ML ecosystem lives. One language per concern keeps the build and on-call surface small. |
| Text embeddings | **BAAI/bge-m3** (multilingual dense, 1024-dim) served by **Hugging Face TEI** | One multilingual model avoids per-language routing; TEI is a purpose-built, batched, GPU/CPU inference server spoken to over HTTP, so embedding capacity scales independently of the pipeline workers. CI overrides the model to keep runs fast ([ADR-003](adr/ADR-003-dev-stack-deviations.md)). |
| Image / AV enrichment (M3) | OpenCLIP ViT-B/32 + Tesseract OCR; faster-whisper ASR; ffmpeg keyframes | Media becomes text + vectors and rejoins the standard pipeline. |
| Blob store | **MinIO** (dev) / any S3 API (prod) | Originals and thumbnails stored once behind the ubiquitous S3 API — no cloud lock-in. Per-tenant envelope encryption (per-tenant DEK wrapped by a KEK) shipped in M1 via `platform/crypto`; the KEK is a file-based dev shim (shared compose volume) until Vault arrives in M4. |
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

The M1 Vespa schema (`vespa/app/schemas/doc.sd`) indexes the full projection: `doc_id`,
`connector_id`, `type`, `title`, `body`, `chunks`, a chunk-mapped `embedding` tensor (its
dimension templated at deploy time per [ADR-005](adr/ADR-005-embedding-dim-deploy-config.md)),
`participants`, `metadata_json`, `created_at`/`modified_at`, `version_etag`, and `acl`.

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

Degradation ladder: under pressure, drop the rerank first, then drop vector retrieval
(keyword-only) — never fail closed. As of M1 the keyword-only rung is live (rerank does not
exist yet, per [ADR-006](adr/ADR-006-hybrid-ranking-streaming.md)): if TEI or the hybrid rank
profile fails, the query service retries keyword-only and marks the response `degraded`;
`mode=keyword` forces it. Load-testing the ladder is M5 work. Rate limiting is per-tenant at
the gateway (token bucket in Redis, 600 req/min dev default).

## 6. What exists as of M1

M1 delivered the vertical slice: a document travels source → connector → pipeline → Vespa and
is found, tenant-isolated, from the web UI.

- **Platform libraries** — `platform/tenancy` (the tenant chokepoint, plus the `tenancygrpc`
  interceptors that are the only way tenant identity crosses internal gRPC,
  [ADR-009](adr/ADR-009-internal-rpc-tenant-propagation.md)), `platform/proto` (canonical
  Document + the internal gRPC contracts), `platform/kafkautil` (tenant-checked produce/
  consume with dead-lettering, [ADR-004](adr/ADR-004-kafka-pipeline-contract.md)),
  `platform/crypto` (per-tenant envelope encryption), `platform/blob` (S3/MinIO blob store),
  `platform/telemetry`, `platform/config`.
- **Ingestion pipeline, end to end** — the connector hub (`services/connector-hub`) schedules
  per-instance syncs (30s interval, 10s scheduler tick in dev), keeps cursors and encrypted
  OAuth tokens in the control plane (`services/control-plane`, Postgres), receives webhook
  pushes, and emits canonical Documents to `docs.raw`; ingest workers (`services/ingest`)
  normalize, dedupe (Redis doc/version cache) and chunk (~512-token windows, 64-token
  overlap, structure-aware) onto `docs.chunked`; the Python enrich worker (`services/enrich`)
  embeds chunks via TEI (dimension per [ADR-005](adr/ADR-005-embedding-dim-deploy-config.md))
  onto `docs.enriched`; the index writer (`services/index-writer`) upserts into Vespa
  streaming groups keyed by tenant. Tombstones ride the same path and delete from the index;
  poison documents are quarantined to `docs.deadletter`.
- **Connectors** — Gmail (full sync, incremental sync via `history.list`, push via watch +
  Pub/Sub-shaped webhook with polling fallback; dev and CI run against the local `fake-gmail`
  service, [ADR-008](adr/ADR-008-fake-gmail-dev-ci.md)) and direct upload
  (`POST /v1/upload` → blob store → pipeline). Both are in-process Go connectors built on
  `connectors/sdk`.
- **Query path** — gateway REST `/v1/search` → query service gRPC (`services/query`) → query
  understanding (`from:` participants, date ranges, type filters) → TEI query embedding →
  single-pass Vespa hybrid retrieval (`nativeRank` + `closeness` linear blend,
  [ADR-006](adr/ADR-006-hybrid-ranking-streaming.md)) → snippet/highlight assembly. Results
  are cached in Redis (short TTL, keyed by tenant + query hash). The degradation ladder's
  keyword-only rung is live (§5).
- **Gateway** — OIDC JWT validation, tenant only from verified claims
  ([ADR-002](adr/ADR-002-tenant-id-derivation.md)), connector CRUD + token storage, upload,
  search, per-tenant token-bucket rate limiting in Redis, CORS for the web UI.
- **Web UI** — React search app (`web/`, served by the `web` compose service): search box,
  result cards with snippets and highlights, type/date/participant filters, pagination.
- **Dev stack and tests** — 16 health-gated compose services (the M0 infrastructure plus the
  seven application services, `fake-gmail`, and `web`); e2e suites: smoke (`make e2e-smoke`),
  the M1 exit test (`make e2e-m1`: 10K synthetic emails across 3 tenants, freshness, edits,
  tombstones), and the sacred cross-tenant leakage suite (`make e2e-leakage`).

Still missing, by milestone:

- **M2** — connector breadth (Outlook, Drive, S3, calendars, Slack, Confluence, Jira,
  WhatsApp export, iMessage agent), the out-of-process gRPC plugin transport for third-party
  connectors (the SDK is in-process Go only today), and the connector management UI.
- **M3** — media enrichment: CLIP + OCR for images, faster-whisper ASR and keyframe
  extraction for audio/video.
- **M4** — production deployment: Helm charts, Strimzi Kafka, Vespa multi-group content
  clusters, Vault (replacing the dev KEK file), TLS, network policies.
- **M5** — scale and SLO verification: load suites, the cross-encoder rerank rung, the query
  cache tuned under load, measured capacity model (`docs/capacity.md`), SLO dashboards.
- **M6** — hardening: security review, GDPR delete drill, quotas, admin console, runbooks.

Scheduling source of truth: [MILESTONES.md](../MILESTONES.md).
