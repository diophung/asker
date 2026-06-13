# Runbook: the top-10 failure modes

> Scope: the M6 operational runbooks for Asker's ten most likely production failure modes,
> confirmed against the M5 load/soak testing and the M5 alert rules
> (`deploy/helm/asker/files/observability/prometheus/rules/asker-slo.rules.yml`). Each mode lists
> **Symptoms** (the alert that fires), **Diagnosis** (commands to confirm), **Remediation**,
> **Verification**, and the **SLO** it protects.
>
> Backup/restore and rollout procedures are NOT duplicated here — they live in their own M4
> runbooks: [Postgres](backup-restore-postgres.md), [Vespa](backup-restore-vespa.md),
> [MinIO](backup-restore-minio.md), and [blue/green deploy](blue-green-deploy.md). This file
> cross-links them where a failure mode escalates to a restore or rollback.

## Conventions

- `NS` is the release namespace (`export NS=asker`). `kubectl` examples assume `-n "$NS"`.
- `PROM` is the Prometheus base URL (in-cluster `http://prometheus:9090`; opt-in, ADR-017 §6).
  PromQL is shown so you can paste it into Grafana's Explore or `promtool query instant`.
- Compose equivalents (dev) use `docker compose` and the service names from
  `deploy/compose/docker-compose.yml` (`gateway`, `query`, `vespa`, `redpanda`, `tei`,
  `keycloak`, `postgres`, `redis`, `minio`, `connector-hub`, `ingest`, `index-writer`,
  `enrich`, `control-plane`).
- The SLOs (architecture.md §5, ADR-017): **query P90 ≤ 5000 ms** (P50 ≤ 800 ms design target);
  **ingest freshness ≤ 30 min** (source edit → searchable); **zero data loss** (the soak signal,
  `asker_pipeline_deadletter_total`).
- Alert names referenced below are exactly the `alert:` names in the M5 rules file.

The system degradation ladder (architecture.md §5) is the governing principle for several of
these: **never fail closed** — drop the cross-encoder rerank first, then drop vector retrieval
(keyword-only), before failing a query.

---

## 1. Vespa content node down / cluster degraded

**Protects:** query availability + correctness (the streaming-mode index is the only store that
can answer a search).

### Symptoms
- `AskerServiceDown` or `AskerScrapeTargetAbsent` for a Vespa target; or `AskerQueryP90LatencyHigh`
  / `AskerQueryP50LatencyHigh` (a degraded group serves slower).
- `AskerService5xxRateHigh` on `query` if Vespa returns errors that the degradation ladder cannot
  paper over.
- Search returns `degraded` responses or empty results for tenants whose group lived on the down
  node.

### Diagnosis
```sh
# Pod / StatefulSet health
kubectl -n "$NS" get pods -l app.kubernetes.io/component=search
kubectl -n "$NS" describe statefulset asker-vespa | sed -n '/Events/,$p'

# Vespa's own cluster health (config server on :19071, query/doc API on :8080)
kubectl -n "$NS" exec asker-vespa-0 -- \
  curl -s http://localhost:19071/state/v1/health
kubectl -n "$NS" exec asker-vespa-0 -- \
  curl -s 'http://localhost:8080/state/v1/health'
```
```promql
# Query P90 (the SLO line is the 5000ms bucket)
histogram_quantile(0.9, sum by (le) (rate(asker_query_search_duration_milliseconds_bucket[5m])))
```
Dev: `docker compose ps vespa` and `docker compose logs --tail=200 vespa`.

### Remediation
1. If a single pod is `CrashLoopBackOff` / `OOMKilled`: `kubectl -n "$NS" delete pod asker-vespa-<n>`
   to reschedule; the StatefulSet re-attaches its `vespa-var-asker-vespa-<n>` PVC and Vespa
   recovers its document store on restart.
2. If the node is gone (disk failure) and the group is unrecoverable: restore that content node's
   PVC from a CSI `VolumeSnapshot`, or rebuild the derived index from source — both covered in
   [backup-restore-vespa.md](backup-restore-vespa.md) (PVC snapshot = minutes; rebuild from Kafka
   source = hours–days but needs no Vespa backup). Vespa is **downstream of the Kafka pipeline and
   rebuildable**.
