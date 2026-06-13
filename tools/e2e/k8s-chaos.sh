#!/usr/bin/env bash
# Asker M4 kind chaos / query-path resilience test (the M4 exit criterion).
#
# THE M4 ACCEPTANCE TEST (specs/asker-v1-personal-search-engine.md, M4 exit):
#   "full stack deploys to a kind/k3d cluster in CI; chaos test (kill any one
#    pod) shows no failed queries beyond retry."
#
# Runs against a kind/k3d cluster that already has the asker chart installed with
# the SLIM CI profile (deploy/helm/asker/values-ci.yaml): gateway (2 replicas +
# PDB), query (2 replicas + PDB), Vespa (1 group/node, streaming), TEI, Redis,
# Keycloak. It:
#
#   1. Waits for the Vespa StatefulSet to be rollout-Ready (its readiness gates on
#      the config port :19071, which comes up WITHOUT the application package).
#   2. Deploys the Vespa application package to the in-cluster config server by
#      running the committed vespa/deploy.sh flow against a kubectl port-forward
#      (EMBEDDING_DIM=384 to match the CI TEI model + the chart's Vespa schema).
#      This activates the query port :8080.
#   3. ONLY NOW waits for the query + gateway Deployments to be rollout-Ready —
#      their /readyz live-pings Vespa :8080, so they cannot be Ready until step 2.
#      (This ordering is why the CI `helm install` does NOT use --wait.)
#   4. Feeds ONE tenant-scoped doc carrying a rare token via the Vespa
#      document/v1 API (exactly like tools/e2e/smoke.sh), scoped to a streaming
#      group g=<tenant>.
#   5. Mints an OIDC token from Keycloak (dev password grant, user alice).
#   6. BASELINE: queries the rare token through the gateway /v1/search -> 1 hit.
#   7. CHAOS: in a background loop, fires CHAOS_REQUESTS gateway /v1/search calls
#      continuously WITH a bounded client retry (CLIENT_RETRIES on 5xx / connect
#      error) while `kubectl delete pod` kills one query pod, then one gateway
#      pod (re-establishing the gateway port-forward after, since it pins to one
#      pod). Asserts EVERY query ULTIMATELY succeeds within its retries (no failed
#      queries beyond retry) AND the killed pods are rescheduled Ready.
#
# Prints a numbered PASS/FAIL summary; exits non-zero on ANY failed-beyond-retry
# query (or any failed check). House style mirrors tools/e2e/smoke.sh.
#
# DEPENDENCIES (CI-only; there is NO local kind in the dev sandbox):
#   - kubectl  (talking to the kind cluster; KUBECONFIG must be set by the job)
#   - kind     (only to have created the cluster; not invoked here)
#   - bash, curl, python3, zip  (zip is needed by vespa/deploy.sh)
#   The asker chart must already be `helm install`ed with values-ci.yaml and the
#   gateway/query images `kind load`ed (see .github/workflows/k8s.yml).
#
# All knobs are env-overridable (defaults suit the CI kind cluster).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

# --- Config (env-overridable) -------------------------------------------------
NAMESPACE="${NAMESPACE:-asker}"
RELEASE="${RELEASE:-asker}"            # helm release name -> object name prefix
KUBECTL="${KUBECTL:-kubectl}"

# Workload object names (release-prefixed Deployments; bare Service names).
GATEWAY_DEPLOY="${GATEWAY_DEPLOY:-${RELEASE}-gateway}"
QUERY_DEPLOY="${QUERY_DEPLOY:-${RELEASE}-query}"
VESPA_STS="${VESPA_STS:-${RELEASE}-vespa}"
GATEWAY_SVC="${GATEWAY_SVC:-gateway}"  # bare Service (compose DNS)
KEYCLOAK_SVC="${KEYCLOAK_SVC:-keycloak}"
VESPA_SVC="${VESPA_SVC:-vespa}"

# Local ports for the kubectl port-forwards (host side).
GATEWAY_LPORT="${GATEWAY_LPORT:-18080}"
KEYCLOAK_LPORT="${KEYCLOAK_LPORT:-18081}"
VESPA_QUERY_LPORT="${VESPA_QUERY_LPORT:-18082}"
VESPA_CFG_LPORT="${VESPA_CFG_LPORT:-19071}"

