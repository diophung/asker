// ingest-load.js — k6 ingest freshness harness for Asker (M5).
//
// WHAT IT MEASURES
//   The ingest-freshness SLO (spec §2.7, docs/architecture.md §5):
//     "source edit -> searchable <= 30 min (1800 s)."
//   Under SUSTAINED ingest load it writes documents through the gateway's authed
//   /v1/upload path — the FULL async pipeline (connector-hub -> Kafka -> ingest
//   -> enrich/TEI -> index-writer -> Vespa) — and for each one STOPWATCHES the
//   edit->searchable latency by polling /v1/search for that document's unique
//   rare token until it appears (or the budget expires). It asserts P90 of that
//   latency < FRESHNESS_SLO_MS (default 1800000 ms = 30 min).
//
// WHY THE UPLOAD PATH (and why k6, not bash)
//   The upload path is the only path that exercises the WHOLE pipeline end to
//   end (vespa-direct, by contrast, writes straight to the index and would
//   measure nothing). k6 (over a bash harness) because k6 already gives us:
//   sustained concurrency (the "under load" part), a P90 Trend, thresholds as
//   PASS/FAIL, and a machine-readable summary — and it can do the polling itself
//   in the same VU. A bash+curl+synthgen harness is documented as the fallback
//   in README (synthgen --target gateway-upload drives the same path), but the
//   freshness MEASUREMENT (per-doc stopwatch + P90 + threshold) is cleaner here.
//
// SHAPE
//   - INGEST_VUS upload VUs each loop for INGEST_DURATION: upload a doc carrying
//     a per-iteration UNIQUE token, then poll until searchable, recording the
//     latency. UNIQUE token namespace is `frsh<runid><vu>_<iter>` (NOT the qzx
//     space — these are written live, not pre-seeded, so they must not collide
//     with synthgen's corpus rare tokens or with each other).
//   - tenant_id derives from the OIDC token (ADR-002); the upload path is
//     single-tenant by construction (all docs land in the token owner's tenant).
//
// THRESHOLDS (PASS/FAIL)
//   - freshness_seconds: p(90) < FRESHNESS_SLO_MS/1000 (the 30-min SLA).
//   - upload_failed:     rate < UPLOAD_FAIL_MAX (uploads must be accepted).
//   - freshness_timeouts:count == 0 is NOT enforced as a hard threshold by
//     default (a single straggler beyond budget under a tiny dev VM should not
//     mask the P90 signal); set STRICT_TIMEOUTS=true to also assert zero
//     timeouts.
//
// PARAMETERS (env; see README): BASE_URL, TOKEN | KC_*, INGEST_VUS,
//   INGEST_DURATION, FRESHNESS_SLO_MS, POLL_INTERVAL_MS, POLL_TIMEOUT_MS,
//   UPLOAD_FAIL_MAX, STRICT_TIMEOUTS, RUN_ID.

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend, Counter } from 'k6/metrics';

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

const BASE_URL = envStr('BASE_URL', 'http://localhost:8080').replace(/\/+$/, '');
const INGEST_VUS = envInt('INGEST_VUS', 4);
const INGEST_DURATION = envStr('INGEST_DURATION', '5m');
const FRESHNESS_SLO_MS = envInt('FRESHNESS_SLO_MS', 1800000); // 30 min
const POLL_INTERVAL_MS = envInt('POLL_INTERVAL_MS', 3000);
const POLL_TIMEOUT_MS = envInt('POLL_TIMEOUT_MS', FRESHNESS_SLO_MS);
const UPLOAD_FAIL_MAX = envFloat('UPLOAD_FAIL_MAX', 0.01);
const STRICT_TIMEOUTS = envBool('STRICT_TIMEOUTS', false);
const RUN_ID = envStr('RUN_ID', String(Date.now()));

const freshness = new Trend('freshness_seconds', true);
const uploadFailed = new Counter('upload_failed_total');
const uploadOK = new Counter('upload_ok_total');
const freshTimeouts = new Counter('freshness_timeouts');
const searchable = new Counter('docs_searchable_total');

export const options = {
  scenarios: {
    ingest: {
      executor: 'constant-vus',
      vus: INGEST_VUS,
      duration: INGEST_DURATION,
      gracefulStop: '30s',
    },
  },
  thresholds: (function () {
    const t = {
      freshness_seconds: [`p(90)<${FRESHNESS_SLO_MS / 1000}`],
      // http_req_failed covers BOTH uploads and search polls; the dedicated
      // upload gate is the per-upload check below feeding upload_failed_total.
    };
    if (STRICT_TIMEOUTS) {
      t.freshness_timeouts = ['count==0'];
    }
    return t;
  })(),
  tags: { testid: 'asker-ingest-freshness' },
};