3. If the whole cluster is unhealthy after a bad app-package activation: re-activate the last-good
   package (config server `:19071`); see backup-restore-vespa.md "Restore the application package".

### Verification
- `kubectl -n "$NS" get pods -l app.kubernetes.io/component=search` all `Running`/`Ready`.
- `tools/e2e/leakage.sh` (sacred) passes and a known query returns its expected hit (the
  `CHECK_EXACT_HITS` assertion from the M5 load suite is the canary).
- Query P90 PromQL back under 5000 ms; `AskerServiceDown`/`AskerQueryP90LatencyHigh` cleared.

---

## 2. Kafka / Redpanda consumer-group lag — freshness SLA at risk

**Protects:** ingest freshness ≤ 30 min.

### Symptoms
- `AskerIngestFreshnessP90High` (the P90 of `asker_index_doc_age_seconds` crossed the 1800 s
  bucket) and/or `AskerIngestFreshnessFractionLow` (the fraction landing ≤ 30 min dropped).
- Edits made at a source are not searchable within 30 minutes.

### Diagnosis
```sh
# Consumer-group lag per pipeline stage (ingest, enrich, index-writer consume their topics)
kubectl -n "$NS" exec -it <kafka-broker-or-redpanda-pod> -- \
  kafka-consumer-groups.sh --bootstrap-server localhost:9092 --describe --all-groups
# Dev (Redpanda):
docker compose exec redpanda rpk group list
docker compose exec redpanda rpk group describe <group>   # LAG column
```
```promql
# In-cluster freshness proxy P90 (now - Document.created_at at index time, ADR-017 §3)
histogram_quantile(0.9, sum by (le) (rate(asker_index_doc_age_seconds_bucket[10m])))
# Per-stage throughput — find the stalled stage
sum by (stage) (rate(asker_pipeline_records_total[5m]))
```

### Remediation
1. Identify the lagging stage from `asker_pipeline_records_total{stage=...}` and the consumer-group
   LAG. Scale that stateless consumer up: `kubectl -n "$NS" scale deploy/asker-<stage> --replicas=N`
   (the HPA may already be doing this; check `kubectl -n "$NS" get hpa`).
2. If `enrich` is the bottleneck, the TEI/CLIP embedding pool is usually the real limiter — see
   **#4 (TEI down/saturated)**; scaling enrich without TEI capacity just moves the queue.
3. If lag is from a broker outage, recover Kafka first; at-least-once delivery + idempotent writers
   (ADR-004) mean the backlog drains without data loss once consumers catch up.

### Verification
- Consumer-group LAG trending to ~0.
- Freshness P90 PromQL back under 1800 s; `AskerIngestFreshnessP90High` /
  `AskerIngestFreshnessFractionLow` cleared.
- A freshly edited source doc is searchable within the SLA (the M5 ingest suite's
  edit→searchable stopwatch is the whole-path check).

---

## 3. Keycloak outage or JWKS rotation breaking token validation

**Protects:** authenticated availability of every `/v1/*` route (a gateway that cannot fetch JWKS
cannot verify tokens).

### Symptoms
- `AskerGateway5xxRateHigh` or a spike in gateway 401s right after a Keycloak restart, a key
  rotation, or a gateway restart.
- `/readyz` on the gateway returns 503 (it probes the JWKS URL — `routes.go handleReadyz`).
- Users report being logged out / "unauthorized".

### Diagnosis
```sh
# Gateway readiness depends on JWKS reachability
kubectl -n "$NS" exec deploy/asker-gateway -- \
  curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8080/readyz
# Can the gateway reach Keycloak's JWKS? (default-deny needs the keycloak ingress allow, ADR-016)
kubectl -n "$NS" exec deploy/asker-gateway -- \
  curl -s -o /dev/null -w '%{http_code}\n' \
  http://keycloak:8080/realms/asker/protocol/openid-connect/certs
kubectl -n "$NS" get pods -l app.kubernetes.io/component=keycloak
```
Dev: `docker compose ps keycloak`, `curl -s http://localhost:8081/realms/asker/.well-known/openid-configuration`.

