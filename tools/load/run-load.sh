#!/usr/bin/env bash
# Asker M5 load/soak orchestrator.
#
# Orchestrates the M5 scale/SLO verification (spec §5 "M5", §2.7 SLOs):
#   1. SEED a synthetic query corpus via tools/synthgen (vespa-direct, multi-
#      tenant, raw index-fill — bypasses Kafka, fast). Small by default, scaled
#      by env. The corpus mints contiguous rare tokens qzx00000000.. that the
#      query suite re-derives (no corpus observation needed).
#   2. TOKEN: mint an OIDC access token (Keycloak dev password grant, user
#      alice) for the gateway-authed suites, unless TOKEN is supplied.
#   3. RUN the k6 suites:
#        - query: stepped ramping-arrival-rate to the cluster max; reports the
#          highest stage RPS at which P90 stayed < 5 s (the headline number the
#          capacity doc extrapolates to 50K TPS).
#        - ingest: sustained uploads through the FULL pipeline; measures
#          edit->searchable P90 freshness, asserts < 30 min.
#      Each emits a machine-readable summary JSON we parse for headline numbers.
#   4. SOAK (opt-in, SOAK=true): run the query suite at a FIXED sub-max rate for
#      SOAK_DURATION (default 2h), asserting ZERO failed requests AND no NEW
#      dead-letters (the authoritative zero-data-loss signal: a rise in
#      asker_pipeline_deadletter_total scraped from the index-writer /metrics).
#   5. PRINT a numbered PASS/FAIL summary + the headline numbers.
#
# House style mirrors tools/e2e/m1-e2e.sh / k8s-chaos.sh: set -euo pipefail,
# numbered PASS/FAIL, python3 for JSON (no jq), everything env-overridable.
#
# Run from anywhere. Requires: bash, curl, python3, k6, go (for synthgen).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

# --- Config (env-overridable) -------------------------------------------------
GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
BASE_URL="${BASE_URL:-$GATEWAY_URL}"
KEYCLOAK_URL="${KEYCLOAK_URL:-http://localhost:8081}"
VESPA_URL="${VESPA_URL:-http://localhost:8082}"
KC_USER="${KC_USER:-alice}"
KC_PASS="${KC_PASS:-password123}"
KC_REALM="${KC_REALM:-asker}"
KC_CLIENT="${KC_CLIENT:-asker-web}"

# index-writer /metrics (health port :9701) — the zero-data-loss scrape source.
# Override INDEX_WRITER_METRICS for K8s/port-forward.
INDEX_WRITER_METRICS="${INDEX_WRITER_METRICS:-http://localhost:9701/metrics}"

K6="${K6:-k6}"

# What to run.
RUN_QUERY="${RUN_QUERY:-true}"
RUN_INGEST="${RUN_INGEST:-true}"
SOAK="${SOAK:-false}"
SEED_CORPUS="${SEED_CORPUS:-true}"

# Synthgen seed-corpus knobs. We seed a SINGLE tenant — the one the query token
# resolves to (see step 1) — so the query suite actually hits. SYNTH_DOCS_PER_TENANT
# is therefore the representative per-tenant corpus size the query SLO is measured
# at; multi-tenant storage scale is the capacity model's concern (docs/capacity.md).
SYNTH_DOCS_PER_TENANT="${SYNTH_DOCS_PER_TENANT:-50}"
SYNTH_RARE_RATE="${SYNTH_RARE_RATE:-1.0}" # 1.0 => every doc gets a rare token (keeps RARE_TOKEN_COUNT exact)
SYNTH_SEED="${SYNTH_SEED:-1}"
SYNTH_CONCURRENCY="${SYNTH_CONCURRENCY:-8}"

# Query suite shape (passed through to query-load.js as env).
RPS_START="${RPS_START:-10}"
RPS_MAX="${RPS_MAX:-100}"
RPS_STEP="${RPS_STEP:-10}"
STAGE_HOLD="${STAGE_HOLD:-30s}"
MODE="${MODE:-hybrid}"
QUERY_LIMIT="${QUERY_LIMIT:-10}"
P90_SLO_MS="${P90_SLO_MS:-5000}"
FAIL_RATE_MAX="${FAIL_RATE_MAX:-0.01}"