ROLLOUT_TIMEOUT="${ROLLOUT_TIMEOUT:-600}"     # seconds to wait for each rollout
VESPA_DEPLOY_TIMEOUT="${VESPA_DEPLOY_TIMEOUT:-300}" # vespa/deploy.sh per-step
EMBEDDING_DIM="${EMBEDDING_DIM:-384}"         # MUST match the CI TEI model
CLIP_DIM="${CLIP_DIM:-512}"

# OIDC (dev realm; users alice/bob, password123).
KC_USER="${KC_USER:-alice}"
KC_PASS="${KC_PASS:-password123}"

# Chaos parameters.
CHAOS_REQUESTS="${CHAOS_REQUESTS:-60}"        # gateway queries fired during chaos
CLIENT_RETRIES="${CLIENT_RETRIES:-3}"         # bounded retries per query
RETRY_BACKOFF="${RETRY_BACKOFF:-2}"           # seconds between retries
REQUEST_INTERVAL="${REQUEST_INTERVAL:-0.3}"   # seconds between query launches

# Tenant-scoped probe doc (rare token; like smoke.sh).
CHAOS_TENANT="${CHAOS_TENANT:-chaos-tenant}"
CHAOS_DOC_ID="${CHAOS_DOC_ID:-chaos-1}"
CHAOS_TOKEN="${CHAOS_TOKEN:-plughxyzzy}"

# --- Report scaffolding (smoke.sh house style) --------------------------------
STEP=0
FAILED=0
CURRENT_NAME=""
declare -a SUMMARY=()

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

CURL=(curl -fsS --max-time 30)

# json_field FIELD: read JSON on stdin, print the string value of FIELD ("" if
# absent). Exits non-zero (silently) on unparseable input.
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

# hits_total: read a /v1/search JSON on stdin, print "<len(hits)> <total>".
hits_total() {
  python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    print("? ?")
    sys.exit(1)
hits = d.get("hits") or []
print(len(hits), d.get("total", 0))
'
}

# --- Port-forward management --------------------------------------------------
declare -a PF_PIDS=()

# port_forward TARGET LOCAL REMOTE: background `kubectl port-forward`, record PID.
port_forward() {
  local target="$1" lport="$2" rport="$3"
  "$KUBECTL" -n "$NAMESPACE" port-forward "$target" "${lport}:${rport}" \
    >/dev/null 2>&1 &
  PF_PIDS+=("$!")
}

# (Re)establish the GATEWAY port-forward. kubectl port-forward to a Service pins
# to ONE backing pod and ends when that pod terminates; the chaos arm kills a
# gateway pod, which can be exactly the pinned one. If we did not re-establish it,
# the TEST's own single ingress — not the cluster (2 replicas + PDB keep serving)
# — would be the single point of failure, spuriously FAILing a healthy stack. We
# `wait` for the old forward to fully exit (freeing the local port) before
# rebinding; the in-flight queries' bounded retries cover the brief blip.
GATEWAY_PF_PID=""
start_gateway_pf() {
  if [ -n "$GATEWAY_PF_PID" ]; then
    kill "$GATEWAY_PF_PID" >/dev/null 2>&1 || true
    wait "$GATEWAY_PF_PID" 2>/dev/null || true
  fi
  "$KUBECTL" -n "$NAMESPACE" port-forward "svc/${GATEWAY_SVC}" \
    "${GATEWAY_LPORT}:8080" >/dev/null 2>&1 &
  GATEWAY_PF_PID=$!
  PF_PIDS+=("$GATEWAY_PF_PID")
}

# wait_local_http URL ATTEMPTS: poll a local URL until it answers (any code).
wait_local_http() {
  local url="$1" attempts="${2:-30}" i
  for i in $(seq 1 "$attempts"); do
    if curl -s -o /dev/null --max-time 5 "$url"; then
      return 0
    fi
    sleep 1
  done
  return 1
}

