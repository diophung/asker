export const meta = {
  name: 'asker-m1-review',
  description: 'Adversarial review of M1 code: 5 dimension reviewers, findings verified by skeptics',
  phases: [
    { title: 'Review', detail: '5 parallel dimension reviewers' },
    { title: 'Verify', detail: 'adversarial verification of each finding' },
  ],
}

const FINDINGS = {
  type: 'object',
  required: ['findings'],
  properties: {
    findings: {
      type: 'array',
      items: {
        type: 'object',
        required: ['title', 'file', 'severity', 'detail'],
        properties: {
          title: { type: 'string' },
          file: { type: 'string' },
          severity: { type: 'string', enum: ['blocker', 'major', 'minor'] },
          detail: { type: 'string' },
          fix: { type: 'string' },
        },
      },
    },
  },
}

const VERDICT = {
  type: 'object',
  required: ['isReal', 'reasoning'],
  properties: {
    isReal: { type: 'boolean' },
    reasoning: { type: 'string' },
    severityAdjustment: { type: 'string' },
  },
}

const PREAMBLE = [
  'You are reviewing milestone M1 ("vertical slice") of Asker, a multi-tenant personal search engine,',
  'at /Users/dio/works/asker. M1 scope: connector-hub (scheduler, token vault, webhooks, upload),',
  'gmail + upload connectors, ingest (chunk/dedupe), enrich (Python embeddings), index-writer, query',
  'service (hybrid + degradation), gateway REST (search/connectors/upload/rate-limit/CORS), control-plane,',
  'platform libs (kafkautil, crypto, tenancy, blob). Read the spec (specs/asker-v1-personal-search-engine.md',
  'M1 + sections 2.4-2.6) and docs/adr/ADR-004..009. Do NOT flag intentionally-deferred items: pure-vector',
  'or RRF recall (M5), gRPC plugin transport (M2), more connectors (M2), media (M3), Helm/Vault/mTLS/otelgrpc',
  '(M4), single-replica rate-limit clock skew. The dev VM is memory-constrained — do NOT start/stop/restart',
  'compose services or run dev-up/dev-down; you MAY read code, run go test on individual packages, grep, and',
  'read docker logs read-only. Report only findings with concrete file-and-line evidence. The biggest known',
  'bug (hybrid OR-nearestNeighbor matching everything) is ALREADY FIXED in services/query/vespa.go — verify',
  'the fix is correct/complete rather than re-reporting it.',
].join(' ')

