# ADR-004: Kafka pipeline contract — Document protobuf, tenant keying, at-least-once + dead-letter

## Status

Accepted (M1).

## Context

The ingestion pipeline is a chain of Kafka topics (`docs.raw` → `docs.chunked` →
`docs.enriched`) with independently scaled workers between them, written in two languages (Go
services, Python enrichment). Every stage needs to agree on the record format, the keying, how
tenancy travels with each record, what delivery guarantee consumers provide, and what happens
to a record that cannot be processed. These rules live in `platform/kafkautil` so no producer
or consumer reinvents them.

## Decision

1. **Wire format: the canonical `asker.v1.Document` protobuf** (`platform/proto`), serialized
   with `proto.Marshal`, on `docs.raw`, `docs.chunked`, and `docs.enriched`. Each stage
   consumes a Document and produces an enriched Document — there are no stage-private message
   types. Topic names are exported constants in `platform/kafkautil`.

2. **Records are keyed by `tenant_id`** (the spec's routing rule): one tenant's documents land
   on one partition, giving per-tenant ordering — a tombstone can never overtake the upsert it
   revokes within a tenant.

3. **Record headers carry `tenant_id`, `doc_id`, `version_etag`.** The producer is a tenancy
   chokepoint: `Producer.ProduceDocument` requires a `tenancy.Context` on the Go context and
   errors unless it matches `doc.TenantId`. Consumers reconstruct the tenancy context from the
   `tenant_id` header via `tenancy.FromHeaderValue` (which re-validates the syntax allowlist)
   before invoking the handler — a record without a valid tenant never reaches application
   code.

4. **Delivery is at-least-once; writes are idempotent.** Consumers commit offsets only after
   the handler succeeds, so duplicates are possible and downstream writes must be keyed by
   `(doc_id, version_etag)` (Vespa upserts, blob puts, control-plane rows) so a replay is a
   no-op.

5. **Poison-document quarantine.** On handler error the consumer retries in-process with
   capped exponential backoff (3 attempts), then produces the record to `docs.deadletter`
   (original headers plus an error header) and commits — one bad document never stalls a
   partition. Malformed records (undecodable proto, invalid tenant header) skip retries and go
   straight to the dead letter.

6. **Partition counts: 4 in dev, ≥512 in prod.** `EnsureTopics` creates topics idempotently;
   compose passes 4 (plenty for a laptop, keeps Redpanda small per ADR-003 §5). The spec's
   ≥512 for prod is sized properly in `docs/capacity.md` during M5.

## Consequences

- Two-language parity burden: the Python enrich worker must implement the same header,
  commit-after-success, and dead-letter semantics as `kafkautil` by hand (ADR-007); a
  contract drift there breaks delivery guarantees silently, so its tests assert these
  behaviors explicitly.
- At-least-once + idempotent is mandatory, not optional: any new sink must be keyed by
  `(doc_id, version_etag)` or it will double-write under rebalance/retry.
- Full Documents on every topic means payloads grow stage by stage (chunks, then embeddings);
  large bodies inflate broker storage and replay cost. Acceptable at M1 scale; if it bites,
  the escape hatch is blob-offloading bodies behind `BlobRef`, not a new message type.
- The dead-letter topic is a real operational surface: quarantined documents are invisible to
  the user until replayed. Sync-health UI / replay tooling owes visibility into it (M2).
- Keying by tenant means a single huge tenant cannot parallelize beyond one partition per
  topic. That is the deliberate trade for per-tenant ordering; partition counts in prod are
  sized for the fleet, not one tenant.
- Changing partition counts later re-shuffles tenant→partition assignment, transiently
  breaking per-tenant ordering; do it only between drained deploys.