### Remediation
1. **Keycloak down:** restart/recover it (`kubectl -n "$NS" rollout restart deploy/asker-keycloak`
   or recover the managed instance). The gateway construct does **no** network I/O at startup
   (`oidc.NewRemoteKeySet` is lazy, `auth.go`), so the gateway stays up and self-heals once JWKS is
   reachable again — no gateway restart needed.
2. **NetworkPolicy blocking the JWKS dial** (common after enabling default-deny): confirm the
   `keycloak` ingress allow for the gateway exists (ADR-016 §1 matrix — `keycloak ← gateway`,
   port 8080). Without it a default-deny cluster 401s every request after a gateway restart.
3. **Key rotation:** go-oidc's `RemoteKeySet` refetches on an unknown `kid` (rate-limited to
   once/minute). A storm of unknown-`kid` tokens is bounded by the pre-auth throttle + the bounded
   JWKS client (`auth.go`); no action needed beyond letting the refetch happen.

### Verification
- Gateway `/readyz` → 200; `AskerGateway5xxRateHigh` cleared; 401 rate back to baseline.
- A fresh login + `/v1/me` succeeds.

---

## 4. TEI embedding service down or saturated (query and ingest degrade differently)

**Protects:** query latency SLO (graceful degradation) and ingest freshness (enrich throughput).

