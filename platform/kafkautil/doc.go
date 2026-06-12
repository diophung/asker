// Package kafkautil is the single place where Asker's Kafka pipeline contract
// (ADR-004) is implemented: canonical asker.v1.Document protobufs on the
// docs.* topics, records keyed by tenant_id, tenancy carried in record
// headers, at-least-once delivery with commit-after-success, and a
// dead-letter quarantine for poison documents.
//
// Tenancy chokepoints:
//
//   - Producer.ProduceDocument requires a tenancy.Context on the Go context
//     and refuses to produce unless it matches doc.TenantId, so a document
//     can never be written under the wrong tenant key.
//   - Consumer.Run reconstructs the tenancy.Context from the tenant_id record
//     header (re-validating the tenant syntax allowlist) before invoking the
//     handler; a record without a valid tenant never reaches application
//     code — it is quarantined to docs.deadletter instead.
//
// Trust model: services consume tenant identity from record headers because
// only trusted in-network producers (gateway / connector-hub, which derived
// the tenant from a VERIFIED JWT) can reach the broker on the compose /
// cluster-internal network. mTLS hardens this boundary in M4 (ADR-009).
//
// Delivery: produces are synchronous and consumers commit per record —
// correctness over throughput for M1. Batched async production and batched
// commits are M5 work.
package kafkautil
