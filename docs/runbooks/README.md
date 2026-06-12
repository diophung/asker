# Runbooks

Operational runbooks are written in **M6** (with backup/restore runbooks arriving earlier, in
M4). The M6 exit criterion is that a new engineer can deploy and operate Asker from docs
alone, so each runbook must include symptoms, diagnosis steps, remediation, and verification.

Planned (the top-10 failure modes, to be confirmed against real incidents and M5 load
testing):

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

Plus, from M4: backup and restore procedures for Postgres, Vespa, and MinIO; blue/green
deployment.
