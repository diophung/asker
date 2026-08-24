# CLAUDE CODE PROMPT — Build "Asker": A Personal Search Engine

## Your Role

You are the founding engineering team for **Asker**. You will design and implement the
entire system across multiple working sessions, milestone by milestone. You own
architecture, code, tests, deployment, and documentation. Build to the standard of a
Google production service: every component must be observable, testable, horizontally
scalable, and secure by default.

This is a long-running project. You MUST work milestone-by-milestone (defined below),
and you MUST NOT skip ahead. Each milestone ends with running software and passing
tests before the next begins.

---

## 1. Product Definition

Asker is "Google for your own data." A user connects their personal data sources —
email, chat, files, calendars, wikis, images, video — Asker continuously indexes them,
and the user searches everything with a single query box supporting both **semantic
(vector) search** and **keyword search**, blended.

### Hard Requirements (non-negotiable)
| Requirement | Target |
|---|---|
| Total raw data across all tenants | up to 10 PB |
| Query latency | P90 ≤ 5s end-to-end (target P50 ≤ 800ms) |
| Index freshness | new/changed source data searchable within 30 minutes |
| Concurrent users | up to 10M |
| Peak query throughput | 50K TPS |
| Software | open source only, or written by you |
| Tenancy | strict per-user data isolation — a user can NEVER see another user's data |
| Deployment | Docker Compose (dev/local) AND Kubernetes via Helm (production, cloud-agnostic) |

### V1 Connectors (all in scope)
Email: Gmail, Outlook/Microsoft 365. Files: Google Drive, S3-compatible buckets,
direct upload. Calendar: Google Calendar, Outlook Calendar, iCal feeds.
Chat: Slack, Microsoft Teams, WhatsApp (export-file import), iMessage (local macOS
database import — there is no cloud API; implement as a local agent/CLI that uploads).
Knowledge: Confluence, Jira.
Plus a **Connector SDK** so third parties can build connectors without touching core code.

---

## 2. Architecture (decided — do not relitigate, record deviations as ADRs)

### 2.1 The central insight: multi-tenancy IS the problem
This is not one 10PB index. It is ~10M small private corpora (median user: a few GB;
power user: a few TB). Architect for per-tenant isolation and per-tenant query routing,
not for one global index.

### 2.2 System diagram (logical)
```
                        ┌─────────────────────────────────────────────┐
 Sources ──OAuth──▶     │  CONNECTOR HUB                              │
 (Gmail, Slack, ...)    │  scheduler · sync-state store · token vault │
                        └──────────────┬──────────────────────────────┘
                                       │ canonical Documents
                                       ▼
                            Kafka (topic: docs.raw, keyed by tenant_id)
                                       │
                        ┌──────────────▼──────────────┐
                        │  INGEST WORKERS             │
                        │  parse · dedupe · chunk     │
                        └──────────────┬──────────────┘
                                       ▼  Kafka: docs.chunked
                        ┌──────────────▼──────────────┐
                        │  ENRICH WORKERS             │
                        │  text embeddings (TEI)      │
                        │  image: CLIP + OCR          │
                        │  video/audio: Whisper ASR   │
                        └──────────────┬──────────────┘
                                       ▼  Kafka: docs.enriched
                        ┌──────────────▼──────────────┐
                        │  INDEX WRITERS ──▶ VESPA    │ (streaming-mode content
                        └─────────────────────────────┘  clusters, grouped by tenant)
 
 User ──▶ Gateway (authn, rate-limit) ──▶ Query Service ──▶ Vespa (tenant-scoped)
                                              │                  │
                                              ▼                  ▼
                                        Redis (cache)    blended BM25 + vector
                                                          + cross-encoder rerank
 
 Blobs: MinIO/S3 (originals, thumbnails)   Metadata/control-plane: PostgreSQL
 AuthN: Keycloak (OIDC)                     Observability: OTel → Prometheus/Grafana/Tempo/Loki
```

