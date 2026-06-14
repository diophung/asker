#!/usr/bin/env bash
# Asker connector OAuth end-to-end test (wave 1).
#
# THE OAuth acceptance test: it drives the WHOLE authorization-code + PKCE flow
# for the gmail (Google) connector against the dev FAKE OAuth provider, with no
# real provider credentials, and asserts the connector ends up CONNECTED — i.e.
# the gateway exchanged the code, stored an Asker OAuth token in the same
# encrypted control-plane vault used for manual tokens, and the hub then synced
# real documents that become searchable.
#
# What it exercises (the cross-builder OAuth contract):
#   1. GET /v1/connectors/{id}/oauth/start  (AUTHED) -> {"authorize_url":...}
#   2. following authorize_url (the fake auto-consents) redirects the browser to
#      GET /v1/oauth/callback?code=&state=  (PUBLIC, no bearer), which exchanges
#      the code and PUTs oauth.Marshal(Token) via control-plane, then 302s to
#      <WEB_APP_URL>/connectors?oauth=connected .
#   3. the hub reads that stored token, refreshes-if-needed, and hands the
#      ACCESS TOKEN ("fake-gmail-token:<email>") to the gmail connector, whose
#      call to the dev fake-gmail succeeds -> a seeded email becomes searchable.
#
# Security properties asserted (must hold or the suite FAILS):
#   - the callback is UNAUTHENTICATED yet the token lands under alice's tenant:
#     tenant + connector come from the SERVER-SIDE state /oauth/start created,
#     never from the callback request.
#   - the random `state` is SINGLE-USE: replaying the same callback is rejected.
#   - the post-callback redirect target is the FIXED configured WEB_APP_URL
#     (no open redirect), and never carries a code/token.
#   - tokens are never echoed by the gateway: the start/callback responses leak
#     no access/refresh token (defense-in-depth check on the wire).
#
# Self-contained and idempotent: every marker (mailbox, instance, tokens)
# embeds RUN_ID, so reruns against a shared stack are safe; the connector
# instance this run creates is deleted on exit. It never restarts compose.
#
# NETWORK NOTE: the redirect chain is fake-oauth -> gateway callback -> web.
# The gateway's authorize_url points at the fake provider's PUBLIC base
# (FAKE_OAUTH_URL, reachable from this host shell), and the provider redirects
# the browser back to the gateway's PUBLIC callback (GATEWAY_URL). Because BOTH
# legs are host-reachable in the dev compose (all bound to 127.0.0.1), this
# script runs from the host with a plain `curl -L`. The connector->provider
# token-endpoint call and the connector->fake-gmail call happen INSIDE the
# compose network using the in-network names (fake-oauth:9500, fake-gmail:9400)
# the gateway/hub are configured with — those names need not resolve from this
# shell. If you run this script from inside the compose network instead, set
# GATEWAY_URL/FAKE_OAUTH_URL to the in-network names.
#
# Run from anywhere. Requires: bash, curl, python3. No jq.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
KEYCLOAK_URL="${KEYCLOAK_URL:-http://localhost:8081}"
# The fake OAuth provider's authorize/token base, as reachable from THIS shell.
# (The gateway is separately configured with the IN-NETWORK base via
# ASKER_OAUTH_GOOGLE_AUTH_URL/_TOKEN_URL=http://fake-oauth:9500/... .)
FAKE_OAUTH_URL="${FAKE_OAUTH_URL:-http://localhost:9500}"
# fake-gmail admin API as reachable from THIS shell (host port mapping).
FAKE_GMAIL_URL="${FAKE_GMAIL_URL:-http://localhost:9400}"
# Where the web UI lives; the gateway callback 302s here (WEB_APP_URL). Used to
# assert the redirect target is fixed and carries no secret.
WEB_APP_URL="${WEB_APP_URL:-http://localhost:13001}"

USER_NAME="${OAUTH_E2E_USER:-alice}"
USER_PASS="${OAUTH_E2E_PASS:-password123}"
# The login_hint / mailbox the fake provider mints a token for, and that the
# gmail connector is configured to read. The Google fake issues an access token
# "fake-gmail-token:<email>" so the connector's fake-gmail call succeeds.
USER_EMAIL="${OAUTH_E2E_EMAIL:-alice@example.com}"
# fake-gmail as reachable from INSIDE the compose network — the connector
# instance's base_url (ADR-008): connectors run in-network, not on the host.
FAKE_GMAIL_INTERNAL_URL="${FAKE_GMAIL_INTERNAL_URL:-http://fake-gmail:9400}"

