#!/usr/bin/env bash
# Asker M1 cross-tenant leakage suite — THE SACRED SUITE.
#
# It attempts every public API with mismatched tenant tokens and must prove
# isolation (spec §M1 exit criteria). Adversarial surface covered:
#
#   1. Search isolation     — alice's rare tokens are invisible to bob/carol in
#                             keyword, hybrid and vector modes, with and
#                             without type/participant filters.
#   2. Resource-ID attacks  — bob probing alice's connector instance id gets
#                             404 (never 200/403: no existence oracle), and
#                             his attempts leave alice's instance untouched.
#   3. Header/param spoofing— x-asker-tenant / X-Tenant-Id headers and
#                             tenant_id/tenant/groupname/streaming.groupname
#                             query params are ignored: results stay scoped to
#                             the VERIFIED token (ADR-002, ADR-009).
#   4. Token abuse          — garbage, expired-looking, and bit-flipped JWTs
#                             are 401; every /v1/* route is 401 without a token.
#   5. Internal exposure    — control-plane/query/connector-hub ports are not
#                             reachable from the host; the dev-exposed Vespa
#                             port fails closed without a streaming group.
#   6. Cross-pollination    — bob uploading a file CONTAINING alice's rare
#                             tokens never surfaces in alice's results.
#
# Self-contained and idempotent: every marker (mailbox, tokens, titles) embeds
# RUN_ID, so reruns against a stack that already ran this suite are safe, and
# foreign documents from concurrent suites are tolerated — asserts target THIS
# run's markers and doc_ids, never global totals.
#
# Assertion model (why not always "0 hits"): hybrid/vector retrieval includes
# a nearestNeighbor arm that legitimately returns the caller's OWN documents
# for any query text, and reruns leave same-tenant documents behind. So:
#   - keyword-mode searches for run-unique tokens assert STRICT ZERO hits;
#   - other modes assert ZERO LEAKED hits: no hit may carry one of alice's
#     doc_ids for this run, and no hit may contain a run-scoped marker
#     (the mailbox address or the leakzz<RUN_ID> token prefix). Foreign,
#     provably-not-alice hits are tolerated and reported.
# A real leak fails the suite either way.
#
# Run from anywhere. Requires: bash, curl, python3, docker compose. No jq.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
KEYCLOAK_URL="${KEYCLOAK_URL:-http://localhost:8081}"
VESPA_URL="${VESPA_URL:-http://localhost:8082}"
# fake-gmail admin API as reachable from the HOST (dev-only, ADR-008).
FAKE_GMAIL_ADMIN_URL="${FAKE_GMAIL_ADMIN_URL:-http://localhost:9400}"
# fake-gmail base_url as reachable from INSIDE the compose network — this is
# what the connector instance must be configured with.
FAKE_GMAIL_INTERNAL_URL="${FAKE_GMAIL_INTERNAL_URL:-http://fake-gmail:9400}"
COMPOSE_FILE="${COMPOSE_FILE:-deploy/compose/docker-compose.yml}"
KC_ADMIN_USER="${KC_ADMIN_USER:-admin}"
KC_ADMIN_PASSWORD="${KC_ADMIN_PASSWORD:-admin}"
USER_PASSWORD="${USER_PASSWORD:-password123}"
# Generous end-to-end wait (connector sync interval 30s + pipeline).
LEAK_WAIT_SECS="${LEAK_WAIT_SECS:-300}"
POLL_INTERVAL=5

COMPOSE=(docker compose -f "$COMPOSE_FILE")
CURL=(curl -fsS --max-time 30)

# --- Run-scoped markers (idempotency: everything embeds RUN_ID) --------------
RUN_ID="${RUN_ID:-$(date +%s)}"
MAILBOX="alice-leak+${RUN_ID}@example.com"
MAILBOX_MARKER="alice-leak+${RUN_ID}"     # appears in from/to metadata of seeded mail
TOKEN_PREFIX="leakzz${RUN_ID}"            # prefix of all run-unique rare tokens
TOKEN_MAIL="${TOKEN_PREFIX}mail"          # custom probe email body token
TOKEN_UPLOAD="${TOKEN_PREFIX}upload"      # alice's upload body token
TOKEN_POST="${TOKEN_PREFIX}post"          # post-attack sync-proof token
# Seeded deterministic rare tokens: tools/fake-gmail/server/seed.go RareToken(i)
# = "qzx%05d" for message index i. We seed 5 messages (leakage spec step 1;
# the wave-level E2E_EMAIL_COUNT knob belongs to the ingest-scale suite).
SEED_COUNT=5
SEED_TOKENS=(qzx00000 qzx00001 qzx00002 qzx00003 qzx00004)
# Markers whose presence in ANY cross-tenant hit is a leak, regardless of doc_id.
LEAK_MARKERS="${MAILBOX_MARKER},${TOKEN_PREFIX}"

STEP=0
FAILED=0
CURRENT_NAME=""
declare -a SUMMARY=()

ALICE_TOKEN="" BOB_TOKEN="" CAROL_TOKEN=""
ALICE_TENANT="" BOB_TENANT="" CAROL_TENANT=""
ALICE_IID=""        # alice's connector instance id
ALICE_UP_DOC=""     # alice's upload doc_id
BOB_UP_DOC=""       # bob's cross-pollination upload doc_id
ALICE_DOC_IDS=""    # comma-joined doc_ids of alice's run-scoped documents

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

note() {
  printf '     -> %s\n' "$*"
}

# json_field FIELD: print the string value of FIELD from JSON on stdin.
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

http_code() {
  curl -s -o /dev/null --max-time 30 -w '%{http_code}' "$@"
}