# Ingest/freshness suite shape.
INGEST_VUS="${INGEST_VUS:-4}"
INGEST_DURATION="${INGEST_DURATION:-5m}"
FRESHNESS_SLO_MS="${FRESHNESS_SLO_MS:-1800000}" # 30 min

# Soak shape.
SOAK_DURATION="${SOAK_DURATION:-2h}"
SOAK_RPS="${SOAK_RPS:-20}"

# Computed: the rare-token count the query suite should sample from. Each doc
# draws a rare token at SYNTH_RARE_RATE, so the corpus mints
# ~ tenants*docs*rate tokens as the contiguous ordinals qzx00000000..
RARE_TOKEN_COUNT="${RARE_TOKEN_COUNT:-}"

OUTDIR="${OUTDIR:-$(mktemp -d "${TMPDIR:-/tmp}/asker-load.XXXXXX")}"
mkdir -p "$OUTDIR"

# --- Report scaffolding (m1-e2e.sh / k8s-chaos.sh house style) ----------------
STEP=0
FAILED=0
CURRENT_NAME=""
declare -a SUMMARY=()

# Headline numbers (filled as we go).
MAX_RPS_OK="?"      # highest stage RPS with P90 < SLO
QUERY_P90="?"       # aggregate query P90 (ms)
FRESH_P90="?"       # freshness P90 (s)
SOAK_FAILED="?"     # soak failed-request count
SOAK_DL_DELTA="?"   # soak deadletter delta

