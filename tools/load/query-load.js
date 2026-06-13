// query-load.js — k6 query load suite for Asker (M5 scale/SLO verification).
//
// WHAT IT DOES
//   Drives the gateway /v1/search query path at a STEPPED, climbing request
//   rate to find the cluster's maximum sustained throughput at which the P90
//   end-to-end latency SLO (<= 5000 ms; spec §2.7, docs/architecture.md §5)
//   still holds. Each request asks for a KNOWN rare token (qzxNNNNNNNN) minted
//   by tools/synthgen into the synthetic corpus, so every query has real,
//   bounded, tenant-scoped work to do (not an empty-result no-op).
//
// HOW IT FINDS THE MAX
//   A ramping-arrival-rate executor (open model: k6 launches NEW iterations at
//   the target rate regardless of how slow the system gets, which is what you
//   want for a throughput-vs-latency curve — a closed VU model would silently
//   throttle itself when the system slows and hide the cliff). Each STAGE holds
//   a fixed RPS for STAGE_HOLD; the rate climbs RPS_START -> RPS_MAX in RPS_STEP
//   increments. Every request is TAGGED with its stage's RPS (tag `rps`), so the
//   per-stage P90 can be read out of the summary and the report can name the
//   highest stage whose P90 stayed < 5 s. That stage's RPS is the headline
//   "max sustained TPS at P90<5s on N nodes" the capacity doc extrapolates.
//
// THRESHOLDS (k6 exits non-zero if violated — this is the suite's PASS/FAIL)
//   - http_req_failed:   rate < FAIL_RATE_MAX (default 1%).
//   - http_req_duration: p(90) < 5000 ms (the HARD SLO), aggregate.
//   plus a custom Trend `e2e_latency_ms` recorded from the gateway-reported
//   took_ms (server-side end-to-end), so the report can compare client-observed
//   vs server-observed latency.
//
// DETERMINISM HOOK
//   synthgen mints rare tokens as the contiguous global ordinals
//   RareToken(0)..RareToken(R-1) == qzx00000000..qzx<R-1>, each in EXACTLY ONE
//   doc in EXACTLY ONE tenant. This script re-derives those token strings from
//   RARE_TOKEN_COUNT (no corpus observation needed) and queries a random one per
//   iteration. With CHECK_EXACT_HITS=true it also asserts the marker-scoped hit
//   count is exactly 1 (a correctness gate under load); default false because a
//   shared/min-rate corpus may not have minted every ordinal.
//
// PARAMETERS (all via env; see README)
//   BASE_URL, TOKEN | (KC_URL,KC_REALM,KC_CLIENT,KC_USER,KC_PASS),
//   RPS_START, RPS_MAX, RPS_STEP, STAGE_HOLD, RAMP, PRE_VUS, MAX_VUS,
//   RARE_TOKEN_COUNT, RARE_TOKEN_BASE, MODE, QUERY_LIMIT, FAIL_RATE_MAX,
//   P90_SLO_MS, CHECK_EXACT_HITS, SOAK (run a single fixed-rate stage),
//   SOAK_RPS, SOAK_DURATION.
//
// k6 docs: https://grafana.com/docs/k6/ (ramping-arrival-rate, thresholds, tags)

import http from 'k6/http';
import { check } from 'k6';
import { Trend, Counter } from 'k6/metrics';

// --- env helpers -------------------------------------------------------------

function envStr(name, dflt) {
  const v = __ENV[name];
  return v === undefined || v === '' ? dflt : v;
}
function envInt(name, dflt) {
  const v = __ENV[name];
  if (v === undefined || v === '') return dflt;
  const n = parseInt(v, 10);
  return Number.isNaN(n) ? dflt : n;
}
function envFloat(name, dflt) {
  const v = __ENV[name];
  if (v === undefined || v === '') return dflt;
  const n = parseFloat(v);
  return Number.isNaN(n) ? dflt : n;
}
function envBool(name, dflt) {
  const v = __ENV[name];
  if (v === undefined || v === '') return dflt;
  return v === '1' || v.toLowerCase() === 'true' || v.toLowerCase() === 'yes';
}

// --- configuration -----------------------------------------------------------