# fetch_token USER: print an access token via the dev password grant.
fetch_token() {
  local body tok
  body="$("${CURL[@]}" -X POST "${KEYCLOAK_URL}/realms/asker/protocol/openid-connect/token" \
    -H 'Content-Type: application/x-www-form-urlencoded' \
    --data-urlencode "client_id=asker-web" \
    --data-urlencode "grant_type=password" \
    --data-urlencode "username=$1" \
    --data-urlencode "password=${USER_PASSWORD}" 2>&1)" || {
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

# admin_token: Keycloak master-realm admin token (dev creds, admin console).
admin_token() {
  local body tok
  body="$("${CURL[@]}" -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
    -H 'Content-Type: application/x-www-form-urlencoded' \
    --data-urlencode "client_id=admin-cli" \
    --data-urlencode "grant_type=password" \
    --data-urlencode "username=${KC_ADMIN_USER}" \
    --data-urlencode "password=${KC_ADMIN_PASSWORD}" 2>&1)" || {
    echo "admin token request failed: ${body:0:200}"
    return 1
  }
  tok="$(json_field access_token <<<"$body" || true)"
  [ -n "$tok" ] || { echo "no admin access_token: ${body:0:200}"; return 1; }
  printf '%s\n' "$tok"
}

# refresh_tokens: re-fetch the three user tokens (the dev realm's access-token
# lifespan is 300s; the suite's polling phases can outlive one token).
refresh_tokens() {
  ALICE_TOKEN="$(fetch_token alice)" || return 1
  BOB_TOKEN="$(fetch_token bob)" || return 1
  CAROL_TOKEN="$(fetch_token carol)" || return 1
}

# search_raw TOKEN [curl args...]: GET /v1/search as TOKEN; prints the body.
# Callers pass --data-urlencode pairs (q=, mode=, limit=, ...) and extra -H.
search_raw() {
  local token="$1"
  shift
  "${CURL[@]}" -G "${GATEWAY_URL}/v1/search" \
    -H "Authorization: Bearer ${token}" "$@"
}

# hit_count: print len(hits) from a /v1/search response on stdin.
hit_count() {
  python3 -c '
import json, sys
try:
    data = json.load(sys.stdin)
except Exception:
    sys.exit(1)
print(len(data.get("hits") or []))
'
}

# hit_doc_ids: print the doc_id of every hit, one per line.
hit_doc_ids() {
  python3 -c '
import json, sys
try:
    data = json.load(sys.stdin)
except Exception:
    sys.exit(1)
for h in data.get("hits") or []:
    d = h.get("doc_id")
    if d:
        print(d)
'
}

# scan_hits MARKERS FORBIDDEN_IDS: leak detector. Reads a /v1/search response
# on stdin. A hit leaks if its doc_id is in FORBIDDEN_IDS (comma list) or if
# its full JSON serialization contains any MARKER (comma list) — that catches
# leaked content even under a rewritten doc_id. Prints "hits=N leaks=K" then
# one "LEAK: ..." detail line per leak.
scan_hits() {
  python3 -c '
import json, sys
markers = [m for m in sys.argv[1].split(",") if m]
forbidden = set(i for i in sys.argv[2].split(",") if i)
try:
    data = json.load(sys.stdin)
except Exception:
    print("hits=-1 leaks=-1")
    sys.exit(1)
hits = data.get("hits") or []
leaks = []
for h in hits:
    did = h.get("doc_id", "")
    if did in forbidden:
        leaks.append("forbidden doc_id %s (connector=%s)" % (did, h.get("connector_id")))
        continue
    blob = json.dumps(h)
    for m in markers:
        if m in blob:
            leaks.append("marker %r leaked in hit doc_id=%s" % (m, did))
            break
print("hits=%d leaks=%d" % (len(hits), len(leaks)))
for l in leaks:
    print("LEAK: " + l)
' "$1" "$2"
}

# assert_no_leak NAME TOKEN STRICT [curl args...]: run one search as TOKEN and
# apply the leak detector. STRICT=1 additionally requires zero hits total
# (only valid for keyword-mode searches on run-unique tokens).
assert_no_leak() {
  local name="$1" token="$2" strict="$3"
  shift 3
  begin "$name"
  local body scan head hits leaks
  if ! body="$(search_raw "$token" "$@" 2>&1)"; then
    fail "search failed: ${body:0:200}"
    return
  fi
  scan="$(scan_hits "$LEAK_MARKERS" "$ALICE_DOC_IDS" <<<"$body")" || {
    fail "unparseable search response: ${body:0:200}"
    return
  }
  head="$(head -n1 <<<"$scan")"
  hits="${head#hits=}"; hits="${hits%% *}"
  leaks="${head##*leaks=}"
  if [ "$leaks" != "0" ]; then
    fail "CROSS-TENANT LEAK: $(tail -n +2 <<<"$scan" | tr '\n' '; ')"
    return
  fi
  if [ "$strict" = "1" ] && [ "$hits" != "0" ]; then
    fail "expected strict 0 hits, got ${hits}: ${body:0:300}"
    return
  fi
  pass
  if [ "$hits" != "0" ]; then
    note "tolerated ${hits} foreign hit(s): none carries alice's doc_ids or run markers"
  fi
}

# ensure_carol: idempotently ensure the carol user exists in the RUNNING realm
# (she is in the realm IMPORT file but the live realm predates her). POST
# tolerates 409; the follow-up PUT repairs partially-created accounts
# (firstName/lastName/requiredActions — without them the password grant fails
# with "Account is not fully set up") and reset-password pins the password.
ensure_carol() {
  local atok code cid
  atok="$(admin_token)" || { echo "$atok"; return 1; }
  code="$(http_code -X POST "${KEYCLOAK_URL}/admin/realms/asker/users" \
    -H "Authorization: Bearer ${atok}" -H 'Content-Type: application/json' \
    -d '{"username":"carol","enabled":true,"emailVerified":true,
         "email":"carol@example.com","firstName":"Carol","lastName":"Asker",
         "credentials":[{"type":"password","value":"'"${USER_PASSWORD}"'","temporary":false}]}')"
  case "$code" in
    201 | 409) ;;
    *) echo "create carol: unexpected status $code"; return 1 ;;
  esac
  cid="$("${CURL[@]}" -H "Authorization: Bearer ${atok}" \
    "${KEYCLOAK_URL}/admin/realms/asker/users?username=carol&exact=true" |
    python3 -c 'import json,sys; u=json.load(sys.stdin); print(u[0]["id"] if u else "")')" || true
  [ -n "$cid" ] || { echo "carol not found after ensure"; return 1; }
  code="$(http_code -X PUT "${KEYCLOAK_URL}/admin/realms/asker/users/${cid}" \
    -H "Authorization: Bearer ${atok}" -H 'Content-Type: application/json' \
    -d '{"enabled":true,"emailVerified":true,"email":"carol@example.com",
         "firstName":"Carol","lastName":"Asker","requiredActions":[]}')"
  [ "$code" = "204" ] || { echo "repair carol profile: status $code"; return 1; }
  code="$(http_code -X PUT "${KEYCLOAK_URL}/admin/realms/asker/users/${cid}/reset-password" \
    -H "Authorization: Bearer ${atok}" -H 'Content-Type: application/json' \
    -d '{"type":"password","value":"'"${USER_PASSWORD}"'","temporary":false}')"
  [ "$code" = "204" ] || { echo "reset carol password: status $code"; return 1; }
  echo "carol ensured (user id ${cid})"
}