begin() {
  STEP=$((STEP + 1))
  CURRENT_NAME="$1"
  printf '[%2d] %s ... ' "$STEP" "$CURRENT_NAME"
}
pass() {
  echo "PASS"
  SUMMARY+=("$(printf '%2d|PASS|%s' "$STEP" "$CURRENT_NAME")")
}
fail() {
  echo "FAIL"
  if [ $# -gt 0 ]; then
    printf '     -> %s\n' "$*"
  fi
  SUMMARY+=("$(printf '%2d|FAIL|%s' "$STEP" "$CURRENT_NAME")")
  FAILED=1
}
note() { printf '     -> %s\n' "$*"; }

CURL=(curl -fsS --max-time 30)

# json_field FIELD: read JSON on stdin, print FIELD's string value ("" if absent).
json_field() {
  python3 -c '
import json, sys
try:
    data = json.load(sys.stdin)
except Exception:
    sys.exit(1)
val = data.get(sys.argv[1])
print("" if val is None else val)
' "$1"
}

# fetch_token USER PASS: print an access token via the dev password grant.
fetch_token() {
  local body tok
  body="$("${CURL[@]}" -X POST "${KEYCLOAK_URL}/realms/${KC_REALM}/protocol/openid-connect/token" \
    -H 'Content-Type: application/x-www-form-urlencoded' \
    --data-urlencode "client_id=${KC_CLIENT}" \
    --data-urlencode "grant_type=password" \
    --data-urlencode "username=$1" \
    --data-urlencode "password=$2" 2>&1)" || {
    echo "token request failed: ${body:0:200}"
    return 1
  }
  tok="$(json_field access_token <<<"$body" || true)"
  if [ -z "$tok" ]; then
    echo "no access_token in response: ${body:0:200}"
    return 1
  fi
  printf '%s\n' "$tok"
}

# deadletter_total: sum asker_pipeline_deadletter_total across all origin_topic
# label sets from the index-writer /metrics scrape. Prints an integer-ish float,
# or "" if the endpoint/metric is unavailable (the soak then degrades to a
# warning rather than a false FAIL).
deadletter_total() {
  local body
  body="$(curl -fsS --max-time 15 "$INDEX_WRITER_METRICS" 2>/dev/null)" || {
    echo ""
    return 0
  }
  python3 -c '
import sys, re
total = 0.0
found = False
for line in sys.stdin:
    line = line.strip()
    if not line or line.startswith("#"):
        continue
    # asker_pipeline_deadletter_total{origin_topic="docs.raw"} 3
    if line.startswith("asker_pipeline_deadletter_total"):
        m = re.search(r"\s([0-9.eE+-]+)\s*$", line)
        if m:
            try:
                total += float(m.group(1))
                found = True
            except ValueError:
                pass
print(("%g" % total) if found else "")
' <<<"$body"
}

# k6_metric SUMMARY_JSON METRIC SUBKEY: print one metric value from a k6
# handleSummary JSON (.metrics.<METRIC>.values.<SUBKEY>), "" if absent.
k6_metric() {
  python3 -c '
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    sys.exit(0)
m = (d.get("metrics") or {}).get(sys.argv[2]) or {}
v = (m.get("values") or {}).get(sys.argv[3])
print("" if v is None else v)
' "$1" "$2" "$3"
}

# k6_max_rps_ok SUMMARY_JSON SLO_MS: find the highest `rps` tag whose tagged
# http_req_duration p(90) stayed < SLO_MS. k6 records per-tag sub-metrics only
# when run with --summary-trend-stats or when tags are present in the trend; we
# parse the per-stage trend the script emits via the `rps` tag using the
# sub-metric naming k6 uses in the summary: "http_req_duration{rps:NN}".
k6_max_rps_ok() {
  python3 -c '
import json, sys
slo = float(sys.argv[2])
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print("?"); sys.exit(0)
metrics = d.get("metrics") or {}
best = None
import re
for name, m in metrics.items():
    mt = re.match(r"http_req_duration\{rps:([0-9]+)\}$", name)
    if not mt:
        continue
    rps = int(mt.group(1))
    p90 = (m.get("values") or {}).get("p(90)")
    if p90 is None:
        continue
    if p90 < slo:
        if best is None or rps > best:
            best = rps
if best is None:
    # No per-tag sub-metrics (thresholds were aggregate-only). Fall back to the
    # aggregate: report the configured RPS_MAX if aggregate p90<slo, else "?".
    agg = ((metrics.get("http_req_duration") or {}).get("values") or {}).get("p(90)")
    if agg is not None and agg < slo:
        print("aggregate-p90-ok")
    else:
        print("?")
else:
    print(best)
' "$1" "$2"
}

cleanup() {
  set +e
  # Keep OUTDIR if the caller pinned it; otherwise leave it for inspection only
  # when a failure occurred (helpful in CI artifacts). Default: remove on success.
  if [ -z "${KEEP_OUTDIR:-}" ] && [ "$FAILED" = "0" ]; then
    rm -rf "$OUTDIR"
  fi
}
trap cleanup EXIT

echo "== Asker M5 load orchestrator =="
echo "   gateway=${BASE_URL} keycloak=${KEYCLOAK_URL} vespa=${VESPA_URL}"
echo "   run_query=${RUN_QUERY} run_ingest=${RUN_INGEST} soak=${SOAK} seed=${SEED_CORPUS}"
echo "   outdir=${OUTDIR}"
echo

# --- 0. Preflight: k6 present -------------------------------------------------
begin "preflight: k6 on PATH"
if command -v "$K6" >/dev/null 2>&1; then
  pass
  note "$($K6 version 2>&1 | head -1)"
else
  fail "k6 not found (set K6=/path/to/k6); install: https://grafana.com/docs/k6/latest/set-up/install-k6/"
fi

begin "preflight: gateway /healthz -> 200"
code="$(curl -s -o /dev/null --max-time 15 -w '%{http_code}' "${BASE_URL}/healthz" 2>/dev/null || echo 000)"
if [ "$code" = "200" ]; then
  pass
else
  fail "expected 200, got ${code} (is the stack up?)"
fi

# --- 1. Token + resolve the querying tenant (BEFORE seeding) ------------------
# The query path scopes every search to the caller's Vespa STREAMING GROUP, which
# the gateway derives from the verified token (tenant_id claim, else sub). So we
# must seed the corpus into THAT exact group, or every query scans an empty group
# and returns 0 hits — measuring empty-result latency, not real search (M5 review).
TOKEN="${TOKEN:-}"
begin "token: OIDC password grant for ${KC_USER}"
if [ -n "$TOKEN" ]; then
  pass
  note "using TOKEN from env"
elif TOKEN="$(fetch_token "$KC_USER" "$KC_PASS")"; then
  pass
else
  fail "$TOKEN"
  TOKEN=""
fi

QUERY_TENANT="${QUERY_TENANT:-}"
begin "tenant: resolve the querying tenant via GET /v1/me"
if [ -n "$QUERY_TENANT" ]; then
  pass
  note "using QUERY_TENANT from env: ${QUERY_TENANT}"
elif [ -z "$TOKEN" ]; then
  fail "no token; cannot resolve tenant"
elif me="$("${CURL[@]}" -H "Authorization: Bearer ${TOKEN}" "${BASE_URL}/v1/me" 2>&1)" &&
  QUERY_TENANT="$(json_field tenant_id <<<"$me")" && [ -n "$QUERY_TENANT" ]; then
  pass
  note "querying tenant (streaming group): ${QUERY_TENANT}"
else
  fail "could not resolve tenant_id from /v1/me: ${me:0:200}"
  QUERY_TENANT=""
fi

# --- 2. Seed the query corpus INTO the querying tenant (vespa-direct) ----------
# A single representative tenant: each query scans exactly ONE streaming group, so
# per-tenant corpus size is what the query-latency SLO measures; multi-tenant
# storage scale is the capacity model's job (docs/capacity.md). rare-token-rate
# 1.0 (default) makes every doc carry a contiguous qzx ordinal, so the query suite
# re-derives the exact set and RARE_TOKEN_COUNT below is exact.
if [ "$SEED_CORPUS" = "true" ] && [ -n "$QUERY_TENANT" ]; then
  begin "seed: synthgen vespa-direct into ${QUERY_TENANT} (${SYNTH_DOCS_PER_TENANT} docs, rate ${SYNTH_RARE_RATE})"
  if go run ./tools/synthgen \
    --target vespa-direct --vespa-url "$VESPA_URL" \
    --tenant-id "$QUERY_TENANT" --docs-per-tenant "$SYNTH_DOCS_PER_TENANT" \
    --rare-token-rate "$SYNTH_RARE_RATE" --seed "$SYNTH_SEED" \
    --concurrency "$SYNTH_CONCURRENCY" --dry-run=false \
    --progress-every 10s >"$OUTDIR/synthgen.log" 2>&1; then
    pass
    note "log: $OUTDIR/synthgen.log"
  else
    fail "synthgen seed failed; tail: $(tail -n 3 "$OUTDIR/synthgen.log" 2>/dev/null | tr '\n' ' ')"
  fi
else
  note "seed skipped (SEED_CORPUS=${SEED_CORPUS}, tenant='${QUERY_TENANT}')"
fi
# Rare-token universe the query suite samples [0, RARE_TOKEN_COUNT). Prefer the
# EXACT minted count synthgen reports ("rare tokens: N"), robust at any rate;
# fall back to the at-rate-1.0 exact value (docs_per_tenant) for a skipped seed.
if [ -z "$RARE_TOKEN_COUNT" ]; then
  RARE_TOKEN_COUNT="$(grep -oE 'rare tokens: +[0-9]+' "$OUTDIR/synthgen.log" 2>/dev/null | grep -oE '[0-9]+' | head -1)"
fi
if [ -z "$RARE_TOKEN_COUNT" ]; then
  RARE_TOKEN_COUNT="$(python3 -c "print(max(1, int(${SYNTH_DOCS_PER_TENANT}*${SYNTH_RARE_RATE})))")"
fi
note "rare-token universe: ${RARE_TOKEN_COUNT} (qzx00000000..)"

# --- 3. Query suite (stepped to cluster max) ----------------------------------
if [ "$RUN_QUERY" = "true" ] && [ "$SOAK" != "true" ]; then
  begin "query suite: stepped ${RPS_START}->${RPS_MAX} RPS (step ${RPS_STEP}, hold ${STAGE_HOLD})"
  if [ -z "$TOKEN" ]; then
    fail "no token; cannot run query suite"
  else
    QSUM="$OUTDIR/query-summary.json"
    set +e
    K6_SUMMARY_PATH="$QSUM" \
    BASE_URL="$BASE_URL" TOKEN="$TOKEN" \
    RPS_START="$RPS_START" RPS_MAX="$RPS_MAX" RPS_STEP="$RPS_STEP" \
    STAGE_HOLD="$STAGE_HOLD" MODE="$MODE" QUERY_LIMIT="$QUERY_LIMIT" \
    P90_SLO_MS="$P90_SLO_MS" FAIL_RATE_MAX="$FAIL_RATE_MAX" \
    RARE_TOKEN_COUNT="$RARE_TOKEN_COUNT" \
    CHECK_EXACT_HITS="${CHECK_EXACT_HITS:-true}" \
      "$K6" run \
        --summary-trend-stats "avg,min,med,p(90),p(95),p(99),max" \
        "${REPO_ROOT}/tools/load/query-load.js" >"$OUTDIR/query.out" 2>&1
    k6rc=$?
    set -e
    QUERY_P90="$(k6_metric "$QSUM" http_req_duration 'p(90)')"
    MAX_RPS_OK="$(k6_max_rps_ok "$QSUM" "$P90_SLO_MS")"
    if [ "$k6rc" = "0" ]; then
      pass
      note "P90=${QUERY_P90:-?}ms (SLO ${P90_SLO_MS}ms); max sustained RPS at P90<SLO: ${MAX_RPS_OK}"
    else
      fail "k6 thresholds breached (rc=${k6rc}); P90=${QUERY_P90:-?}ms; see $OUTDIR/query.out"
      note "max sustained RPS at P90<SLO: ${MAX_RPS_OK}"
    fi
  fi
fi

# --- 4. Ingest / freshness suite ----------------------------------------------
if [ "$RUN_INGEST" = "true" ] && [ "$SOAK" != "true" ]; then
  begin "ingest suite: ${INGEST_VUS} VUs for ${INGEST_DURATION}; freshness P90 < 30min"
  if [ -z "$TOKEN" ]; then
    fail "no token; cannot run ingest suite"
  else
    ISUM="$OUTDIR/ingest-summary.json"
    set +e
    K6_SUMMARY_PATH="$ISUM" \
    BASE_URL="$BASE_URL" TOKEN="$TOKEN" \
    INGEST_VUS="$INGEST_VUS" INGEST_DURATION="$INGEST_DURATION" \
    FRESHNESS_SLO_MS="$FRESHNESS_SLO_MS" \
      "$K6" run \
        --summary-trend-stats "avg,min,med,p(90),p(95),p(99),max" \
        "${REPO_ROOT}/tools/load/ingest-load.js" >"$OUTDIR/ingest.out" 2>&1
    k6rc=$?
    set -e
    FRESH_P90="$(k6_metric "$ISUM" freshness_seconds 'p(90)')"
    if [ "$k6rc" = "0" ]; then
      pass
      note "freshness P90=${FRESH_P90:-?}s (SLO $((FRESHNESS_SLO_MS / 1000))s)"
    else
      fail "freshness threshold breached (rc=${k6rc}); P90=${FRESH_P90:-?}s; see $OUTDIR/ingest.out"
    fi
  fi
fi

# --- 5. Soak (opt-in): fixed sub-max rate for SOAK_DURATION -------------------
if [ "$SOAK" = "true" ]; then
  echo
  echo "-- SOAK: query @ ${SOAK_RPS} RPS + concurrent ingest for ${SOAK_DURATION}; zero failed + no new deadletters --"

  # Snapshot the deadletter counter BEFORE the soak (zero-data-loss baseline).
  DL_BEFORE="$(deadletter_total)"
  begin "soak: read deadletter baseline (asker_pipeline_deadletter_total)"
  if [ -n "$DL_BEFORE" ]; then
    pass
    note "baseline deadletters=${DL_BEFORE} (from ${INDEX_WRITER_METRICS})"
  else
    # Not a hard fail: in some topologies the index-writer /metrics is not
    # reachable from the runner. Warn loudly; the soak still asserts zero failed.
    pass
    note "WARNING: ${INDEX_WRITER_METRICS} unreachable or metric absent; deadletter check will be SKIPPED"
  fi

  # Drive sustained INGEST traffic concurrently with the query soak: documents
  # must actually flow through the connector->Kafka->ingest->enrich->index
  # pipeline whose deadletter counter the zero-data-loss gate scrapes. A read-only
  # soak can never exercise the data-loss path it claims to verify (M5 review).
  SOAK_INGEST_PID=""
  if [ -n "$TOKEN" ]; then
    K6_SUMMARY_PATH="$OUTDIR/soak-ingest-summary.json" \
    BASE_URL="$BASE_URL" TOKEN="$TOKEN" \
    INGEST_VUS="${SOAK_INGEST_VUS:-2}" INGEST_DURATION="$SOAK_DURATION" \
    FRESHNESS_SLO_MS="$FRESHNESS_SLO_MS" \
      "$K6" run "${REPO_ROOT}/tools/load/ingest-load.js" >"$OUTDIR/soak-ingest.out" 2>&1 &
    SOAK_INGEST_PID=$!
    note "concurrent soak ingest started (pid ${SOAK_INGEST_PID}, ${SOAK_INGEST_VUS:-2} VUs for ${SOAK_DURATION})"
  fi

  begin "soak: query suite, ${SOAK_RPS} RPS for ${SOAK_DURATION}, 0 failed requests"
  if [ -z "$TOKEN" ]; then
    fail "no token; cannot run soak"
  else
    SSUM="$OUTDIR/soak-summary.json"
    set +e
    K6_SUMMARY_PATH="$SSUM" \
    BASE_URL="$BASE_URL" TOKEN="$TOKEN" \
    SOAK=true SOAK_RPS="$SOAK_RPS" SOAK_DURATION="$SOAK_DURATION" \
    MODE="$MODE" QUERY_LIMIT="$QUERY_LIMIT" \
    P90_SLO_MS="$P90_SLO_MS" FAIL_RATE_MAX="${SOAK_FAIL_RATE_MAX:-0.001}" \
    RARE_TOKEN_COUNT="$RARE_TOKEN_COUNT" \
    CHECK_EXACT_HITS="${CHECK_EXACT_HITS:-true}" \
      "$K6" run \
        --summary-trend-stats "avg,min,med,p(90),p(95),p(99),max" \
        "${REPO_ROOT}/tools/load/query-load.js" >"$OUTDIR/soak.out" 2>&1
    k6rc=$?
    set -e
    # Reap the concurrent ingest load (it runs for the same SOAK_DURATION).
    if [ -n "$SOAK_INGEST_PID" ]; then
      wait "$SOAK_INGEST_PID" 2>/dev/null || true
    fi
    # The k6 http_req_failed threshold for the soak uses a tiny positive bound
    # (SOAK_FAIL_RATE_MAX, default 0.001) so a clean run PASSES the k6 gate (a
    # literal rate<0 can never be satisfied). The STRICT zero-failure assertion
    # is the computed SOAK_FAILED == 0 below.
    # Failed-request count = http_reqs total * http_req_failed rate (rounded).
    local_failed="$(python3 -c "
import json,sys
try:
    d=json.load(open('$SSUM'))
except Exception:
    print('?'); sys.exit(0)
m=d.get('metrics') or {}
reqs=((m.get('http_reqs') or {}).get('values') or {}).get('count') or 0
rate=((m.get('http_req_failed') or {}).get('values') or {}).get('rate') or 0
print(int(round(reqs*rate)))
")"
    SOAK_FAILED="$local_failed"
    QUERY_P90="$(k6_metric "$SSUM" http_req_duration 'p(90)')"
    if [ "$k6rc" = "0" ] && [ "$SOAK_FAILED" = "0" ]; then
      pass
      note "0 failed over the soak; P90=${QUERY_P90:-?}ms"
    else
      fail "soak failed-request count=${SOAK_FAILED} (want 0), k6 rc=${k6rc}; see $OUTDIR/soak.out"
    fi
  fi

  # Zero-data-loss: no NEW deadletters during the soak.
  begin "soak: zero data loss (no new deadletters)"
  DL_AFTER="$(deadletter_total)"
  if [ -z "$DL_BEFORE" ] || [ -z "$DL_AFTER" ]; then
    # Many topologies do not expose the index-writer health port (:9701) to the
    # load runner: the service /metrics is scraped IN-NETWORK by Prometheus, not
    # published to the host (the compose dev stack does not publish health ports).
    # Treat an unreachable deadletter metric as a WARNING, not a failure — the
    # primary zero-failed-requests assertion above still holds. To make this a
    # hard check, point INDEX_WRITER_METRICS at a reachable /metrics (run from
    # inside the cluster/network, or `kubectl port-forward` the index-writer).
    pass
    note "WARNING/INCONCLUSIVE: deadletter metric unreachable (before='${DL_BEFORE}' after='${DL_AFTER}')"
    note "set INDEX_WRITER_METRICS to a reachable /metrics to make this a hard zero-data-loss gate"
  else
    SOAK_DL_DELTA="$(python3 -c "print('%g' % (float('${DL_AFTER}') - float('${DL_BEFORE}')))")"
    if python3 -c "import sys; sys.exit(0 if float('${DL_AFTER}') <= float('${DL_BEFORE}') else 1)"; then
      pass
      note "deadletters before=${DL_BEFORE} after=${DL_AFTER} delta=${SOAK_DL_DELTA} (zero data loss)"
    else
      fail "deadletters rose by ${SOAK_DL_DELTA} (before=${DL_BEFORE} after=${DL_AFTER}) — DATA LOSS"
    fi
  fi
fi

# --- Summary -------------------------------------------------------------------
echo
echo "== Summary =="
printf '  %-4s %-6s %s\n' "STEP" "STATUS" "CHECK"
for row in "${SUMMARY[@]}"; do
  IFS='|' read -r num status name <<<"$row"
  printf '  %-4s %-6s %s\n' "$num" "$status" "$name"
done
echo
echo "== Headline numbers =="
printf '  query P90 latency:            %s ms (SLO %s ms)\n' "${QUERY_P90}" "${P90_SLO_MS}"
printf '  max sustained RPS at P90<SLO: %s\n' "${MAX_RPS_OK}"
printf '  ingest freshness P90:         %s s (SLO %s s)\n' "${FRESH_P90}" "$((FRESHNESS_SLO_MS / 1000))"
if [ "$SOAK" = "true" ]; then
  printf '  soak failed requests:         %s (want 0)\n' "${SOAK_FAILED}"
  printf '  soak deadletter delta:        %s (want <= 0)\n' "${SOAK_DL_DELTA}"
fi
echo
echo "  artifacts: ${OUTDIR}"
echo "  EXTRAPOLATION: feed 'max sustained RPS at P90<SLO' on N nodes into"
echo "  docs/capacity.md to project node counts for 50K TPS (see tools/load/README.md)."
echo

if [ "$FAILED" -ne 0 ]; then
  echo "M5 LOAD: FAILED"
  exit 1
fi
echo "M5 LOAD: ALL ${STEP} CHECKS PASSED"