### 2.3 Technology decisions (use exactly these unless an ADR justifies otherwise)
- **Search/index: Vespa, streaming search mode.** Rationale: streaming mode is purpose-
  built for personal/per-user search — it scopes each query to one user's document
  group and scans raw vectors + text without maintaining per-user ANN/inverted-index
  structures. This makes per-tenant cost ~zero at rest and makes 10M tenants tractable.
  Use Vespa document groups keyed by `tenant_id`. Hybrid ranking: BM25 + closeness()
  fused with reciprocal rank fusion or learned linear blend, then optional
  cross-encoder rerank of top-50 (budget permitting within the 5s P90).
- **Event backbone: Kafka API.** Use **Redpanda** in Docker Compose for dev (single
  binary); Apache Kafka (Strimzi operator) in K8s. All topics keyed by `tenant_id`.
- **Services language: Go** (gateway, query, connector-hub, ingest, control-plane).
  **Python** only for ML workers (enrichment). **TypeScript + React** for web UI.
- **Embeddings:** text — `BAAI/bge-m3` (multilingual, dense; 1024-dim) served via
  Hugging Face **Text Embeddings Inference (TEI)**; images — OpenCLIP ViT-B/32 +
  Tesseract OCR for embedded text; audio/video — **faster-whisper** ASR, then text
  pipeline; video also gets keyframe extraction (ffmpeg scene detection) → CLIP.
- **Blob store:** MinIO (dev) / any S3 API (prod). Originals are stored once,
  encrypted per-tenant (envelope encryption, per-tenant DEK wrapped by a KEK; KEK in
  HashiCorp Vault in prod, file-based dev shim with the same interface).
- **Control plane DB:** PostgreSQL — tenants, connector configs, sync cursors,
  OAuth tokens (encrypted at rest), GDPR/delete jobs, quotas.
- **Cache:** Redis — query result cache (short TTL, keyed tenant+query hash), session
  data, rate-limit counters.
- **AuthN/AuthZ:** Keycloak OIDC; gateway validates JWT; `tenant_id` derives ONLY from
  the verified token — never from request body. Every internal RPC and Kafka message
  carries `tenant_id`; every Vespa query MUST include the tenant group selector.
  Write a single shared library `platform/tenancy` that is the only way to construct
  a data-access context; make it impossible to query without a tenant.
- **API:** external — REST+JSON (OpenAPI spec checked into repo); internal — gRPC
  with protobuf definitions in `platform/proto`.
- **Observability:** OpenTelemetry everywhere; RED metrics per service; exemplar
  traces; per-tenant usage metrics. Prometheus + Grafana + Tempo + Loki. Ship
  dashboards and alert rules as code.

### 2.4 Canonical Document model (the contract everything depends on — design first)
```protobuf
message Document {
  string tenant_id;        // REQUIRED everywhere
  string doc_id;           // stable: hash(connector_id + source_native_id)
  string connector_id;     // e.g. "gmail", "slack"
  string source_native_id; // message id, file id, event id...
  DocType type;            // EMAIL | CHAT_MESSAGE | FILE | CALENDAR_EVENT | WIKI_PAGE | TICKET | IMAGE | VIDEO | AUDIO
  string title;
  string body_text;        // extracted/normalized text
  repeated Chunk chunks;   // chunk_id, text, embedding ref, char offsets
  map<string,string> metadata;  // sender, channel, path, labels...
  repeated Participant participants; // people: name, email/handle (typed search facet)
  Timestamps ts;           // created, modified, ingested
  AclInfo acl;             // for shared sources (Drive, Confluence): who may see it
  BlobRef original;        // pointer into blob store
  string version_etag;     // for idempotent upserts
  Tombstone tombstone;     // deletions propagate as documents too
}
```
Chunking policy: ~512-token chunks, 64-token overlap, structure-aware (don't split
mid-message for chat; emails split by quoted-reply boundaries; calendar events are
single chunks). Deletes and updates are first-class: connectors emit tombstones;
index writers and blob GC honor them within the 30-min SLA.