# wait_for_alice_keyword TOKEN SECS: poll until alice's keyword search for
# TOKEN returns >=1 hit. Progress on one line; returns 1 on timeout.
wait_for_alice_keyword() {
  local want="$1" budget="$2" waited=0 n
  while [ "$waited" -le "$budget" ]; do
    n="$(search_raw "$ALICE_TOKEN" \
      --data-urlencode "q=${want}" --data-urlencode "mode=keyword" \
      --data-urlencode "limit=50" 2>/dev/null | hit_count 2>/dev/null || echo 0)"
    if [ "${n:-0}" -ge 1 ] 2>/dev/null; then
      return 0
    fi
    sleep "$POLL_INTERVAL"
    waited=$((waited + POLL_INTERVAL))
    if [ $((waited % 30)) -eq 0 ]; then
      printf '(t+%ss) ' "$waited"
      # The dev realm token lifespan is 300s; keep alice's fresh mid-wait.
      ALICE_TOKEN="$(fetch_token alice)" || true
    fi
  done
  return 1
}

TMP_ALICE_FILE="$(mktemp /tmp/asker-leak-alice.XXXXXX)"
TMP_BOB_FILE="$(mktemp /tmp/asker-leak-bob.XXXXXX)"

cleanup() {
  rm -f "$TMP_ALICE_FILE" "$TMP_BOB_FILE"
  # Best-effort: delete the connector instance this run created (idempotent;
  # a fresh token because the setup-time one may have expired).
  if [ -n "$ALICE_IID" ]; then
    local_tok="$(fetch_token alice 2>/dev/null || true)"
    if [ -n "$local_tok" ]; then
      curl -s -o /dev/null --max-time 30 -X DELETE \
        -H "Authorization: Bearer ${local_tok}" \
        "${GATEWAY_URL}/v1/connectors/${ALICE_IID}" || true
    fi
  fi
}
trap cleanup EXIT

echo "== Asker M1 cross-tenant leakage suite (sacred) =="
echo "   gateway=${GATEWAY_URL} keycloak=${KEYCLOAK_URL} vespa=${VESPA_URL}"
echo "   fake-gmail admin (host)=${FAKE_GMAIL_ADMIN_URL} (compose)=${FAKE_GMAIL_INTERNAL_URL}"
echo "   RUN_ID=${RUN_ID} mailbox=${MAILBOX}"
echo "   seeded rare tokens: ${SEED_TOKENS[*]} (RareToken(i)=qzx%05d, seed.go)"
echo "   run-unique tokens : ${TOKEN_MAIL} ${TOKEN_UPLOAD} ${TOKEN_POST}"
echo

# === Phase 0: preflight =======================================================

begin "gateway: GET /healthz -> 200"
if code="$(http_code "${GATEWAY_URL}/healthz")" && [ "$code" = "200" ]; then
  pass
else
  fail "expected 200, got $code"
fi

begin "fake-gmail: GET /healthz -> 200 (host, dev-only admin surface)"
if code="$(http_code "${FAKE_GMAIL_ADMIN_URL}/healthz")" && [ "$code" = "200" ]; then
  pass
else
  fail "expected 200, got $code"
fi

# === Phase 1: setup ===========================================================

begin "keycloak: ensure user carol exists (idempotent, admin REST API)"
if out="$(ensure_carol)"; then
  pass
  note "$out"
else
  fail "$out"
fi

begin "keycloak/gateway: alice+bob+carol tokens; three distinct tenant ids"
tok_err=""
if ! ALICE_TOKEN="$(fetch_token alice)"; then tok_err="alice: ${ALICE_TOKEN}"; fi
if [ -z "$tok_err" ] && ! BOB_TOKEN="$(fetch_token bob)"; then tok_err="bob: ${BOB_TOKEN}"; fi
if [ -z "$tok_err" ] && ! CAROL_TOKEN="$(fetch_token carol)"; then tok_err="carol: ${CAROL_TOKEN}"; fi
if [ -n "$tok_err" ]; then
  fail "$tok_err"
else
  ALICE_TENANT="$("${CURL[@]}" -H "Authorization: Bearer ${ALICE_TOKEN}" "${GATEWAY_URL}/v1/me" | json_field tenant_id)" || ALICE_TENANT=""
  BOB_TENANT="$("${CURL[@]}" -H "Authorization: Bearer ${BOB_TOKEN}" "${GATEWAY_URL}/v1/me" | json_field tenant_id)" || BOB_TENANT=""
  CAROL_TENANT="$("${CURL[@]}" -H "Authorization: Bearer ${CAROL_TOKEN}" "${GATEWAY_URL}/v1/me" | json_field tenant_id)" || CAROL_TENANT=""
  if [ -n "$ALICE_TENANT" ] && [ -n "$BOB_TENANT" ] && [ -n "$CAROL_TENANT" ] &&
    [ "$ALICE_TENANT" != "$BOB_TENANT" ] && [ "$ALICE_TENANT" != "$CAROL_TENANT" ] &&
    [ "$BOB_TENANT" != "$CAROL_TENANT" ]; then
    pass
    note "alice=${ALICE_TENANT} bob=${BOB_TENANT} carol=${CAROL_TENANT}"
  else
    fail "tenant ids not distinct/empty: alice='${ALICE_TENANT}' bob='${BOB_TENANT}' carol='${CAROL_TENANT}'"
  fi
fi

begin "alice: POST /v1/connectors (gmail, mailbox ${MAILBOX}) -> 201 + id"
body=""
if [ -z "$ALICE_TOKEN" ]; then
  fail "skipped: no alice token"