const DIMS = [
  {
    key: 'tenancy-isolation',
    body: 'Dimension: TENANCY ISOLATION (the sacred property). Audit every path where tenant scoping must hold: '
      + 'query service (streaming.groupname ALWAYS from ctx tenant never request fields; YQL injection safety; '
      + 'the hybrid fix in services/query/vespa.go — does the keyword match-set fully scope to the group, can any '
      + 'mode/filter combination widen beyond the tenant); gateway (tenant from verified JWT only on every new '
      + 'route search/connectors/upload/token; x-asker-tenant only set OUTBOUND to hub never trusted inbound from '
      + 'clients; rate-limit keys per-tenant); connector-hub (emit chokepoint doc.tenant_id == instance tenant; '
      + 'SchedulerService cross-tenant enumeration exemption — is it EXACTLY scoped to ListAllInstances, can a '
      + 'client reach SchedulerService; webhook/upload tenant derivation); control-plane (every RPC filters by ctx '
      + 'tenant, cross-tenant returns NotFound with no oracle, token vault per-tenant, blob tenant-prefix fail '
      + 'closed); kafkautil (tenant header re-validation on consume, producer chokepoint). Hunt for ANY way tenant '
      + "A's data reaches tenant B. Run go test on suspect packages if useful.",
  },
  {
    key: 'pipeline-correctness',
    body: 'Dimension: PIPELINE CORRECTNESS & IDEMPOTENCY. Audit the async pipeline: at-least-once + idempotency '
      + '(ingest dedupe SETNX race window, index-writer upsert keyed by doc_id+version_etag, enrich '
      + 'commit-after-produce — can a crash duplicate or lose a doc); tombstone/delete propagation through ingest '
      + '-> enrich -> index-writer -> Vespa DELETE (does a delete actually remove within SLA; edit-at-source Gmail '
      + 'messageDeleted+messageAdded same id producing an upsert not a delete); chunking (byte-offset correctness, '
      + 'structure-aware email splitting, overlap, cap, unicode); enrich Python (embedding dim validation, batching '
      + 'math, snappy decode, deadletter parity, tenant mismatch); gmail connector (cursor checkpoint/resume, '
      + 'history replay ordering last-event-per-id, ErrCursorExpired -> full sync, pagination); index-writer (feed '
      + 'JSON tensor blocks, enum names, epoch seconds, dimension-mismatch reject). Hunt data-loss, duplication, '
      + 'ordering, freshness-SLA bugs.',
  },
  {
    key: 'query-degradation',
    body: 'Dimension: QUERY PATH & DEGRADATION. Focus services/query + gateway search: the hybrid fix '
      + '(services/query/vespa.go) — is the new buildYQL correct for all four retrievalKinds, does retrieveVector '
      + 'still work (nearestNeighbor match set), does filter-only + hybrid interact right, are filters '
      + '(type/date/participant) correctly ANDed and still tenant-scoped; degradation ladder (TEI failure -> '
      + 'keyword-only degraded flag; Vespa hybrid 5xx -> keyword retry; vector-mode TEI down -> error; never fail '
      + 'closed on degradable paths — verify code matches ADR-006); query understanding (from:/type:/before:/after: '
      + 'extraction edge cases, residual text, empty-residual-with-filters -> filter-only); cache (key includes '
      + 'tenant + normalized request, TTL, stale-after-write, Redis-down skip); snippet/highlight assembly (<hi> '
      + 'handling, fallback chain, XSS-safety of what reaches gateway JSON). Run go test ./services/query/.',
  },
  {
    key: 'security-crypto',
    body: 'Dimension: SECURITY (crypto, secrets, SSRF, input). Audit: platform/crypto envelope encryption '
      + '(AES-GCM nonce uniqueness, tenant-bound AAD, version prefix, DEK cache concurrency, FileKEK permissions; '
      + "connector-hub file DEK store first-writer-wins); platform/blob (tenant-prefix enforcement on Get fail "
      + 'closed, encrypt-before-store, sha256 verify); gmail connector + connector-hub fetchers SSRF — base_url and '
      + 'webhook_url are tenant-controllable config, can a tenant point a connector at an internal address '
      + '(169.254.169.254, localhost, control-plane:9100) and exfiltrate, is there URL validation (note honestly '
      + 'whether M1-acceptable given dev trust model but FLAG if tenant-supplied base_url is fetched unvalidated '
      + 'server-side); gateway (upload size cap via MaxBytesReader, multipart handling, CORS not star-with-'
      + 'credentials, rate-limit fail-open risk, JSON error leakage); secrets (no creds in code/logs, tokens '
      + 'encrypted at rest, do any logs print token bytes or doc bodies). SSRF in connector fetchers is in scope.',
  },
  {
    key: 'robustness-quality',
    body: 'Dimension: ROBUSTNESS & GO/PYTHON QUALITY. Hunt real bugs (not style): goroutine leaks (scheduler '
      + 'workers, consumers), unbounded reads/memory, missing context timeouts, HTTP clients without timeouts, '
      + 'resource leaks (unclosed bodies/conns), nil-deref, races (run go test -race on connector-hub, kafkautil, '
      + 'control-plane); connector-hub scheduler (worker lifecycle stop-on-remove/pause, backoff correctness, the '
      + 'ListAllInstances-deadline==tick coupling at services/connector-hub/internal/hub/scheduler.go:155 — is a '
      + '10s deadline tied to tick interval a latent timeout bug under load, concurrent sync bounding); error '
      + 'handling (swallowed errors, errors that should deadletter vs crash, partial-failure); health/readiness '
      + 'correctness, graceful shutdown ordering, config validation (required env, EMBEDDING_DIM); enrich Python '
      + '(asyncio correctness, commit ordering, exception paths). Run go test -race on heavier service packages.',
  },
]

function verifyPrompt(f) {
  return PREAMBLE + '\n\nA reviewer claims this issue. Try to REFUTE it: read the actual files, run read-only '
    + 'checks, decide if it is a real, actionable M1-scope bug (not style, not deferred, not already handled). If '
    + 'you cannot reproduce the evidence or it misreads the code, it is refuted. Default isReal=false when uncertain.'
    + '\n\nCLAIM [' + f.severity + '] ' + f.title + '\nFile: ' + f.file + '\nDetail: ' + f.detail
    + '\nFix: ' + (f.fix || '(none)')
}

function reviewStage(d) {
  return agent(PREAMBLE + '\n\n' + d.body, { label: 'review:' + d.key, phase: 'Review', schema: FINDINGS })
}

function verifyStage(review, d) {
  const fs = (review && review.findings) || []
  if (!fs.length) return []
  const thunks = fs.map(function (f) {
    return function () {
      return agent(verifyPrompt(f), { label: 'verify:' + d.key, phase: 'Verify', schema: VERDICT })
        .then(function (v) { return Object.assign({}, f, { dimension: d.key, verdict: v }) })
    }
  })
  return parallel(thunks)
}

phase('Review')
const results = await pipeline(DIMS, reviewStage, verifyStage)

const all = results.filter(Boolean).flat().filter(Boolean)
const confirmed = all.filter(function (f) { return f.verdict && f.verdict.isReal })
const refuted = all.filter(function (f) { return !f.verdict || !f.verdict.isReal })
log(confirmed.length + ' confirmed, ' + refuted.length + ' refuted of ' + all.length)

return {
  confirmed: confirmed.map(function (f) {
    return {
      severity: (f.verdict.severityAdjustment || f.severity),
      dimension: f.dimension,
      title: f.title,
      file: f.file,
      detail: f.detail,
      fix: f.fix,
      verifierNote: f.verdict.reasoning,
    }
  }),
  refuted: refuted.map(function (f) {
    return {
      severity: f.severity,
      dimension: f.dimension,
      title: f.title,
      file: f.file,
      why: f.verdict ? f.verdict.reasoning : 'verifier failed',
    }
  }),
}