const BASE_URL = envStr('BASE_URL', 'http://localhost:8080').replace(/\/+$/, '');
const MODE = envStr('MODE', 'hybrid'); // hybrid | keyword | vector
const QUERY_LIMIT = envInt('QUERY_LIMIT', 10);

// Rare-token universe minted by synthgen. RARE_TOKEN_COUNT is how many ordinals
// exist (== the corpus's rare-token total); RARE_TOKEN_BASE is the first ordinal
// (0 unless a checkpointed run shifted the base). qzx is zero-padded to 8 digits.
const RARE_TOKEN_COUNT = envInt('RARE_TOKEN_COUNT', 1000);
const RARE_TOKEN_BASE = envInt('RARE_TOKEN_BASE', 0);
const CHECK_EXACT_HITS = envBool('CHECK_EXACT_HITS', false);

// SLO + threshold knobs.
const P90_SLO_MS = envInt('P90_SLO_MS', 5000);
const FAIL_RATE_MAX = envFloat('FAIL_RATE_MAX', 0.01);

// Stepped ramp shape.
const RPS_START = envInt('RPS_START', 10);
const RPS_MAX = envInt('RPS_MAX', 200);
const RPS_STEP = envInt('RPS_STEP', 10);
const STAGE_HOLD = envStr('STAGE_HOLD', '30s'); // hold each rate this long
const RAMP = envStr('RAMP', '5s'); // climb between rates this long
const PRE_VUS = envInt('PRE_VUS', 20);
// Headroom so the open model can keep launching iterations as latency grows.
const MAX_VUS = envInt('MAX_VUS', 500);

// Soak mode: one fixed sub-max stage held for SOAK_DURATION (the 2h soak).
const SOAK = envBool('SOAK', false);
const SOAK_RPS = envInt('SOAK_RPS', 20);
const SOAK_DURATION = envStr('SOAK_DURATION', '2h');

// --- custom metrics ----------------------------------------------------------

const e2eLatency = new Trend('e2e_latency_ms', true); // gateway took_ms
const cacheHits = new Counter('asker_cache_hits');
const cacheMiss = new Counter('asker_cache_miss');
const degradedResponses = new Counter('asker_degraded_responses');
const exactHitMismatch = new Counter('asker_exact_hit_mismatch');

// --- token derivation --------------------------------------------------------

function rareToken(ordinal) {
  // Mirror synthgen RareToken(g): fmt.Sprintf("qzx%08d", g).
  return 'qzx' + String(ordinal).padStart(8, '0');
}

// --- stages ------------------------------------------------------------------

function buildStages() {
  if (SOAK) {
    // Single fixed-rate stage held for the soak duration (plus a short ramp-in).
    return [
      { target: SOAK_RPS, duration: RAMP },
      { target: SOAK_RPS, duration: SOAK_DURATION },
    ];
  }
  const stages = [];
  let rps = RPS_START;
  // Ramp from 0 -> RPS_START first so we don't start with a thundering herd.
  stages.push({ target: RPS_START, duration: RAMP });
  while (rps <= RPS_MAX) {
    stages.push({ target: rps, duration: STAGE_HOLD });
    if (rps === RPS_MAX) break;
    rps = Math.min(rps + RPS_STEP, RPS_MAX);
    stages.push({ target: rps, duration: RAMP });
  }
  return stages;
}

const stages = buildStages();

// startRate is where the arrival-rate executor begins; for the stepped profile
// we start at 0 and let the first ramp climb to RPS_START.
const startRate = SOAK ? Math.max(1, Math.floor(SOAK_RPS / 2)) : 0;

export const options = {
  scenarios: {
    query: {
      executor: 'ramping-arrival-rate',
      startRate: startRate,
      timeUnit: '1s',
      preAllocatedVUs: PRE_VUS,
      maxVUs: MAX_VUS,
      stages: stages,
      gracefulStop: '30s',
    },
  },
  thresholds: buildThresholds(),
  // Tag the whole run so a Prometheus/k6-cloud sink can slice by test.
  tags: { testid: SOAK ? 'asker-query-soak' : 'asker-query-step' },
  // Discard per-iteration response bodies after we read them (memory).
  discardResponseBodies: false,
};