elif body="$("${CURL[@]}" -X POST "${GATEWAY_URL}/v1/connectors" \
  -H "Authorization: Bearer ${ALICE_TOKEN}" -H 'Content-Type: application/json' \
  -d '{"connector_id":"gmail","display_name":"leakage probe '"${RUN_ID}"'",
       "config":{"base_url":"'"${FAKE_GMAIL_INTERNAL_URL}"'","user_email":"'"${MAILBOX}"'"}}' 2>&1)" &&
  ALICE_IID="$(json_field id <<<"$body")" && [ -n "$ALICE_IID" ]; then
  pass
  note "instance id: ${ALICE_IID}"
else
  fail "no instance id; response: ${body:0:300}"
fi

begin "alice: PUT /v1/connectors/{id}/token (fake-gmail dev token) -> 204"
if [ -z "$ALICE_IID" ]; then
  fail "skipped: no instance"
elif code="$(http_code -X PUT "${GATEWAY_URL}/v1/connectors/${ALICE_IID}/token" \
  -H "Authorization: Bearer ${ALICE_TOKEN}" -H 'Content-Type: application/json' \
  -d '{"token":"fake-gmail-token:'"${MAILBOX}"'"}')" && [ "$code" = "204" ]; then
  pass
else
  fail "expected 204, got $code"
fi

begin "fake-gmail: seed ${SEED_COUNT} deterministic messages (seed=${RUN_ID})"
if body="$("${CURL[@]}" -X POST "${FAKE_GMAIL_ADMIN_URL}/admin/users/${MAILBOX}/seed" \
  -H 'Content-Type: application/json' \
  -d '{"count":'"${SEED_COUNT}"',"seed":'"${RUN_ID}"'}' 2>&1)" &&
  seeded="$(json_field seeded <<<"$body")" && [ "$seeded" = "$SEED_COUNT" ]; then
  pass
else
  fail "seed response: ${body:0:300}"
fi

begin "fake-gmail: add probe message carrying ${TOKEN_MAIL}"
if body="$("${CURL[@]}" -X POST "${FAKE_GMAIL_ADMIN_URL}/admin/users/${MAILBOX}/messages" \
  -H 'Content-Type: application/json' \
  -d '{"subject":"leak probe '"${RUN_ID}"'",
       "body":"Cross tenant probe. Tracking reference '"${TOKEN_MAIL}"'.",
       "from":"ava.alvarez@example.com","to":"'"${MAILBOX}"'"}' 2>&1)" &&
  [ -n "$(json_field id <<<"$body")" ]; then
  pass
else
  fail "add message response: ${body:0:300}"
fi

begin "alice: POST /v1/upload (file carrying ${TOKEN_UPLOAD}) -> 202 + doc_id"
printf 'Alice private upload for leakage run %s.\nTracking reference %s.\n' \
  "$RUN_ID" "$TOKEN_UPLOAD" >"$TMP_ALICE_FILE"
if [ -z "$ALICE_TOKEN" ]; then
  fail "skipped: no alice token"
elif body="$("${CURL[@]}" -X POST "${GATEWAY_URL}/v1/upload" \
  -H "Authorization: Bearer ${ALICE_TOKEN}" \
  -F "file=@${TMP_ALICE_FILE};filename=alice-leak-${RUN_ID}.txt" \
  -F "title=alice leak probe ${RUN_ID}" 2>&1)" &&
  ALICE_UP_DOC="$(json_field doc_id <<<"$body")" && [ -n "$ALICE_UP_DOC" ]; then
  pass
  note "upload doc_id: ${ALICE_UP_DOC}"
else
  fail "upload response: ${body:0:300}"
fi

begin "alice: all rare tokens searchable (5 seeded + mail + upload; <=${LEAK_WAIT_SECS}s)"
if [ -z "$ALICE_IID" ] || [ -z "$ALICE_UP_DOC" ]; then
  fail "skipped: setup incomplete"
else
  all_found=1
  for t in "${SEED_TOKENS[@]}" "$TOKEN_MAIL" "$TOKEN_UPLOAD"; do
    if ! wait_for_alice_keyword "$t" "$LEAK_WAIT_SECS"; then
      all_found=0
      printf '[token %s not searchable] ' "$t"
      break
    fi
    printf '%s ' "$t"
  done
  if [ "$all_found" = "1" ]; then
    # Collect alice's doc_ids for every run token: the forbidden set for all
    # cross-tenant scans. (Seeded qzx tokens may also match alice's docs from
    # prior runs — they are hers, so they belong in the forbidden set too.)
    ids="$(
      for t in "${SEED_TOKENS[@]}" "$TOKEN_MAIL" "$TOKEN_UPLOAD"; do
        search_raw "$ALICE_TOKEN" --data-urlencode "q=${t}" \
          --data-urlencode "mode=keyword" --data-urlencode "limit=50" | hit_doc_ids
      done | sort -u
    )"
    ALICE_DOC_IDS="$(tr '\n' ',' <<<"$ids" | sed 's/,$//')"
    if [ -z "$ALICE_DOC_IDS" ]; then
      fail "found tokens but collected no doc_ids"
    elif ! grep -qF "$ALICE_UP_DOC" <<<"$ids"; then
      fail "alice's upload doc_id ${ALICE_UP_DOC} missing from her own results"
    else
      pass
      note "alice doc_id set ($(wc -l <<<"$ids" | tr -d ' ') docs): ${ALICE_DOC_IDS:0:160}..."
    fi
  else
    fail "pipeline did not index all probe tokens within ${LEAK_WAIT_SECS}s"
  fi
fi

# Re-fetch all tokens: the indexing wait can outlive the 300s dev token TTL.
refresh_tokens || true

# === Phase 2: search isolation ===============================================
# Strict zero: keyword-mode searches on run-unique tokens (no NN arm, token
# exists nowhere outside alice's tenant). Other modes: zero LEAKED hits.

assert_no_leak "bob: keyword search '${TOKEN_MAIL}' -> 0 hits" \
  "$BOB_TOKEN" 1 \
  --data-urlencode "q=${TOKEN_MAIL}" --data-urlencode "mode=keyword" \
  --data-urlencode "limit=50"

