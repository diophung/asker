# Runbooks

Operational runbooks (written in **M6**, with backup/restore arriving earlier in M4). The M6 exit
criterion is that a new engineer can deploy and operate Asker from docs alone, so each runbook
includes symptoms (the alert that fires), diagnosis steps, remediation, and verification.

## Failure-mode runbooks (M6)

The **top-10 failure modes** are fully covered, one section each, in
[`top-10-failure-modes.md`](top-10-failure-modes.md):

1. Vespa content node down / cluster degraded
2. Kafka/Redpanda consumer-group lag — freshness SLA (30 min) at risk
3. Keycloak outage or JWKS rotation breaking token validation at the gateway
4. TEI embedding service down or saturated (query and ingest paths degrade differently)
5. Postgres failover / control-plane unavailability
6. Redis outage (cache + rate limiting; must fail open per the degradation ladder)
7. MinIO/S3 outage or blob corruption
8. Connector stuck: OAuth token refresh failures, source API quota exhaustion
9. Poison documents filling the dead-letter topic
10. Query latency SLO burn (P90 > 5s) — degradation ladder and rollback

Each cites the M5 Prometheus alert (e.g. `AskerQueryP90LatencyHigh`, `AskerDeadletterNonZero`,
`AskerIngestFreshnessP90High`) that signals it.

## Backup / restore + deploy (M4)

- [`backup-restore-postgres.md`](backup-restore-postgres.md) — control-plane store (incl. the
  catastrophic `tenant_deks` case)
- [`backup-restore-vespa.md`](backup-restore-vespa.md) — the streaming-mode index (PVC snapshot,
  logical export, or rebuild-from-source)
- [`backup-restore-minio.md`](backup-restore-minio.md) — the blob store
- [`blue-green-deploy.md`](blue-green-deploy.md) — zero-/low-downtime stateless-tier rollout and
  rollback

## Related

- [`../security.md`](../security.md) — threat model, authz matrix, SSRF & GDPR guarantees
- [`../ship-review.md`](../ship-review.md) — the M6 exit-criterion sign-off (milestone status,
  SLO posture, accepted risks, the deploy-from-docs checklist)
- [`../capacity.md`](../capacity.md) — scaling math; [`../../deploy/k8s-docs.md`](../../deploy/k8s-docs.md) — deploy to Kubernetes