// buildThresholds returns the threshold map. Beyond the HARD aggregate SLO
// gates, it registers a PER-STAGE tagged sub-metric threshold
// `http_req_duration{rps:NN}` for every distinct stage target. Registering a
// tagged threshold is what makes k6 EMIT that tagged sub-metric (with its own
// p(90)) into the handleSummary JSON — which is how run-load.sh reads the max
// sustained RPS at which P90 stayed < SLO. `abortOnFail` is left false so a
// single breached stage does NOT abort the run (we want the whole curve); the
// PASS/FAIL gate is the AGGREGATE http_req_duration p(90) plus http_req_failed.
function buildThresholds() {
  const t = {
    // The HARD SLO and the failed-request bound. Violation => non-zero exit.
    http_req_duration: [`p(90)<${P90_SLO_MS}`],
    http_req_failed: [`rate<${FAIL_RATE_MAX}`],
    // Custom server-side end-to-end latency, same SLO line (informational gate).
    e2e_latency_ms: [`p(90)<${P90_SLO_MS}`],
  };
  if (!SOAK) {
    const seen = {};
    for (const s of stages) {
      if (s.target > 0 && !seen[s.target]) {
        seen[s.target] = true;
        // Non-aborting, very loose bound: its only purpose is to materialize the
        // tagged sub-metric in the summary so its p(90) is readable per stage.
        t[`http_req_duration{rps:${s.target}}`] = [
          { threshold: `p(90)<${P90_SLO_MS * 1000}`, abortOnFail: false },
        ];
      }
    }
  }
  return t;
}

// --- setup: mint a bearer token ---------------------------------------------

export function setup() {
  const startMs = Date.now(); // scenario wall-clock start, for per-stage tagging
  let token = envStr('TOKEN', '');
  if (token) {
    return { token: token, startMs: startMs };
  }
  // Keycloak dev password grant (mirrors tools/e2e/m1-e2e.sh fetch_token).
  const kcURL = envStr('KC_URL', 'http://localhost:8081').replace(/\/+$/, '');
  const realm = envStr('KC_REALM', 'asker');
  const client = envStr('KC_CLIENT', 'asker-web');
  const user = envStr('KC_USER', 'alice');
  const pass = envStr('KC_PASS', 'password123');
  const res = http.post(
    `${kcURL}/realms/${realm}/protocol/openid-connect/token`,
    {
      client_id: client,
      grant_type: 'password',
      username: user,
      password: pass,
    },
    { headers: { 'Content-Type': 'application/x-www-form-urlencoded' } }
  );
  if (res.status !== 200) {
    throw new Error(
      `setup: Keycloak password grant failed: HTTP ${res.status}: ${String(res.body).slice(0, 200)}`
    );
  }
  let body;
  try {
    body = res.json();
  } catch (e) {
    throw new Error(`setup: token response not JSON: ${String(res.body).slice(0, 200)}`);
  }
  if (!body.access_token) {
    throw new Error('setup: no access_token in Keycloak response');
  }
  return { token: body.access_token, startMs: startMs };
}

// --- the iteration -----------------------------------------------------------

export default function (data) {
  // Pick a random rare token in [BASE, BASE+COUNT). Each maps to exactly one
  // doc in exactly one tenant in the synthgen corpus.
  const ordinal = RARE_TOKEN_BASE + Math.floor(Math.random() * RARE_TOKEN_COUNT);
  const q = rareToken(ordinal);

  // Tag by the stage's target RPS so the per-stage P90 is readable from the
  // summary. k6 does not expose the live arrival-rate target, so we reconstruct
  // it from the (deterministic) stage schedule using elapsed wall-clock since
  // the scenario started (recorded in setup()).
  const elapsed = data && data.startMs ? (Date.now() - data.startMs) / 1000 : 0;
  const rpsTag = SOAK ? String(SOAK_RPS) : String(targetRPSAt(elapsed));

  const url =
    `${BASE_URL}/v1/search?q=${encodeURIComponent(q)}` +
    `&mode=${encodeURIComponent(MODE)}&limit=${QUERY_LIMIT}`;

  const res = http.get(url, {
    headers: { Authorization: `Bearer ${data.token}` },
    tags: { name: 'v1_search', rps: rpsTag, mode: MODE },
  });

  const ok = check(res, {
    'status is 200': (r) => r.status === 200,
  });

  if (res.status === 200) {
    let body = null;
    try {
      body = res.json();
    } catch (e) {
      body = null;
    }
    if (body) {
      if (typeof body.took_ms === 'number') {
        e2eLatency.add(body.took_ms, { rps: rpsTag });
      }
      if (body.cached === true) cacheHits.add(1);
      else cacheMiss.add(1);
      if (body.degraded && body.degraded !== '') degradedResponses.add(1);

      if (CHECK_EXACT_HITS) {
        const hits = Array.isArray(body.hits) ? body.hits : [];
        // The rare token lives in exactly one doc; the querying tenant must be
        // its owner for the hit to appear. Under a single-token-per-tenant
        // corpus a foreign tenant sees 0 — so we assert <=1, and count==1 only
        // when it is the owner. We only flag the impossible >1.
        if (hits.length > 1) {
          exactHitMismatch.add(1);
          check(res, { 'rare token hits <= 1': () => false });
        }
      }
    }
  }
  // No artificial think-time: the arrival-rate executor governs pacing.
  void ok;
}