assert_no_leak "bob: keyword search '${TOKEN_UPLOAD}' -> 0 hits" \
  "$BOB_TOKEN" 1 \
  --data-urlencode "q=${TOKEN_UPLOAD}" --data-urlencode "mode=keyword" \
  --data-urlencode "limit=50"

begin "bob: keyword search of all 5 seeded rare tokens -> no alice docs"
seed_leaks=""
for t in "${SEED_TOKENS[@]}"; do
  body="$(search_raw "$BOB_TOKEN" --data-urlencode "q=${t}" \
    --data-urlencode "mode=keyword" --data-urlencode "limit=50" 2>&1)" || {
    seed_leaks="search ${t} failed: ${body:0:120}"
    break
  }
  scan="$(scan_hits "$LEAK_MARKERS" "$ALICE_DOC_IDS" <<<"$body")"
  if ! grep -q 'leaks=0' <<<"$(head -n1 <<<"$scan")"; then
    seed_leaks="token ${t}: $(tail -n +2 <<<"$scan" | tr '\n' '; ')"
    break
  fi
done
if [ -z "$seed_leaks" ]; then
  pass
else
  fail "CROSS-TENANT LEAK: ${seed_leaks}"
fi

assert_no_leak "bob: hybrid search '${TOKEN_MAIL}' -> no alice docs/markers" \
  "$BOB_TOKEN" 0 \
  --data-urlencode "q=${TOKEN_MAIL}" --data-urlencode "mode=hybrid" \
  --data-urlencode "limit=50"

assert_no_leak "bob: hybrid (default mode) search '${TOKEN_UPLOAD}' -> no alice docs" \
  "$BOB_TOKEN" 0 \
  --data-urlencode "q=${TOKEN_UPLOAD}" --data-urlencode "limit=50"

assert_no_leak "bob: vector search '${TOKEN_MAIL}' -> no alice docs/markers" \
  "$BOB_TOKEN" 0 \
  --data-urlencode "q=${TOKEN_MAIL}" --data-urlencode "mode=vector" \
  --data-urlencode "limit=50"

assert_no_leak "bob: keyword + types=EMAIL + participant=${MAILBOX_MARKER} -> 0 hits" \
  "$BOB_TOKEN" 1 \
  --data-urlencode "q=${TOKEN_MAIL}" --data-urlencode "mode=keyword" \
  --data-urlencode "types=EMAIL" --data-urlencode "participant=${MAILBOX_MARKER}" \
  --data-urlencode "limit=50"

assert_no_leak "bob: keyword + types=EMAIL,FILE + participant=ava.alvarez -> 0 hits" \
  "$BOB_TOKEN" 1 \
  --data-urlencode "q=${TOKEN_MAIL}" --data-urlencode "mode=keyword" \
  --data-urlencode "types=EMAIL,FILE" --data-urlencode "participant=ava.alvarez" \
  --data-urlencode "limit=50"

assert_no_leak "carol: keyword search '${TOKEN_MAIL}' -> 0 hits" \
  "$CAROL_TOKEN" 1 \
  --data-urlencode "q=${TOKEN_MAIL}" --data-urlencode "mode=keyword" \
  --data-urlencode "limit=50"

assert_no_leak "carol: keyword search '${TOKEN_UPLOAD}' -> 0 hits" \
  "$CAROL_TOKEN" 1 \
  --data-urlencode "q=${TOKEN_UPLOAD}" --data-urlencode "mode=keyword" \
  --data-urlencode "limit=50"

begin "carol: keyword search of all 5 seeded rare tokens -> no alice docs"
seed_leaks=""
for t in "${SEED_TOKENS[@]}"; do
  body="$(search_raw "$CAROL_TOKEN" --data-urlencode "q=${t}" \
    --data-urlencode "mode=keyword" --data-urlencode "limit=50" 2>&1)" || {
    seed_leaks="search ${t} failed: ${body:0:120}"
    break
  }
  scan="$(scan_hits "$LEAK_MARKERS" "$ALICE_DOC_IDS" <<<"$body")"
  if ! grep -q 'leaks=0' <<<"$(head -n1 <<<"$scan")"; then
    seed_leaks="token ${t}: $(tail -n +2 <<<"$scan" | tr '\n' '; ')"
    break
  fi
done
if [ -z "$seed_leaks" ]; then
  pass
else
  fail "CROSS-TENANT LEAK: ${seed_leaks}"
fi

assert_no_leak "carol: hybrid search '${TOKEN_MAIL}' -> no alice docs/markers" \
  "$CAROL_TOKEN" 0 \
  --data-urlencode "q=${TOKEN_MAIL}" --data-urlencode "mode=hybrid" \
  --data-urlencode "limit=50"

assert_no_leak "carol: keyword + types=EMAIL + participant=${MAILBOX_MARKER} -> 0 hits" \
  "$CAROL_TOKEN" 1 \
  --data-urlencode "q=${TOKEN_MAIL}" --data-urlencode "mode=keyword" \
  --data-urlencode "types=EMAIL" --data-urlencode "participant=${MAILBOX_MARKER}" \
  --data-urlencode "limit=50"

# === Phase 3: resource-ID isolation ==========================================

begin "bob+carol: GET /v1/connectors does NOT list alice's instance"
if [ -z "$ALICE_IID" ]; then
  fail "skipped: no instance"
else
  leak=""
  for who_tok in "$BOB_TOKEN" "$CAROL_TOKEN"; do
    body="$("${CURL[@]}" -H "Authorization: Bearer ${who_tok}" "${GATEWAY_URL}/v1/connectors" 2>&1)" || {
      leak="list failed: ${body:0:120}"
      break
    }
    if grep -qF "$ALICE_IID" <<<"$body"; then
      leak="alice's instance ${ALICE_IID} visible in a foreign tenant's list"
      break
    fi
  done
  if [ -z "$leak" ]; then pass; else fail "CROSS-TENANT LEAK: $leak"; fi
fi

begin "bob: GET /v1/connectors/{alice-id} == GET {random-id}, never 200/403"
if [ -z "$ALICE_IID" ]; then
  fail "skipped: no instance"