# Number of synthetic emails to seed so the post-OAuth sync has something to
# index, and how long to wait for a seeded token to become searchable.
OAUTH_E2E_SEED_COUNT="${OAUTH_E2E_SEED_COUNT:-25}"
OAUTH_E2E_SYNC_TIMEOUT="${OAUTH_E2E_SYNC_TIMEOUT:-300}"

RUN_ID="$(date +%s)"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/oauth-e2e.XXXXXX")"

# Run-unique markers so reruns / concurrent suites never collide.
PROBE_TOKEN="oauthe2e${RUN_ID}"
INSTANCE=""
TOKEN=""        # alice OIDC access token
TENANT=""       # alice tenant_id from /v1/me

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

http_code() {
  curl -s -o /dev/null --max-time 30 -w '%{http_code}' "$@"
}

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

# fetch_token USER PASSWORD: print an access token via the dev password grant.
fetch_token() {
  local body tok
  body="$("${CURL[@]}" -X POST "${KEYCLOAK_URL}/realms/asker/protocol/openid-connect/token" \
    -H 'Content-Type: application/x-www-form-urlencoded' \
    --data-urlencode "client_id=asker-web" \
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

# search_count TOKEN QUERY: GET /v1/search as TOKEN for QUERY; print the hit
# count ("?" on failure). Retries on 429 (the per-tenant limit is shared).
search_count() {
  local token="$1" query="$2" code attempt
  for attempt in 1 2 3 4 5; do
    code="$(curl -s -o "$TMP/search.json" -w '%{http_code}' --max-time 30 -G \
      -H "Authorization: Bearer ${token}" "${GATEWAY_URL}/v1/search" \
      --data-urlencode "q=${query}" --data-urlencode "limit=20")" || code="000"
    if [ "$code" = "200" ]; then
      python3 -c '
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print("?"); sys.exit(0)
print(len(d.get("hits") or []))
' "$TMP/search.json"
      return 0
    fi
    [ "$code" = "429" ] && { sleep 3; continue; }
    break
  done
  echo "?"
  return 0
}

# sync_state INSTANCE_ID: print "<phase> <docsEmitted>" for INSTANCE_ID from a
# GET /v1/connectors response on stdin. protojson renders int64 as a string.
read_sync() {
  python3 -c '
import json, sys
want = sys.argv[1]
for e in json.load(sys.stdin):
    if (e.get("instance") or {}).get("id") == want:
        sy = e.get("sync") or {}
        print(sy.get("phase", "NONE"), int(sy.get("docsEmitted") or 0))
        break
else:
    print("MISSING 0")
' "$1"
}

# assert_no_secret LABEL FILE: fail the current step's context if FILE contains
# anything that looks like an access/refresh token (defense-in-depth: the
# gateway must never echo tokens on the wire). Prints a diagnostic, returns
# non-zero on a leak.
assert_no_secret() {
  local label="$1" file="$2"
  if grep -Eiq 'access_token|refresh_token|fake-gmail-token' "$file" 2>/dev/null; then
    echo "${label}: response appears to contain a token (leak!)"
    return 1
  fi
  return 0
}

cleanup() {
  set +e
  # Best-effort: delete this run's connector instance.
  if [ -n "$INSTANCE" ]; then
    local t
    t="$(fetch_token "$USER_NAME" "$USER_PASS" 2>/dev/null)" &&
      curl -s --max-time 30 -X DELETE -H "Authorization: Bearer ${t}" \
        "${GATEWAY_URL}/v1/connectors/${INSTANCE}" >/dev/null 2>&1
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT

echo "== Asker connector OAuth e2e (fake provider) =="
echo "   gateway=${GATEWAY_URL} keycloak=${KEYCLOAK_URL}"
echo "   fake-oauth=${FAKE_OAUTH_URL} fake-gmail=${FAKE_GMAIL_URL} web=${WEB_APP_URL}"
echo "   run_id=${RUN_ID} user=${USER_NAME} email=${USER_EMAIL} seed=${OAUTH_E2E_SEED_COUNT}"
echo

# --- 1. Preflight --------------------------------------------------------------

begin "gateway: GET /healthz -> 200"
if code="$(http_code "${GATEWAY_URL}/healthz")" && [ "$code" = "200" ]; then
  pass
else
  fail "expected 200, got ${code} (is the dev stack up?)"
fi

begin "fake-oauth: GET /healthz -> 200"
if code="$(http_code "${FAKE_OAUTH_URL}/healthz")" && [ "$code" = "200" ]; then
  pass
else
  fail "expected 200, got ${code} (is the fake-oauth service running? integrator wires it)"
fi

begin "fake-gmail: GET /healthz -> 200"
if code="$(http_code "${FAKE_GMAIL_URL}/healthz")" && [ "$code" = "200" ]; then
  pass
else
  fail "expected 200, got ${code} (is the dev stack up?)"
fi

begin "keycloak/gateway: token for ${USER_NAME}; tenant_id from /v1/me"
if ! TOKEN="$(fetch_token "$USER_NAME" "$USER_PASS")"; then
  fail "$TOKEN"
  TOKEN=""
elif body="$("${CURL[@]}" -H "Authorization: Bearer ${TOKEN}" "${GATEWAY_URL}/v1/me" 2>&1)" &&
  TENANT="$(json_field tenant_id <<<"$body")" && [ -n "$TENANT" ]; then
  pass
  printf '     -> tenant_id=%s\n' "$TENANT"
else
  fail "/v1/me failed: ${body:0:160}"
fi

begin "oauth/start without a bearer -> 401 (the start endpoint is authed)"
# Use a throwaway connector path: any /v1/connectors/.../oauth/start requires a
# bearer regardless of the id (authn precedes lookup).
if code="$(http_code "${GATEWAY_URL}/v1/connectors/none-${RUN_ID}/oauth/start")" &&
  [ "$code" = "401" ]; then
  pass
else
  fail "expected 401 without bearer, got ${code}"
fi

# --- 2. Seed the source + create the gmail connector (NO token yet) ------------

begin "fake-gmail: seed ${OAUTH_E2E_SEED_COUNT} emails + probe message for ${USER_EMAIL}"
seed_ok=0
diag=""
if body="$("${CURL[@]}" -X POST "${FAKE_GMAIL_URL}/admin/users/${USER_EMAIL}/seed" \
  -H 'Content-Type: application/json' \
  -d "{\"count\":${OAUTH_E2E_SEED_COUNT},\"seed\":${RUN_ID}}" 2>&1)" &&
  seeded="$(json_field seeded <<<"$body")" && [ "$seeded" = "$OAUTH_E2E_SEED_COUNT" ]; then
  # A run-unique probe message so the post-OAuth sync has a marker to find.
  if pbody="$("${CURL[@]}" -X POST "${FAKE_GMAIL_URL}/admin/users/${USER_EMAIL}/messages" \
    -H 'Content-Type: application/json' \
    -d "{\"subject\":\"OAuth e2e probe ${PROBE_TOKEN}\",\"body\":\"Connected via OAuth. Tracking reference ${PROBE_TOKEN}.\",\"from\":\"oauth.probe@example.com\",\"to\":\"${USER_EMAIL}\"}" 2>&1)" &&
    pid="$(json_field id <<<"$pbody")" && [ -n "$pid" ]; then
    seed_ok=1
  else
    diag="probe inject failed: ${pbody:0:200}"
  fi
else
  diag="seed failed: ${body:0:200}"
fi
if [ "$seed_ok" = "1" ]; then
  pass
  printf '     -> probe token=%s\n' "$PROBE_TOKEN"
else
  fail "$diag"
fi

begin "gateway: create gmail connector (config user_email + in-network base_url)"
conn_ok=0
diag=""
if [ -z "$TOKEN" ]; then
  fail "skipped: no ${USER_NAME} token"
else
  create_payload="$(printf '{"connector_id":"gmail","display_name":"oauth-e2e %s %s","config":{"base_url":"%s","user_email":"%s"}}' \
    "$USER_NAME" "$RUN_ID" "$FAKE_GMAIL_INTERNAL_URL" "$USER_EMAIL")"
  if body="$("${CURL[@]}" -X POST "${GATEWAY_URL}/v1/connectors" \
    -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
    -d "$create_payload" 2>&1)" &&
    INSTANCE="$(json_field id <<<"$body")" && [ -n "$INSTANCE" ]; then
    conn_ok=1
  else
    diag="create connector failed: ${body:0:200}"
  fi
  if [ "$conn_ok" = "1" ]; then
    pass
    printf '     -> instance=%s (no token yet)\n' "$INSTANCE"
  else
    fail "$diag"
  fi
fi

# --- 3. OAuth start: get the provider authorize URL ----------------------------

AUTHORIZE_URL=""
begin "GET /v1/connectors/{id}/oauth/start (authed) -> 200 {authorize_url}"
start_ok=0
diag=""
if [ -z "$INSTANCE" ]; then
  fail "skipped: no connector instance"
elif code="$(curl -s -o "$TMP/start.json" -w '%{http_code}' --max-time 30 \
  -H "Authorization: Bearer ${TOKEN}" \
  "${GATEWAY_URL}/v1/connectors/${INSTANCE}/oauth/start")" && [ "$code" = "200" ]; then
  AUTHORIZE_URL="$(json_field authorize_url <"$TMP/start.json")"
  if [ -n "$AUTHORIZE_URL" ] && assert_no_secret "oauth/start" "$TMP/start.json"; then
    # Sanity: it must carry the PKCE challenge + state + response_type=code.
    if grep -q 'code_challenge=' <<<"$AUTHORIZE_URL" &&
      grep -q 'code_challenge_method=S256' <<<"$AUTHORIZE_URL" &&
      grep -q 'state=' <<<"$AUTHORIZE_URL" &&
      grep -q 'response_type=code' <<<"$AUTHORIZE_URL"; then
      start_ok=1
    else
      diag="authorize_url missing PKCE/state/response_type: ${AUTHORIZE_URL:0:200}"
    fi
  else
    diag="no authorize_url, or response leaked a token: $(head -c 160 "$TMP/start.json")"
  fi
else
  diag="oauth/start -> ${code}: $(head -c 200 "$TMP/start.json")"
fi
if [ "$start_ok" = "1" ]; then
  pass
  printf '     -> authorize_url host: %s\n' "$(python3 -c 'import sys,urllib.parse as u; print(u.urlparse(sys.argv[1]).netloc)' "$AUTHORIZE_URL")"
else
  fail "$diag"
fi

# Extract the server-side `state` so we can later prove it is single-use.
STATE=""
if [ -n "$AUTHORIZE_URL" ]; then
  STATE="$(python3 -c '
import sys, urllib.parse as u
q = u.parse_qs(u.urlparse(sys.argv[1]).query)
print((q.get("state") or [""])[0])
' "$AUTHORIZE_URL")"
fi

# --- 4. Follow the authorize URL: the fake consents -> gateway callback -> web -

begin "follow authorize_url (-L): fake consents, gateway callback 302s to WEB_APP_URL?oauth=connected"
follow_ok=0
diag=""
if [ -z "$AUTHORIZE_URL" ]; then
  fail "skipped: no authorize_url"
else
  # Pass a login_hint so the fake mints a token for our mailbox even if the
  # authorize_url did not already carry one. -L follows the full redirect chain
  # (fake-oauth -> /v1/oauth/callback -> WEB_APP_URL). We capture the FINAL URL
  # and the full header trace to inspect the callback's Location.
  hinted_url="$AUTHORIZE_URL"
  case "$hinted_url" in
    *login_hint=*) ;;
    *\?*) hinted_url="${hinted_url}&login_hint=${USER_EMAIL}" ;;
    *) hinted_url="${hinted_url}?login_hint=${USER_EMAIL}" ;;
  esac
  final_url="$(curl -s -o "$TMP/follow_body.txt" -D "$TMP/follow_hdrs.txt" \
    -L --max-redirs 10 --max-time 60 -w '%{url_effective}' "$hinted_url" 2>>"$TMP/follow_err.txt" || true)"
  # The final landing URL must be the FIXED WEB_APP_URL with oauth=connected,
  # and must NOT carry a code or token (no open redirect, no secret leak).
  if [ -n "$final_url" ] &&
    grep -q "^${WEB_APP_URL}/connectors" <<<"$final_url" &&
    grep -q 'oauth=connected' <<<"$final_url"; then
    if grep -Eiq 'code=|access_token|refresh_token|fake-gmail-token' <<<"$final_url"; then
      diag="final redirect leaked a code/token: ${final_url}"
    elif assert_no_secret "callback Location headers" "$TMP/follow_hdrs.txt"; then
      follow_ok=1
    else
      diag="a redirect header leaked a token"
    fi
  else
    diag="final url not WEB_APP_URL/connectors?oauth=connected: '${final_url}' (errors: $(head -c 160 "$TMP/follow_err.txt" 2>/dev/null))"
  fi
  if [ "$follow_ok" = "1" ]; then
    pass
    printf '     -> landed at: %s\n' "$final_url"
  else
    fail "$diag"
  fi