cleanup() {
  set +e
  # Best-effort: delete the probe doc, then kill all port-forwards.
  curl -fsS --max-time 15 -X DELETE \
    "http://localhost:${VESPA_QUERY_LPORT}/document/v1/asker/doc/group/${CHAOS_TENANT}/${CHAOS_DOC_ID}" \
    >/dev/null 2>&1
  local pid
  for pid in "${PF_PIDS[@]:-}"; do
    [ -n "$pid" ] && kill "$pid" >/dev/null 2>&1
  done
  rm -f "$TMP_REQLOG" 2>/dev/null
}
trap cleanup EXIT

TMP_REQLOG="$(mktemp "${TMPDIR:-/tmp}/k8s-chaos.XXXXXX")"

echo "== Asker M4 kind chaos test =="
echo "   namespace=${NAMESPACE} release=${RELEASE}"
echo "   gateway=${GATEWAY_DEPLOY} query=${QUERY_DEPLOY} vespa=${VESPA_STS}"
echo "   chaos_requests=${CHAOS_REQUESTS} client_retries=${CLIENT_RETRIES} embedding_dim=${EMBEDDING_DIM}"
echo

# --- 1. Wait for Vespa StatefulSet Ready --------------------------------------
# ORDERING (critical): the query readinessProbe (/readyz) live-pings Vespa
# :8080, which only serves AFTER the application package is activated. So we must
# activate the package BEFORE gating on query/gateway readiness — otherwise query
# can never become Ready and a `helm install --wait` (or a query rollout wait)
# would deadlock. Vespa's OWN readiness gates on the config port :19071, which
# comes up WITHOUT the package, so we can wait for the StatefulSet here, then
# deploy the package (step 2), then wait for query+gateway (step 3).

begin "rollout: Vespa StatefulSet Ready (config :19071, pre-package)"
if "$KUBECTL" -n "$NAMESPACE" rollout status "statefulset/${VESPA_STS}" \
  --timeout "${ROLLOUT_TIMEOUT}s" >/dev/null 2>&1; then
  pass
else
  fail "vespa rollout not ready within ${ROLLOUT_TIMEOUT}s"
fi

# --- 2. Port-forward Vespa (config + query) and deploy the app package --------

begin "port-forward: Vespa config(${VESPA_CFG_LPORT}) + query(${VESPA_QUERY_LPORT})"
port_forward "svc/${VESPA_SVC}" "$VESPA_CFG_LPORT" 19071
port_forward "svc/${VESPA_SVC}" "$VESPA_QUERY_LPORT" 8080
if wait_local_http "http://localhost:${VESPA_CFG_LPORT}/state/v1/health" 30; then
  pass
else
  fail "Vespa config server port-forward not answering on :${VESPA_CFG_LPORT}"
fi

begin "vespa: deploy application package (EMBEDDING_DIM=${EMBEDDING_DIM})"
# Reuse the committed, tested deploy flow against the in-cluster config server
# via the port-forward. The single-node vespa/app (node1, no hosts.xml) matches
# the CI profile's 1-group/1-node Vespa StatefulSet.
if VESPA_CFG_URL="http://localhost:${VESPA_CFG_LPORT}" \
  VESPA_QUERY_URL="http://localhost:${VESPA_QUERY_LPORT}" \
  WAIT_TIMEOUT_SECS="$VESPA_DEPLOY_TIMEOUT" \
  EMBEDDING_DIM="$EMBEDDING_DIM" CLIP_DIM="$CLIP_DIM" \
  bash "${REPO_ROOT}/vespa/deploy.sh" >/dev/null 2>&1; then
  pass
else
  fail "vespa/deploy.sh failed against :${VESPA_CFG_LPORT}"
fi

# --- 3. NOW wait for query + gateway (their /readyz needs the live Vespa :8080) -

begin "rollout: query Deployment Ready (post-package)"
if "$KUBECTL" -n "$NAMESPACE" rollout status "deployment/${QUERY_DEPLOY}" \
  --timeout "${ROLLOUT_TIMEOUT}s" >/dev/null 2>&1; then
  pass
else
  fail "query rollout not ready within ${ROLLOUT_TIMEOUT}s (Vespa :8080 / app package?)"
fi