else
  rand_id="00000000-dead-beef-0000-$(printf '%012d' "$((RUN_ID % 1000000))")"
  ga="$(http_code -H "Authorization: Bearer ${BOB_TOKEN}" "${GATEWAY_URL}/v1/connectors/${ALICE_IID}")"
  gr="$(http_code -H "Authorization: Bearer ${BOB_TOKEN}" "${GATEWAY_URL}/v1/connectors/${rand_id}")"
  case "$ga" in
    200 | 2?? | 403) fail "existence oracle: GET alice's id -> $ga" ;;
    *)
      if [ "$ga" = "$gr" ]; then
        pass
        note "both alice's id and a random id answer ${ga} (no oracle; GET is unrouted -> 405)"
      else
        fail "oracle: alice's id -> $ga but random id -> $gr"
      fi
      ;;
  esac
fi

begin "bob: DELETE /v1/connectors/{alice-id} -> 404 (and not deleted)"
if [ -z "$ALICE_IID" ]; then
  fail "skipped: no instance"
elif code="$(http_code -X DELETE -H "Authorization: Bearer ${BOB_TOKEN}" \
  "${GATEWAY_URL}/v1/connectors/${ALICE_IID}")" && [ "$code" = "404" ]; then
  pass
else
  fail "expected 404, got $code"
fi

begin "bob: PUT /v1/connectors/{alice-id}/token -> 404 (no overwrite)"
if [ -z "$ALICE_IID" ]; then
  fail "skipped: no instance"
elif code="$(http_code -X PUT -H "Authorization: Bearer ${BOB_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{"token":"fake-gmail-token:attacker@example.com"}' \
  "${GATEWAY_URL}/v1/connectors/${ALICE_IID}/token")" && [ "$code" = "404" ]; then
  pass
else
  fail "expected 404, got $code"
fi

begin "carol: DELETE /v1/connectors/{alice-id} -> 404"
if [ -z "$ALICE_IID" ]; then
  fail "skipped: no instance"
elif code="$(http_code -X DELETE -H "Authorization: Bearer ${CAROL_TOKEN}" \
  "${GATEWAY_URL}/v1/connectors/${ALICE_IID}")" && [ "$code" = "404" ]; then
  pass
else
  fail "expected 404, got $code"
fi

begin "alice: instance still listed with intact sync state after attacks"
if [ -z "$ALICE_IID" ]; then
  fail "skipped: no instance"
elif out="$("${CURL[@]}" -H "Authorization: Bearer ${ALICE_TOKEN}" "${GATEWAY_URL}/v1/connectors" |
  python3 -c '
import json, sys
iid = sys.argv[1]
for e in json.load(sys.stdin):
    inst = e.get("instance") or {}
    if inst.get("id") == iid:
        sync = e.get("sync") or {}
        print("phase=%s docsEmitted=%s" % (sync.get("phase"), sync.get("docsEmitted")))
        sys.exit(0)
sys.exit(1)
' "$ALICE_IID" 2>&1)"; then
  pass
  note "$out"
else
  fail "instance ${ALICE_IID} missing from alice's list: ${out:0:200}"
fi

begin "alice: token still syncs after bob's attempts (new msg searchable)"
if [ -z "$ALICE_IID" ]; then
  fail "skipped: no instance"
elif body="$("${CURL[@]}" -X POST "${FAKE_GMAIL_ADMIN_URL}/admin/users/${MAILBOX}/messages" \
  -H 'Content-Type: application/json' \
  -d '{"subject":"post-attack sync proof '"${RUN_ID}"'",
       "body":"Sync survived. Tracking reference '"${TOKEN_POST}"'.",
       "to":"'"${MAILBOX}"'"}' 2>&1)" && [ -n "$(json_field id <<<"$body")" ]; then
  if wait_for_alice_keyword "$TOKEN_POST" "$LEAK_WAIT_SECS"; then
    pass
    note "bob's 404s had no side effects: alice's connector kept syncing"
  else
    fail "post-attack message not searchable in ${LEAK_WAIT_SECS}s — did an attack break alice's sync?"
  fi
else
  fail "could not add post-attack message: ${body:0:200}"
fi

# Re-fetch tokens again after the sync-proof wait.
refresh_tokens || true

# === Phase 4: header/parameter tenant injection ==============================
# Every variant runs as BOB with alice's REAL tenant id injected; results must
# remain bob-scoped (the run-unique token yields strict 0 hits in keyword mode).

assert_no_leak "bob+header x-asker-tenant:<alice>: search '${TOKEN_MAIL}' -> 0 hits" \
  "$BOB_TOKEN" 1 \
  -H "x-asker-tenant: ${ALICE_TENANT}" \
  --data-urlencode "q=${TOKEN_MAIL}" --data-urlencode "mode=keyword" \
  --data-urlencode "limit=50"

assert_no_leak "bob+header X-Tenant-Id:<alice>: search '${TOKEN_MAIL}' -> 0 hits" \
  "$BOB_TOKEN" 1 \
  -H "X-Tenant-Id: ${ALICE_TENANT}" \
  --data-urlencode "q=${TOKEN_MAIL}" --data-urlencode "mode=keyword" \
  --data-urlencode "limit=50"

assert_no_leak "bob+param tenant_id=<alice>: search '${TOKEN_MAIL}' -> 0 hits" \
  "$BOB_TOKEN" 1 \
  --data-urlencode "tenant_id=${ALICE_TENANT}" \
  --data-urlencode "q=${TOKEN_MAIL}" --data-urlencode "mode=keyword" \
  --data-urlencode "limit=50"

assert_no_leak "bob+param tenant=<alice>: search '${TOKEN_MAIL}' -> 0 hits" \
  "$BOB_TOKEN" 1 \
  --data-urlencode "tenant=${ALICE_TENANT}" \
  --data-urlencode "q=${TOKEN_MAIL}" --data-urlencode "mode=keyword" \
  --data-urlencode "limit=50"

assert_no_leak "bob+param groupname=<alice>: search '${TOKEN_MAIL}' -> 0 hits" \
  "$BOB_TOKEN" 1 \
  --data-urlencode "groupname=${ALICE_TENANT}" \
  --data-urlencode "q=${TOKEN_MAIL}" --data-urlencode "mode=keyword" \
  --data-urlencode "limit=50"