### Symptoms
- Query path: `AskerQueryDegradationSpike` (queries are silently going keyword-only) and/or
  `AskerQueryP90LatencyHigh`. The degradation ladder keeps queries answering, so you see a
  degradation spike *before* a hard error — that is by design (ADR-017 §5: "fast **and** not
  silently degraded").
- Ingest path: `AskerIngestFreshnessP90High` (enrich cannot embed chunks fast enough) and rising
  enrich consumer-group lag (**#2**).
- `AskerServiceDown` / `AskerScrapeTargetAbsent` for the TEI target if it is fully down.

### Diagnosis
```sh
kubectl -n "$NS" get pods -l app.kubernetes.io/component=tei
kubectl -n "$NS" exec deploy/asker-query -- curl -s -o /dev/null -w '%{http_code}\n' http://tei:80/health
```
```promql
# Are queries degrading to keyword-only? (rung label, ADR-017 §5)
sum by (rung) (rate(asker_query_degradation_events_total[5m]))
# Query latency by mode — keyword-only should be fast; hybrid/vector slow = TEI pressure
histogram_quantile(0.9, sum by (le, mode) (rate(asker_query_search_duration_milliseconds_bucket[5m])))
```

### Remediation
1. **Saturated:** scale the TEI pool (`kubectl -n "$NS" scale deploy/asker-tei --replicas=N`;
   capacity.md §2 sizes it from query-time + ingest-time embed QPS and the cache hit rate). Verify
   the query result cache is healthy (**#6**) — a cold cache multiplies TEI load.
2. **Down:** the query path **auto-degrades to keyword-only** and marks responses `degraded`
   (architecture.md §5) — queries keep answering, so this is a latency/quality incident, not an
   outage. Recover TEI; the ingest backlog (chunks awaiting embeddings) drains once it returns
   (at-least-once, ADR-004) — no data loss.
3. If TEI OOMs on the dev VM (8GB), this is expected under load; CI uses `bge-small` /
   `EMBEDDING_DIM=384` (ADR-003) — not a prod incident.

### Verification
- `asker_query_degradation_events_total{rung="keyword-only"}` rate back to ~0;
  `AskerQueryDegradationSpike` cleared.
- TEI `/health` → 200; enrich lag draining; freshness P90 back under SLA.

---

## 5. Postgres failover / control-plane unavailability

**Protects:** connector management, the token vault, quotas, and the GDPR delete path (the query
*read* path does NOT depend on Postgres).

### Symptoms
- `AskerServiceDown` / `AskerService5xxRateHigh` on `control-plane`; `AskerScrapeTargetAbsent` for
  Postgres.
- Connector CRUD (`/v1/connectors`) fails; syncs stop checkpointing; `DELETE /v1/me/data` fails.
- **Search keeps working** — query reads Vespa + Redis, not Postgres. This is the degradation
  ladder working in the operator's favor.

### Diagnosis
```sh
kubectl -n "$NS" get pods -l app.kubernetes.io/component=postgres
kubectl -n "$NS" exec deploy/asker-control-plane -- \
  curl -s -o /dev/null -w '%{http_code}\n' http://localhost:9101/healthz   # checks the pool
kubectl -n "$NS" logs deploy/asker-control-plane --tail=100 | grep -i 'postgres\|pool\|dial'
```
Dev: `docker compose ps postgres`, `docker compose exec postgres pg_isready`.

### Remediation
1. **Pod restart / failover:** recover the `asker-postgres` StatefulSet or fail over the managed
   primary. control-plane reconnects via its pgx pool; no control-plane restart needed for a
   transient blip.
2. **Data loss / corruption:** restore per [backup-restore-postgres.md](backup-restore-postgres.md)
   (the system of record for `tenants`, `connector_instances`, `tokens`, and **`tenant_deks`** —
   note: losing `tenant_deks` is **catastrophic**, it crypto-shreds every tenant; that runbook's
   RPO is your real data-durability bound).
3. While down, scheduled syncs pause and resume cleanly (cursors are persisted); the query read
   path is unaffected, so prioritize Postgres recovery without an emergency for search.

### Verification
- control-plane `/healthz` → 200; `/v1/connectors` round-trips.
- A sync pass checkpoints (sync state advances); `AskerServiceDown` cleared.

---

## 6. Redis outage (cache + rate limiting — must fail open)

**Protects:** query latency (cache) and abuse control (rate limiting) — but Redis is **non-critical
by design**: the path must fail open, never closed.

### Symptoms
- `AskerQueryCacheHitRateLow` (cache misses spike toward 100%) and a secondary
  `AskerQueryP90LatencyHigh` as every query goes cold to Vespa + TEI.
- `AskerScrapeTargetAbsent` for Redis.
- Per-tenant rate limiting stops counting (the limiter must fail open, per architecture.md §5).

### Diagnosis
```sh
kubectl -n "$NS" get pods -l app.kubernetes.io/component=redis
kubectl -n "$NS" exec deploy/asker-redis -- redis-cli ping     # PONG
```
```promql
sum(rate(asker_query_cache_requests_total{result="hit"}[5m]))
/ sum(rate(asker_query_cache_requests_total[5m]))
```
Dev: `docker compose exec redis redis-cli ping`.

### Remediation
1. Recover the `asker-redis` pod/instance. There is **nothing authoritative in Redis** — the query
   result cache (`q:<tenant>:*`) and rate-limit counters (`rl:<tenant>:*`) are derived and TTL'd, so
   a flush/restart loses only cache warmth, not data.
2. Confirm the query path **failed open** (queries still answered, just slower) — if a Redis error
   ever fails a query closed, that is a bug, file it; the design is degrade-not-fail.
3. Expect a TEI/Vespa load bump while the cache re-warms; pre-scale TEI (**#4**) if the bump
   threatens P90.

### Verification
- Cache hit-rate PromQL recovering toward its baseline; `AskerQueryCacheHitRateLow` cleared.
- Redis `ping` → PONG; query P90 back under SLO.

---

## 7. MinIO / S3 outage or blob corruption

**Protects:** media serving (`/v1/media`), upload ingest, and the GDPR blob purge — the **text
search path does NOT depend on MinIO** (bodies/snippets are in Vespa).

### Symptoms
- `AskerServiceDown` patterns on `connector-hub` (it owns the blob store) or upload/media 5xx;
  `AskerScrapeTargetAbsent` for MinIO if scraped.
- `GET /v1/media` (thumbnails/keyframes) and `POST /v1/upload` fail; text search is unaffected.

### Diagnosis
```sh
kubectl -n "$NS" get pods -l app.kubernetes.io/component=minio
kubectl -n "$NS" exec deploy/asker-connector-hub -- \
  curl -s -o /dev/null -w '%{http_code}\n' http://minio:9000/minio/health/ready
```
Dev: `docker compose ps minio`, `curl -s http://localhost:9000/minio/health/ready`.

### Remediation
1. **Pod restart:** recover the `asker-minio` StatefulSet; connector-hub reconnects.
2. **Data loss / corruption:** restore per [backup-restore-minio.md](backup-restore-minio.md)
   (bucket `asker-blobs`). Blobs are envelope-encrypted under the per-tenant DEK — a restore is
   only readable while the matching `tenant_deks` rows survive (do NOT restore a blob backup older
   than a tenant's DEK rotation without the matching keys).
3. Text search keeps working throughout; prioritize MinIO only for media/upload features.

### Verification
- MinIO `health/ready` → 200; `GET /v1/media` serves a known thumbnail; `POST /v1/upload`
  round-trips to searchable.

---

## 8. Connector stuck: OAuth token refresh failures / source API quota exhaustion

**Protects:** ingest freshness for the affected tenant's source (a stuck connector silently stops
delivering new docs).

### Symptoms
- No new `AskerDeadletterNonZero` (this is not a poison-doc problem) but a single tenant's
  freshness lags; `AskerIngestFreshnessFractionLow` if many connectors are stuck.
- connector-hub logs show repeated `sync pass failed` with backoff (`scheduler.go`), or the
  instance's `SyncState.last_error` records an auth/quota error.

### Diagnosis
```sh
kubectl -n "$NS" logs deploy/asker-connector-hub --tail=200 | grep -iE 'sync pass failed|token|401|403|quota|backoff'
# The instance's persisted sync state (phase FAILED + last_error) is the per-tenant signal:
#   GET /v1/connectors  ->  status/last_error per instance (tenant-scoped)
```
- A 401/403 from the source = expired/revoked OAuth token. A 429 = source API quota exhaustion.

### Remediation
1. **Token refresh failed (401/403):** the user must re-authorize. Surface it: the instance moves
   to `ERROR`/`FAILED` with `last_error` set; the user re-connects via `PUT /v1/connectors/{id}/token`
   (the hub fetches the decrypted token from the control-plane vault each pass, `scheduler.go`
   `fetchToken`). No operator data access — the token is the user's.
2. **Source quota (429):** the scheduler already backs off exponentially (5s → 5min cap,
   `backoffDelay`). If a tenant is abusively spawning instances, the per-tenant connector quota
   (security.md §3.1) and, if needed, an admin `POST /v1/admin/tenants/{tenant}/suspend` pause the
   tenant's connectors.
3. **Cursor expired at source:** handled automatically — the hub clears the cursor and restarts a
   full sync (`sdk.ErrCursorExpired`, `scheduler.go syncOnce`).

### Verification
- The instance returns to `ACTIVE`/`INCREMENTAL` with an empty `last_error`; docs flow again;
  freshness recovers for that tenant.

---

## 9. Poison documents filling the dead-letter topic (zero-data-loss alarm)

**Protects:** the **zero-data-loss** SLO. `asker_pipeline_deadletter_total` is the *authoritative*
loss signal (ADR-017 §4) — a record is counted only after it exhausts kafkautil's retries and is
quarantined to `docs.deadletter` (ADR-004), i.e. **actually dropped from the live pipeline**.

### Symptoms
- `AskerDeadletterNonZero` — any increase is a data-loss event, not a transient. Distinct from
  `AskerPipelineAttemptErrorRateHigh`, which counts *per-attempt* handler failures that retries
  recover (NOT loss; do not conflate).

### Diagnosis
```promql
# Authoritative loss — which origin topic is shedding?
sum by (origin_topic) (increase(asker_pipeline_deadletter_total[1h]))
# Per-attempt error rate (retryable; context, not loss)
sum by (stage) (rate(asker_pipeline_records_total{result="error"}[5m]))
```
```sh
# Inspect the quarantined records (they carry the original tenant_id header + the failure reason)
docker compose exec redpanda rpk topic consume docs.deadletter --num 10        # dev
# K8s: exec into a broker and kafka-console-consumer the docs.deadletter topic
```

### Remediation
1. Read a sample of `docs.deadletter` to find the poison pattern (a malformed Document, an
   oversized payload, an unparseable attachment). The records retain their `tenant_id` header, so
   you can scope the blast radius to affected tenants.
2. Fix the root cause (a connector emitting bad Documents, an ingest parser edge case). The
   pipeline is at-least-once + idempotent (ADR-004), so once the bug is fixed you can **re-drive**
   the quarantined records back onto their `origin_topic` and they will index correctly without
   duplicates (idempotent upserts keyed by `(doc_id, version_etag)`).
3. If the poison is a single tenant's hostile upload, the admin suspend/erase paths (security.md
   §2.1) contain it.

### Verification
- `increase(asker_pipeline_deadletter_total[...]) == 0` going forward (the soak pass condition).
- Re-driven records appear in search; `AskerDeadletterNonZero` clears once the rate is zero.

---

## 10. Query latency SLO burn (P90 > 5s) — the degradation ladder and rollback

**Protects:** the headline SLO, query **P90 ≤ 5000 ms**.

### Symptoms
- `AskerQueryP90LatencyHigh` (the 5000 ms SLO line) and/or `AskerQueryP50LatencyHigh` (the 800 ms
  design target). Possibly alongside `AskerQueryDegradationSpike` — meeting P90 *only by silently
  degrading* is itself an alert (ADR-017 §5).

### Diagnosis
```promql
# End-to-end query P90 / P50
histogram_quantile(0.9, sum by (le) (rate(asker_query_search_duration_milliseconds_bucket[5m])))
histogram_quantile(0.5, sum by (le) (rate(asker_query_search_duration_milliseconds_bucket[5m])))
# Split by mode + degraded to localize the cost (TEI? Vespa? cache cold?)
histogram_quantile(0.9, sum by (le, mode, degraded) (rate(asker_query_search_duration_milliseconds_bucket[5m])))
```
Walk the latency budget (architecture.md §5): gateway authn 20ms → cache 5ms → embed (TEI) 50ms →
Vespa 200–800ms → snippet 50ms. The two usual culprits are TEI (**#4**) and a cold cache (**#6**).

### Remediation
1. **Confirm the ladder is engaging** (it should, automatically): rerank is dropped first, then
   vector → keyword-only (`mode=keyword`), never failing closed. A `degraded` spike means the ladder
   is protecting availability — recover the degraded dependency (TEI/CLIP).
2. **Scale the hot tier:** query (stateless, HPA), TEI pool, or Vespa content nodes per capacity.md
   §1/§2/§3. Check `kubectl -n "$NS" get hpa`.
3. **If a recent deploy caused the burn:** roll back the stateless tier via
   [blue-green-deploy.md](blue-green-deploy.md) (image=values, Service-name contract; the stateful
   layer is migrated forward, not rolled back).

### Verification
- P90 PromQL back under 5000 ms (P50 under 800 ms); `AskerQueryP90LatencyHigh` /
  `AskerQueryP50LatencyHigh` cleared.
- Degradation events back to baseline (`AskerQueryDegradationSpike` cleared) — fast **and** not
  silently degraded.

---

## Escalation cross-reference

| Need | Runbook |
| :--- | :--- |
| Restore Postgres (incl. the catastrophic `tenant_deks` case) | [backup-restore-postgres.md](backup-restore-postgres.md) |
| Restore / rebuild the Vespa index | [backup-restore-vespa.md](backup-restore-vespa.md) |
| Restore MinIO blobs | [backup-restore-minio.md](backup-restore-minio.md) |
| Roll back a bad stateless deploy | [blue-green-deploy.md](blue-green-deploy.md) |
| Threat model, authz matrix, SSRF & GDPR guarantees | [../security.md](../security.md) |
| Scaling math (nodes for 50K TPS / 10PB) | [../capacity.md](../capacity.md) |
| Deploy to Kubernetes from scratch | [../../deploy/k8s-docs.md](../../deploy/k8s-docs.md) |