begin "rollout: gateway Deployment Ready (post-package)"
if "$KUBECTL" -n "$NAMESPACE" rollout status "deployment/${GATEWAY_DEPLOY}" \
  --timeout "${ROLLOUT_TIMEOUT}s" >/dev/null 2>&1; then
  pass
else
  fail "gateway rollout not ready within ${ROLLOUT_TIMEOUT}s"
fi

# --- 4. Feed the tenant-scoped probe doc --------------------------------------

FED=0
CHAOS_DOC_URL="http://localhost:${VESPA_QUERY_LPORT}/document/v1/asker/doc/group/${CHAOS_TENANT}/${CHAOS_DOC_ID}"
begin "vespa: feed probe doc for ${CHAOS_TENANT} (rare token ${CHAOS_TOKEN})"
if out="$("${CURL[@]}" -X POST "$CHAOS_DOC_URL" \
  -H 'Content-Type: application/json' \
  -d "{
    \"fields\": {
      \"doc_id\": \"${CHAOS_DOC_ID}\",
      \"connector_id\": \"chaos\",
      \"type\": \"FILE\",
      \"title\": \"asker chaos probe ${CHAOS_TOKEN}\",
      \"body\": \"this body contains the rare token ${CHAOS_TOKEN} for chaos query-path testing\",
      \"created_at\": 1718000000
    }
  }" 2>&1)"; then
  FED=1
  pass
else
  fail "${out:0:300}"
fi

# --- 5. Mint an OIDC token via Keycloak ---------------------------------------

begin "port-forward: Keycloak(${KEYCLOAK_LPORT})"
port_forward "svc/${KEYCLOAK_SVC}" "$KEYCLOAK_LPORT" 8080
if wait_local_http "http://localhost:${KEYCLOAK_LPORT}/realms/asker/.well-known/openid-configuration" 60; then
  pass
else
  fail "Keycloak port-forward not answering on :${KEYCLOAK_LPORT}"
fi

TOKEN=""
begin "keycloak: password grant for ${KC_USER}"
# KC_HOSTNAME on the in-cluster Keycloak pins the token issuer to
# http://keycloak:8080/realms/asker (== the gateway's OIDC_ISSUER), so the
# gateway accepts this token even though we minted it over the local forward.
if body="$("${CURL[@]}" -X POST \
  "http://localhost:${KEYCLOAK_LPORT}/realms/asker/protocol/openid-connect/token" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode 'client_id=asker-web' \
  --data-urlencode 'grant_type=password' \
  --data-urlencode "username=${KC_USER}" \
  --data-urlencode "password=${KC_PASS}" 2>&1)" &&
  TOKEN="$(json_field access_token <<<"$body")" && [ -n "$TOKEN" ]; then
  pass
else
  fail "no access_token: ${body:0:200}"
  TOKEN=""
fi

# --- 6. Port-forward the gateway + baseline query -----------------------------

begin "port-forward: gateway(${GATEWAY_LPORT})"
start_gateway_pf
if wait_local_http "http://localhost:${GATEWAY_LPORT}/healthz" 30; then
  pass
else
  fail "gateway port-forward not answering on :${GATEWAY_LPORT}"
fi

GATEWAY_BASE="http://localhost:${GATEWAY_LPORT}"

# search_once: GET /v1/search for the rare token. Prints HTTP code to stdout;
# writes the body to $TMP_REQLOG. No retry (the retry wrapper handles that).
search_once() {
  curl -s -o "$TMP_REQLOG" -w '%{http_code}' --max-time 20 -G \
    -H "Authorization: Bearer ${TOKEN}" \
    "${GATEWAY_BASE}/v1/search" \
    --data-urlencode "q=${CHAOS_TOKEN}" \
    --data-urlencode "limit=10" 2>/dev/null || echo "000"
}