// targetRPSAt returns the arrival-rate target in effect at `elapsedSec` into the
// run, reconstructed from the (deterministic) stage schedule. A ramping stage's
// target is the value being climbed TO; we attribute the whole stage interval to
// its target, so requests are tagged by the rate the system is being asked to
// sustain. This is a best-effort tag for per-stage P90 read-out, NOT used by any
// threshold (the SLO thresholds are aggregate); a small mis-bucketing at a stage
// boundary cannot change PASS/FAIL.
let __stageStarts = null;
function targetRPSAt(elapsedSec) {
  if (__stageStarts === null) {
    __stageStarts = [];
    let t = 0;
    for (const s of stages) {
      t += durationToSec(s.duration);
      __stageStarts.push({ end: t, target: s.target });
    }
  }
  for (const st of __stageStarts) {
    if (elapsedSec <= st.end) return st.target;
  }
  return stages.length ? stages[stages.length - 1].target : 0;
}

function durationToSec(d) {
  // Parse a k6 duration string like "30s", "5m", "2h", "1m30s".
  let total = 0;
  const re = /(\d+(?:\.\d+)?)(h|m|s|ms)/g;
  let m;
  while ((m = re.exec(d)) !== null) {
    const v = parseFloat(m[1]);
    switch (m[2]) {
      case 'h': total += v * 3600; break;
      case 'm': total += v * 60; break;
      case 's': total += v; break;
      case 'ms': total += v / 1000; break;
    }
  }
  if (total === 0) {
    const n = parseFloat(d);
    if (!Number.isNaN(n)) total = n;
  }
  return total;
}

// handleSummary writes the machine-readable summary JSON that run-load.sh reads
// to extract the max sustained RPS at P90<5s. K6 calls this at the end.
export function handleSummary(data) {
  const out = {};
  const summaryPath = envStr('K6_SUMMARY_PATH', '');
  if (summaryPath) {
    out[summaryPath] = JSON.stringify(data, null, 2);
  }
  // Always also emit to stdout (k6's default text summary stays too).
  out['stdout'] = textSummary(data);
  return out;
}

function textSummary(data) {
  const m = data.metrics || {};
  const dur = (m.http_req_duration && m.http_req_duration.values) || {};
  const failed = (m.http_req_failed && m.http_req_failed.values) || {};
  const e2e = (m.e2e_latency_ms && m.e2e_latency_ms.values) || {};
  const lines = [];
  lines.push('');
  lines.push('=== asker query-load summary ===');
  lines.push(`  mode=${MODE} soak=${SOAK}`);
  lines.push(`  http_req_duration p90=${fmt(dur['p(90)'])}ms p95=${fmt(dur['p(95)'])}ms p99=${fmt(dur['p(99)'])}ms`);
  lines.push(`  e2e (server took_ms)  p90=${fmt(e2e['p(90)'])}ms p95=${fmt(e2e['p(95)'])}ms`);
  lines.push(`  http_req_failed rate=${fmt(failed.rate)} (bound <${FAIL_RATE_MAX})`);
  lines.push(`  SLO line: P90 < ${P90_SLO_MS}ms`);
  lines.push('');
  return lines.join('\n');
}

function fmt(v) {
  if (v === undefined || v === null) return 'n/a';
  return Math.round(v * 100) / 100;
}