assert_no_leak "bob+param streaming.groupname=<alice>: search -> 0 hits" \
  "$BOB_TOKEN" 1 \
  --data-urlencode "streaming.groupname=${ALICE_TENANT}" \
  --data-urlencode "q=${TOKEN_MAIL}" --data-urlencode "mode=keyword" \
  --data-urlencode "limit=50"

# === Phase 5: token abuse =====================================================

begin "authn: garbage bearer token -> 401"
if code="$(http_code -H 'Authorization: Bearer utter-garbage-not-a-jwt' \
  "${GATEWAY_URL}/v1/search?q=${TOKEN_MAIL}")" && [ "$code" = "401" ]; then
  pass
else
  fail "expected 401, got $code"
fi

begin "authn: expired-looking forged JWT -> 401"
forged="$(python3 -c '
import base64, json, sys
def b64(d): return base64.urlsafe_b64encode(json.dumps(d).encode()).decode().rstrip("=")
hdr = b64({"alg": "RS256", "typ": "JWT", "kid": "forged"})
pay = b64({"sub": sys.argv[1], "iss": sys.argv[2] + "/realms/asker",
           "exp": 1000000000, "iat": 999999000, "aud": "asker-web"})
print(hdr + "." + pay + ".Zm9yZ2VkLXNpZ25hdHVyZQ")
' "$ALICE_TENANT" "$KEYCLOAK_URL")"
if code="$(http_code -H "Authorization: Bearer ${forged}" \
  "${GATEWAY_URL}/v1/search?q=${TOKEN_MAIL}")" && [ "$code" = "401" ]; then
  pass
else
  fail "expected 401, got $code"
fi

begin "authn: alice's real token with one flipped signature char -> 401"
flipped="$(python3 -c '
import sys
t = sys.argv[1]
parts = t.split(".")
if len(parts) < 3:
    sys.exit(1)
s = parts[2]
i = len(s) // 2
c = "x" if s[i] != "x" else "y"
parts[2] = s[:i] + c + s[i + 1:]
print(".".join(parts))
' "$ALICE_TOKEN")"
if [ -z "$flipped" ]; then
  fail "could not derive flipped token"
elif code="$(http_code -H "Authorization: Bearer ${flipped}" \
  "${GATEWAY_URL}/v1/search?q=${TOKEN_MAIL}")" && [ "$code" = "401" ]; then
  pass
else
  fail "expected 401, got $code"
fi

begin "authn: every /v1/* route -> 401 without a token"
bad=""
while IFS='|' read -r method path extra; do
  case "$extra" in
    json) code="$(http_code -X "$method" -H 'Content-Type: application/json' -d '{}' "${GATEWAY_URL}${path}")" ;;
    *) code="$(http_code -X "$method" "${GATEWAY_URL}${path}")" ;;
  esac
  if [ "$code" != "401" ]; then
    bad="${bad}${method} ${path} -> ${code}; "
  fi
done <<'ROUTES'
GET|/v1/me|
GET|/v1/search?q=probe|
GET|/v1/connectors|
POST|/v1/connectors|json
DELETE|/v1/connectors/some-id|
PUT|/v1/connectors/some-id/token|json
POST|/v1/upload|
ROUTES
if [ -z "$bad" ]; then
  pass
else
  fail "non-401 without token: ${bad}"
fi

# === Phase 6: internal surface exposure ======================================

begin "host: internal gRPC/health ports 9100/9101/9200/9201/9300/9301 unreachable"
open_ports=""
for p in 9100 9101 9200 9201 9300 9301; do
  if curl -s -o /dev/null --max-time 2 "http://localhost:${p}/" 2>/dev/null; then
    open_ports="${open_ports}${p} "
  fi
done
if [ -z "$open_ports" ]; then
  pass
  note "control-plane/query/connector-hub bind only inside the compose network (ADR-009)"
else
  fail "internal ports reachable from host: ${open_ports}(trust boundary broken)"
fi

begin "host: fake-gmail :9400 published on 127.0.0.1 ONLY (allowed, dev-only)"
if binding="$("${COMPOSE[@]}" port fake-gmail 9400 2>&1)" &&
  case "$binding" in 127.0.0.1:*) true ;; *) false ;; esac; then
  pass
  note "ALLOWED: ${binding} — loopback-only admin shim for seeding test mailboxes"
  note "(ADR-008: fake-gmail is a dev/CI tool, never deployed to production;"
  note " its unauthenticated /admin surface exists only on this dev host)"
else
  fail "fake-gmail 9400 binding is not loopback-only: ${binding}"
fi

begin "vespa :8082: query WITHOUT streaming.groupname fails closed (0 hits + error)"
body="$("${CURL[@]}" -G "${VESPA_URL}/search/" \
  --data-urlencode 'yql=select * from sources * where userQuery()' \
  --data-urlencode "query=${TOKEN_MAIL}" 2>&1)" || body=""
if out="$(python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    print("unparseable vespa response")
    sys.exit(1)
root = d.get("root", {})
total = root.get("fields", {}).get("totalCount", -1)
children = [c for c in root.get("children", []) if c.get("fields", {}).get("doc_id")]
errors = root.get("errors", [])
msg = errors[0].get("message", "") if errors else ""
if total == 0 and not children and "groupname" in msg:
    print("fail-closed: %s" % msg)
    sys.exit(0)
print("total=%s children=%d errors=%s" % (total, len(children), msg))
sys.exit(1)
' <<<"$body" 2>&1)"; then
  pass
  note "$out"
else
  fail "group-less streaming query did not fail closed: ${out} ${body:0:200}"
fi

begin "vespa :8082: streaming.groupname=<bob> cannot see alice's '${TOKEN_MAIL}'"
body="$("${CURL[@]}" -G "${VESPA_URL}/search/" \
  --data-urlencode 'yql=select * from sources * where userQuery()' \
  --data-urlencode "query=${TOKEN_MAIL}" \
  --data-urlencode "streaming.groupname=${BOB_TENANT}" 2>&1)" || body=""
count="$(python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(1)
print(d.get("root", {}).get("fields", {}).get("totalCount", -1))
' <<<"$body" 2>/dev/null || echo '?')"
if [ "$count" = "0" ]; then
  pass
  note "bob's group holds zero documents matching alice's run token"