fi

begin "callback is single-use: replaying the same state -> redirect to oauth=error"
# Re-driving the callback with the SAME state must fail closed (the state was
# deleted on first use). We cannot reconstruct the provider code, but the state
# alone is enough: a missing/used state must redirect to oauth=error and MUST
# NOT 302 to oauth=connected.
if [ -z "$STATE" ]; then
  fail "skipped: could not extract state from authorize_url"
else
  loc="$(curl -s -o /dev/null -D - --max-time 30 \
    "${GATEWAY_URL}/v1/oauth/callback?code=replayed-${RUN_ID}&state=${STATE}" 2>/dev/null |
    tr -d '\r' | awk 'tolower($1)=="location:"{print $2}' | tail -n1)"
  if grep -q 'oauth=error' <<<"$loc" && ! grep -q 'oauth=connected' <<<"$loc"; then
    pass
    printf '     -> replay redirected to: %s\n' "$loc"
  else
    fail "replayed callback did not fail closed; Location='${loc}'"
  fi
fi

# --- 5. The token is stored + the hub syncs under alice's tenant ---------------

echo
echo "-- waiting for the post-OAuth sync to emit docs (timeout ${OAUTH_E2E_SYNC_TIMEOUT}s) --"
sync_ok=0
sync_phase="NONE"
sync_docs=0
sync_start=$(date +%s)
iter=0
while :; do
  if body="$(curl -fsS --max-time 30 -H "Authorization: Bearer ${TOKEN}" \
    "${GATEWAY_URL}/v1/connectors" 2>/dev/null)"; then
    read -r sync_phase sync_docs <<<"$(read_sync "$INSTANCE" <<<"$body")"
  else
    sync_phase="HTTP-ERR" sync_docs=0
  fi
  # "Connected" proof: the hub got a usable access token from the stored OAuth
  # blob and synced — phase reached a terminal/active non-error state AND at
  # least one doc was emitted. (A missing/garbage token would leave docs=0 and
  # a FAILED/auth phase.)
  if [ "$sync_docs" -ge 1 ]; then
    sync_ok=1
    break
  fi
  elapsed=$(($(date +%s) - sync_start))
  if [ "$elapsed" -ge "$OAUTH_E2E_SYNC_TIMEOUT" ]; then
    break
  fi
  if [ $((iter % 6)) -eq 0 ]; then
    echo "   sync progress (${elapsed}s): phase=${sync_phase} docs=${sync_docs}"
  fi
  iter=$((iter + 1))
  if [ $((iter % 30)) -eq 0 ]; then
    TOKEN="$(fetch_token "$USER_NAME" "$USER_PASS" || echo "$TOKEN")" # tokens live 300s
  fi
  sleep 5