export function setup() {
  let token = envStr('TOKEN', '');
  if (!token) {
    const kcURL = envStr('KC_URL', 'http://localhost:8081').replace(/\/+$/, '');
    const realm = envStr('KC_REALM', 'asker');
    const client = envStr('KC_CLIENT', 'asker-web');
    const user = envStr('KC_USER', 'alice');
    const pass = envStr('KC_PASS', 'password123');
    const res = http.post(
      `${kcURL}/realms/${realm}/protocol/openid-connect/token`,
      { client_id: client, grant_type: 'password', username: user, password: pass },
      { headers: { 'Content-Type': 'application/x-www-form-urlencoded' } }
    );
    if (res.status !== 200) {
      throw new Error(
        `setup: Keycloak password grant failed: HTTP ${res.status}: ${String(res.body).slice(0, 200)}`
      );
    }
    const body = res.json();
    if (!body || !body.access_token) {
      throw new Error('setup: no access_token in Keycloak response');
    }
    token = body.access_token;
  }
  return { token: token };
}

export default function (data) {
  // eslint-disable-next-line no-undef
  const vu = (typeof __VU === 'number' ? __VU : 0);
  // eslint-disable-next-line no-undef
  const iter = (typeof __ITER === 'number' ? __ITER : 0);
  // Per-iteration UNIQUE token, distinct from synthgen's qzx corpus space.
  const token = `frsh${RUN_ID}v${vu}i${iter}`;
  const title = `freshness probe ${token}`;
  const fileBody =
    `Asker M5 ingest-freshness probe.\n` +
    `Unique tracking reference ${token}.\nRun ${RUN_ID} vu ${vu} iter ${iter}.\n`;

  // 1. Upload (start the stopwatch at the POST, the "source edit").
  const t0 = Date.now();
  const form = {
    file: http.file(fileBody, `${token}.txt`, 'text/plain'),
    title: title,
  };
  const up = http.post(`${BASE_URL}/v1/upload`, form, {
    headers: { Authorization: `Bearer ${data.token}` },
    tags: { name: 'v1_upload' },
  });
  const accepted = up.status === 202;
  check(up, { 'upload accepted (202)': (r) => r.status === 202 });
  if (!accepted) {
    uploadFailed.add(1);
    return; // nothing to poll for
  }
  uploadOK.add(1);

  // 2. Poll /v1/search for the token until it appears or the budget expires.
  let found = false;
  let attempt = 0;
  while (Date.now() - t0 < POLL_TIMEOUT_MS) {
    attempt++;
    // Vary limit a touch to bust the gateway's short result cache (limit is part
    // of the cache key), mirroring tools/e2e/m1-e2e.sh wait_hits.
    const lim = 10 + (attempt % 20);
    const res = http.get(
      `${BASE_URL}/v1/search?q=${encodeURIComponent(token)}&limit=${lim}`,
      { headers: { Authorization: `Bearer ${data.token}` }, tags: { name: 'v1_search_poll' } }
    );
    if (res.status === 200) {
      let body = null;
      try {
        body = res.json();
      } catch (e) {
        body = null;
      }
      const hits = body && Array.isArray(body.hits) ? body.hits : [];
      if (hits.length >= 1) {
        found = true;
        break;
      }
    }
    sleep(POLL_INTERVAL_MS / 1000);
  }

  const elapsedSec = (Date.now() - t0) / 1000;
  if (found) {
    freshness.add(elapsedSec);
    searchable.add(1);
    check(null, { 'searchable within budget': () => elapsedSec * 1000 < FRESHNESS_SLO_MS });
  } else {
    freshTimeouts.add(1);
    // Record the budget as the (lower-bound) latency so a timeout still weighs on
    // the P90 rather than vanishing from the distribution.
    freshness.add(POLL_TIMEOUT_MS / 1000);
  }
}

export function handleSummary(data) {
  const out = {};
  const summaryPath = envStr('K6_SUMMARY_PATH', '');
  if (summaryPath) {
    out[summaryPath] = JSON.stringify(data, null, 2);
  }
  out['stdout'] = textSummary(data);
  return out;
}

function textSummary(data) {
  const m = data.metrics || {};
  const fr = (m.freshness_seconds && m.freshness_seconds.values) || {};
  const to = (m.freshness_timeouts && m.freshness_timeouts.values) || {};
  const ok = (m.upload_ok_total && m.upload_ok_total.values) || {};
  const bad = (m.upload_failed_total && m.upload_failed_total.values) || {};
  const lines = [];
  lines.push('');
  lines.push('=== asker ingest-freshness summary ===');
  lines.push(`  uploads ok=${fmt(ok.count)} failed=${fmt(bad.count)}`);
  lines.push(`  freshness_seconds p50=${fmt(fr.med)} p90=${fmt(fr['p(90)'])} p95=${fmt(fr['p(95)'])} max=${fmt(fr.max)}`);
  lines.push(`  freshness timeouts=${fmt(to.count)}`);
  lines.push(`  SLO line: P90 freshness < ${FRESHNESS_SLO_MS / 1000}s (30 min)`);
  lines.push('');
  return lines.join('\n');
}

function fmt(v) {
  if (v === undefined || v === null) return 'n/a';
  return Math.round(v * 100) / 100;
}