else
  fail "expected totalCount 0 in bob's group, got '${count}': ${body:0:200}"
fi
echo "     -> WARN: Vespa query port 8082 is exposed on 127.0.0.1 as a DEV-ONLY"
echo "        convenience (compose). Any host process can query ANY tenant group"
echo "        by naming it — the port itself enforces nothing. The M4 deployment"
echo "        (default-deny NetworkPolicies + mTLS, ADR-009) removes this exposure."

# === Phase 7: upload cross-pollination =======================================

begin "bob: upload file CONTAINING all of alice's rare tokens -> 202"
{
  printf 'Bob attacker upload for leakage run %s. bob-leak+%s\n' "$RUN_ID" "$RUN_ID"
  printf 'Stolen references: %s %s %s\n' "$TOKEN_MAIL" "$TOKEN_UPLOAD" "$TOKEN_POST"
  printf 'Seeded references: %s\n' "${SEED_TOKENS[*]}"
} >"$TMP_BOB_FILE"
if body="$("${CURL[@]}" -X POST "${GATEWAY_URL}/v1/upload" \
  -H "Authorization: Bearer ${BOB_TOKEN}" \
  -F "file=@${TMP_BOB_FILE};filename=bob-leak-${RUN_ID}.txt" \
  -F "title=bob leak probe ${RUN_ID}" 2>&1)" &&
  BOB_UP_DOC="$(json_field doc_id <<<"$body")" && [ -n "$BOB_UP_DOC" ]; then
  pass
  note "bob's doc_id: ${BOB_UP_DOC}"
else
  fail "upload response: ${body:0:300}"
fi

begin "bob: own upload indexed and visible to HIMSELF (<=${LEAK_WAIT_SECS}s)"
if [ -z "$BOB_UP_DOC" ]; then
  fail "skipped: bob upload failed"
else
  found=0 waited=0
  while [ "$waited" -le "$LEAK_WAIT_SECS" ]; do
    ids="$(search_raw "$BOB_TOKEN" --data-urlencode "q=${TOKEN_MAIL}" \
      --data-urlencode "mode=keyword" --data-urlencode "limit=50" 2>/dev/null |
      hit_doc_ids 2>/dev/null || true)"
    if grep -qF "$BOB_UP_DOC" <<<"$ids"; then
      found=1
      break
    fi
    sleep "$POLL_INTERVAL"
    waited=$((waited + POLL_INTERVAL))
    if [ $((waited % 30)) -eq 0 ]; then
      printf '(t+%ss) ' "$waited"
      BOB_TOKEN="$(fetch_token bob)" || true
    fi
  done
  if [ "$found" = "1" ]; then
    pass
    note "bob legitimately sees only his own copy of the stolen tokens"
  else
    fail "bob's upload ${BOB_UP_DOC} not searchable by bob in ${LEAK_WAIT_SECS}s"
  fi
fi

# Fresh alice token for the final asserts (waits above may have aged it).
ALICE_TOKEN="$(fetch_token alice)" || true

begin "alice: '${TOKEN_MAIL}' results contain her docs, NEVER bob's upload"
if [ -z "$BOB_UP_DOC" ]; then
  fail "skipped: bob upload failed"
else
  body="$(search_raw "$ALICE_TOKEN" --data-urlencode "q=${TOKEN_MAIL}" \
    --data-urlencode "mode=hybrid" --data-urlencode "limit=50" 2>&1)" || body=""
  ids="$(hit_doc_ids <<<"$body" 2>/dev/null || true)"
  if grep -qF "$BOB_UP_DOC" <<<"$ids"; then
    fail "CROSS-TENANT LEAK: bob's doc ${BOB_UP_DOC} surfaced in alice's results"
  elif [ -z "$ids" ]; then
    fail "alice's own probe message vanished from her results: ${body:0:200}"
  else
    pass
    note "alice sees $(wc -l <<<"$ids" | tr -d ' ') doc(s), all her own"
  fi
fi

begin "alice: '${TOKEN_UPLOAD}' results exclude bob's upload"
if [ -z "$BOB_UP_DOC" ]; then
  fail "skipped: bob upload failed"
else
  ids="$(search_raw "$ALICE_TOKEN" --data-urlencode "q=${TOKEN_UPLOAD}" \
    --data-urlencode "mode=keyword" --data-urlencode "limit=50" 2>/dev/null |
    hit_doc_ids 2>/dev/null || true)"
  if grep -qF "$BOB_UP_DOC" <<<"$ids"; then
    fail "CROSS-TENANT LEAK: bob's doc ${BOB_UP_DOC} in alice's upload-token results"
  elif ! grep -qF "$ALICE_UP_DOC" <<<"$ids"; then
    fail "alice's own upload ${ALICE_UP_DOC} missing from her results"
  else
    pass
  fi
fi

begin "alice: seeded token '${SEED_TOKENS[0]}' results exclude bob's upload"
if [ -z "$BOB_UP_DOC" ]; then
  fail "skipped: bob upload failed"
else
  ids="$(search_raw "$ALICE_TOKEN" --data-urlencode "q=${SEED_TOKENS[0]}" \
    --data-urlencode "mode=keyword" --data-urlencode "limit=50" 2>/dev/null |
    hit_doc_ids 2>/dev/null || true)"
  if grep -qF "$BOB_UP_DOC" <<<"$ids"; then
    fail "CROSS-TENANT LEAK: bob's doc ${BOB_UP_DOC} in alice's seeded-token results"
  elif [ -z "$ids" ]; then
    fail "alice's seeded message for ${SEED_TOKENS[0]} missing from her results"
  else
    pass
  fi
fi

# === Summary ==================================================================

echo
echo "== Summary =="
printf '  %-4s %-6s %s\n' "STEP" "STATUS" "CHECK"
for row in ${SUMMARY[@]+"${SUMMARY[@]}"}; do
  IFS='|' read -r num status name <<<"$row"
  printf '  %-4s %-6s %s\n' "$num" "$status" "$name"
done
echo

if [ "$FAILED" -ne 0 ]; then
  echo "LEAKAGE: FAILED — at least one isolation property is broken or unprovable"
  exit 1
fi
echo "LEAKAGE: ALL ${STEP} CHECKS PASSED — no cross-tenant leak observed"