done

begin "connector CONNECTED: post-OAuth sync emitted >= 1 doc (hub used the stored token)"
if [ "$sync_ok" = "1" ]; then
  pass
  printf '     -> phase=%s docsEmitted=%s\n' "$sync_phase" "$sync_docs"
else
  fail "no docs emitted within ${OAUTH_E2E_SYNC_TIMEOUT}s (phase=${sync_phase} docs=${sync_docs}); the OAuth token did not reach the connector"
fi

begin "search: probe token '${PROBE_TOKEN}' searchable as ${USER_NAME} (end-to-end)"
search_ok=0
last="?"
search_start=$(date +%s)
TOKEN="$(fetch_token "$USER_NAME" "$USER_PASS" || echo "$TOKEN")"
while :; do
  last="$(search_count "$TOKEN" "$PROBE_TOKEN")"
  if [ "$last" != "?" ] && [ "$last" -ge 1 ]; then
    search_ok=1
    break
  fi
  elapsed=$(($(date +%s) - search_start))
  if [ "$elapsed" -ge "$OAUTH_E2E_SYNC_TIMEOUT" ]; then
    break
  fi
  sleep 5
done
if [ "$search_ok" = "1" ]; then
  pass
  printf '     -> probe searchable (hits=%s)\n' "$last"
else
  fail "probe '${PROBE_TOKEN}' not searchable within ${OAUTH_E2E_SYNC_TIMEOUT}s (last hits=${last})"
fi

# --- 6. Summary ----------------------------------------------------------------

echo
echo "== Summary =="
printf '  %-4s %-6s %s\n' "STEP" "STATUS" "CHECK"
for row in "${SUMMARY[@]}"; do
  IFS='|' read -r num status name <<<"$row"
  printf '  %-4s %-6s %s\n' "$num" "$status" "$name"
done
echo

if [ "$FAILED" -ne 0 ]; then
  echo "OAUTH E2E: FAILED (run_id=${RUN_ID})"
  exit 1
fi
echo "OAUTH E2E: ALL ${STEP} CHECKS PASSED (run_id=${RUN_ID})"