# search_with_retry: fire one query with a bounded retry on 5xx/connection error
# (the "within retries" budget). Prints "OK <code>" on eventual 2xx, else
# "FAIL <lastcode>". 4xx is a hard failure (no retry: not a transient pod kill).
search_with_retry() {
  local attempt code
  for attempt in $(seq 0 "$CLIENT_RETRIES"); do
    code="$(search_once)"
    case "$code" in
      2*) echo "OK ${code}"; return 0 ;;
      000 | 5*) sleep "$RETRY_BACKOFF" ;;  # transient: connection reset / 5xx
      *) echo "FAIL ${code}"; return 1 ;;  # 4xx etc.: not transient
    esac
  done
  echo "FAIL ${code}"
  return 1
}

begin "baseline: gateway /v1/search '${CHAOS_TOKEN}' -> exactly 1 hit"
if [ "$FED" != "1" ] || [ -z "$TOKEN" ]; then
  fail "skipped: feed=${FED} token_present=$([ -n "$TOKEN" ] && echo yes || echo no)"
else
  # Poll briefly: streaming search is immediately consistent, but the query
  # service cache / pod readiness may lag a beat after rollout.
  base_ok=0
  base_diag="?"
  for _ in $(seq 1 15); do
    code="$(search_once)"
    if [ "$code" = "200" ]; then
      read -r nhits ntotal <<<"$(hits_total <"$TMP_REQLOG")"
      base_diag="hits=${nhits} total=${ntotal} code=${code}"
      if [ "$nhits" = "1" ]; then
        base_ok=1
        break
      fi
    else
      base_diag="code=${code} body=$(head -c 160 "$TMP_REQLOG" 2>/dev/null)"
    fi
    sleep 2
  done
  if [ "$base_ok" = "1" ]; then
    pass
    printf '     -> %s\n' "$base_diag"
  else
    fail "$base_diag"
  fi
fi

# --- 7. CHAOS: kill pods while a query loop runs ------------------------------

# The chaos loop runs in a background subshell, writing one result line per
# query ("OK <code>" / "FAIL <code>") to a results file. Meanwhile the main
# shell kills one query pod, then one gateway pod.
CHAOS_RESULTS="$(mktemp "${TMPDIR:-/tmp}/k8s-chaos-res.XXXXXX")"

run_chaos_loop() {
  local i res
  for i in $(seq 1 "$CHAOS_REQUESTS"); do
    res="$(search_with_retry)"
    echo "$res" >>"$CHAOS_RESULTS"
    sleep "$REQUEST_INTERVAL"
  done
}

# kill_one_pod LABEL_DEPLOY DESC: delete the first running pod of a Deployment
# and report whether a kill was issued.
kill_one_pod() {
  local deploy="$1" pod
  pod="$("$KUBECTL" -n "$NAMESPACE" get pods \
    -l "app.kubernetes.io/name=asker" \
    -o jsonpath="{range .items[*]}{.metadata.name}{'\n'}{end}" 2>/dev/null \
    | grep -E "^${deploy}-" | head -n1)"
  if [ -z "$pod" ]; then
    # Fallback: select by the Deployment's pod-template hash via get pods on the
    # ReplicaSet selector is overkill; match on name prefix is sufficient here.
    pod="$("$KUBECTL" -n "$NAMESPACE" get pods -o name 2>/dev/null \
      | sed 's#^pod/##' | grep -E "^${deploy}-" | head -n1)"
  fi
  if [ -n "$pod" ]; then
    "$KUBECTL" -n "$NAMESPACE" delete pod "$pod" --wait=false >/dev/null 2>&1
    echo "$pod"
  fi
}

CHAOS_RAN=0
if [ "$FED" = "1" ] && [ -n "$TOKEN" ]; then
  CHAOS_RAN=1
  echo
  echo "-- chaos: firing ${CHAOS_REQUESTS} queries (retry x${CLIENT_RETRIES}) while killing 1 query + 1 gateway pod --"
  : >"$CHAOS_RESULTS"
  run_chaos_loop &
  LOOP_PID=$!

  # Give the loop a moment to start, then kill one query pod.
  sleep 2
  KILLED_QUERY="$(kill_one_pod "$QUERY_DEPLOY")"
  echo "   killed query pod: ${KILLED_QUERY:-<none found>}"
  sleep 4
  # Then kill one gateway pod.
  KILLED_GATEWAY="$(kill_one_pod "$GATEWAY_DEPLOY")"
  echo "   killed gateway pod: ${KILLED_GATEWAY:-<none found>}"
  # The local port-forward may have been pinned to the just-killed pod; rebind it
  # to a surviving replica so the TEST's own ingress is not the SPOF (see
  # start_gateway_pf). In-flight queries retry across this blip.
  start_gateway_pf
  wait_local_http "http://localhost:${GATEWAY_LPORT}/healthz" 30 || true

  # Wait for the query loop to finish.
  wait "$LOOP_PID" 2>/dev/null || true