### 2.5 Connector SDK (the pluggable framework)
A connector is a Go module (or external gRPC process — support both) implementing:
```go
type Connector interface {
  Spec() ConnectorSpec                      // id, auth type, config schema (JSONSchema)
  Validate(ctx, Config) error
  FullSync(ctx, Config, Emit) error          // initial backfill, resumable via checkpoints
  IncrementalSync(ctx, Config, Cursor, Emit) (Cursor, error)  // poll path
  HandleWebhook(ctx, Config, http.Request, Emit) error        // push path, optional
}
```
The Connector Hub owns: OAuth flows + token refresh + encrypted token vault; per-tenant
per-connector sync schedules (webhook-first, fall back to polling at an interval that
meets the 30-min SLA); checkpointed, resumable backfills with per-tenant rate limits
and per-source API-quota respect; exponential backoff; poison-document quarantine
(dead-letter topic); sync health surfaced in the UI. Third-party connectors run as
out-of-process gRPC plugins with no credentials beyond their own scoped tokens.

### 2.6 Query path & latency budget (P90 ≤ 5000ms; design to P50 ≤ 800ms)
gateway authn+route 20ms → cache check 5ms → query understanding (rewrite, filter
extraction: `from:alice`, date ranges, type filters) 30ms → embed query (TEI) 50ms →
Vespa hybrid retrieval, tenant-scoped 200–800ms → cross-encoder rerank top-50 (skip
if over budget) 300ms → snippet/highlight assembly 50ms. Degrade gracefully: under
load, drop rerank first, then vector (keyword-only), never fail closed. Per-tenant
and global rate limits at the gateway (token bucket in Redis).

### 2.7 Scale model (write this up properly in docs/capacity.md with your math)
Assume 10M registered, ~5% concurrently active, 50K TPS peak. 10PB raw → assume
~8–12% extractable text → ~1PB text → ~2T chunks → with bge-m3 1024-dim fp16
≈ 4TB embeddings per 1B chunks; use Vespa's bfloat16/int8 cell types and paged
attributes. Shard Vespa content clusters by tenant hash; size node groups so a hot
tenant's streaming query touches one group. Kafka partitions ≥ 512 on docs.* topics.
Everything stateless except Vespa/Kafka/Postgres/MinIO scales horizontally on HPA.

---

## 3. Repository Layout (monorepo)
```
asker/
├── README.md, MILESTONES.md, PROGRESS.md
├── docs/ (architecture.md, capacity.md, security.md, adr/ADR-NNN-*.md, runbooks/)
├── platform/ (proto/, tenancy/, telemetry/, kafkautil/, crypto/, config/)
├── services/
│   ├── gateway/  query/  ingest/  enrich/  connector-hub/  control-plane/  index-writer/
├── connectors/ (sdk/, gmail/, outlook-mail/, gdrive/, s3/, gcal/, outlook-cal/, ical/,
│                slack/, msteams/, confluence/, jira/, whatsapp-export/, imessage-agent/, upload/)
├── web/  (React app: search box, filters, result cards w/ snippets+highlights,
│          connector management UI, sync status, admin)
├── vespa/ (application package: schemas, ranking profiles, services.xml)
├── deploy/compose/  deploy/helm/  deploy/k8s-docs.md
└── tools/ (loadtest/ k6 scenarios, seed/ synthetic-data generator, e2e/)
```

## 4. Engineering Standards (enforced)
- Tests: unit + integration per service; an e2e suite that runs against Docker Compose
  in CI; connector contract tests against the SDK with recorded fixtures (no live
  API calls in CI). Target ≥75% coverage on core libs, 100% on `platform/tenancy`.
- CI: GitHub Actions — lint (golangci-lint, ruff, eslint), build, test, compose-up
  e2e, helm lint, trivy image scan. All green before a milestone closes.
- Every cross-cutting decision gets an ADR. Maintain PROGRESS.md after every session
  (what's done, what's next, known issues) — this is your memory between sessions.
- Security: no secrets in code; per-tenant envelope encryption for blobs and tokens;
  TLS internal in prod profile; full GDPR delete (one command wipes a tenant from
  Postgres, Vespa, MinIO, and replays tombstones) with a verification job.
- Idempotency everywhere: doc upserts keyed by (doc_id, version_etag); Kafka consumers
  are at-least-once + idempotent writes.

## 5. Milestones — execute strictly in order

**M0 — Foundations (skeleton that runs).** Monorepo scaffold, platform libs (proto,
tenancy, telemetry, config), Docker Compose stack with Redpanda/Postgres/MinIO/Redis/
Keycloak/Vespa/TEI all healthy, CI green, hello-world gateway with OIDC login.
*Exit: `make dev-up && make e2e-smoke` passes.*

**M1 — Vertical slice.** Upload connector + Gmail connector (full + incremental sync,
webhook via Gmail watch/pubsub shim, polling fallback) → ingest → chunk → embed →
Vespa streaming index → query service with hybrid search → web UI with results,
snippets, highlights, filters. Tombstone/delete flow works.
*Exit: e2e test: ingest 10K synthetic emails for 3 tenants; queries return correct,
tenant-isolated, hybrid-ranked results; a doc edited at the source is searchable
< 30 min; cross-tenant leakage test suite passes (this suite is sacred — it attempts
every API with mismatched tenant tokens and must prove isolation).*

**M2 — Connector framework + breadth.** Finalize Connector SDK (in-proc + gRPC
plugin), then implement: Outlook mail, Google Drive (with ACL ingestion), S3,
Google/Outlook calendar, iCal, Slack (Events API webhook + backfill), Confluence,
Jira. WhatsApp export-file importer. iMessage local agent (Go CLI reading chat.db,
uploads via API). Connector management UI: connect, OAuth, sync status, errors.
*Exit: contract tests pass for every connector against recorded fixtures; a "build a
connector in <1 day" tutorial in docs proven by implementing MS Teams using only the
public SDK.*

**M3 — Media pipeline.** Image OCR + CLIP embeddings (text→image search works);
audio/video → faster-whisper transcript with timestamp-anchored chunks (search hit
deep-links to the moment); thumbnails; ffmpeg keyframes.
*Exit: e2e: search a spoken phrase, get the video at the right timestamp.*

**M4 — Production deployment.** Helm charts for everything (cloud-agnostic: no
provider-specific resources; storage via StorageClass, ingress via ingress-class),
Strimzi Kafka, Vespa on K8s with multi-group content clusters, HPA on stateless
services, PodDisruptionBudgets, network policies (default-deny), Vault integration,
TLS, blue/green deploy docs, backup/restore runbooks for Postgres/Vespa/MinIO.
*Exit: full stack deploys to a kind/k3d cluster in CI; chaos test (kill any one pod)
shows no failed queries beyond retry.*

**M5 — Scale & SLO verification.** Synthetic data generator (realistic corpora: 100K
tenants, mixed doc types, configurable to TB scale); k6 load suites for query
(stepped to the cluster's max, with documented extrapolation math to 50K TPS) and
ingest (prove 30-min freshness under load); query result cache; degradation ladder
implemented and tested; capacity.md finalized with measured per-node throughput →
node counts for 10PB/50K TPS. Grafana SLO dashboards + alert rules.
*Exit: load report committed showing P90 latency and freshness SLAs met at test
scale, with the scaling math to target scale; soak test 2h with zero data loss.*

**M6 — Hardening.** Pen-test-style security review checklist executed (authz matrix,
SSRF in connector fetchers, token vault, injection), GDPR delete drill, quota/abuse
controls, admin console, docs complete, runbooks for top-10 failure modes.
*Exit: ship review doc signed off; a new engineer can deploy and operate from docs alone.*

## 6. How to Work
1. Start every session by reading PROGRESS.md and MILESTONES.md.
2. Within a milestone: design notes/ADR first if non-trivial → implement → tests →
   run the stack → update PROGRESS.md.
3. Prefer boring, proven patterns over clever ones. Optimize only with a measurement.
4. If a requirement conflicts (e.g., rerank vs. latency), implement the degradation
   path and document the tradeoff — never silently drop a requirement.
5. Ask the human only when a decision is irreversible AND not covered here (e.g.,
   paid API keys for live connector testing). Otherwise decide, record an ADR, move.

Begin with M0 now: print your plan for M0 as a checklist, then execute it.