fi

begin "chaos: every query ultimately succeeded within ${CLIENT_RETRIES} retries"
if [ "$CHAOS_RAN" != "1" ]; then
  fail "skipped: baseline preconditions not met"
else
  total_q="$(wc -l <"$CHAOS_RESULTS" | tr -d ' ')"
  ok_q="$(grep -c '^OK' "$CHAOS_RESULTS" || true)"
  bad_q="$(grep -c '^FAIL' "$CHAOS_RESULTS" || true)"
  if [ "$total_q" = "$CHAOS_REQUESTS" ] && [ "$bad_q" = "0" ] && [ "$ok_q" = "$CHAOS_REQUESTS" ]; then
    pass
    printf '     -> %s/%s queries OK, 0 failed beyond retry\n' "$ok_q" "$CHAOS_REQUESTS"
  else
    fail "fired=${total_q} ok=${ok_q} failed_beyond_retry=${bad_q} (want ${CHAOS_REQUESTS}/0)"
    echo "     failed query codes:"
    grep '^FAIL' "$CHAOS_RESULTS" 2>/dev/null | sort | uniq -c | sed 's/^/       /' || true
  fi
fi
rm -f "$CHAOS_RESULTS" 2>/dev/null

begin "chaos: query Deployment rescheduled back to Ready"
if [ "$CHAOS_RAN" != "1" ]; then
  fail "skipped"
elif "$KUBECTL" -n "$NAMESPACE" rollout status "deployment/${QUERY_DEPLOY}" \
  --timeout "${ROLLOUT_TIMEOUT}s" >/dev/null 2>&1; then
  pass
else
  fail "query Deployment not Ready again within ${ROLLOUT_TIMEOUT}s"
fi

begin "chaos: gateway Deployment rescheduled back to Ready"
if [ "$CHAOS_RAN" != "1" ]; then
  fail "skipped"
elif "$KUBECTL" -n "$NAMESPACE" rollout status "deployment/${GATEWAY_DEPLOY}" \
  --timeout "${ROLLOUT_TIMEOUT}s" >/dev/null 2>&1; then
  pass
else
  fail "gateway Deployment not Ready again within ${ROLLOUT_TIMEOUT}s"
fi

begin "post-chaos: gateway /v1/search '${CHAOS_TOKEN}' -> 1 hit again"
if [ "$CHAOS_RAN" != "1" ]; then
  fail "skipped"
else
  post_ok=0
  post_diag="?"
  for _ in $(seq 1 15); do
    code="$(search_once)"
    if [ "$code" = "200" ]; then
      read -r nhits ntotal <<<"$(hits_total <"$TMP_REQLOG")"
      post_diag="hits=${nhits} total=${ntotal}"
      if [ "$nhits" = "1" ]; then
        post_ok=1
        break
      fi
    else
      post_diag="code=${code}"
    fi
    sleep 2
  done
  if [ "$post_ok" = "1" ]; then
    pass
    printf '     -> %s\n' "$post_diag"
  else
    fail "$post_diag"
  fi
fi

# --- Summary ------------------------------------------------------------------

echo
echo "== Summary =="
printf '  %-4s %-6s %s\n' "STEP" "STATUS" "CHECK"
for row in "${SUMMARY[@]}"; do
  IFS='|' read -r num status name <<<"$row"
  printf '  %-4s %-6s %s\n' "$num" "$status" "$name"
done
echo

if [ "$FAILED" -ne 0 ]; then
  echo "K8S CHAOS: FAILED"
  exit 1
fi
echo "K8S CHAOS: ALL ${STEP} CHECKS PASSED (no failed queries beyond retry)"
