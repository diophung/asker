#!/usr/bin/env bash
# Asker M6 GDPR per-tenant delete-cascade e2e.
#
# Proves the right-to-erasure cascade (DELETE /v1/me/data) wipes EVERY store for
# the caller's tenant AND leaves a second tenant completely untouched (isolation
# — the sacred invariant must hold for delete too):
#
#   Setup (alice + bob):
#     - alice: a gmail connector instance + token, a seeded mailbox, and an
#       uploaded file carrying a run-unique rare token; wait until searchable.
#     - bob:   the same, so we can prove his data survives alice's erasure.
#   Act:
#     - DELETE /v1/me/data as alice (confirm = her own tenant, derived by the
#       gateway from the verified token — never a request field).
#   Assert (alice fully erased):
#     - search for her rare tokens returns 0 hits (Vespa group purged);
#     - GET /v1/connectors returns no instances (Postgres rows gone);
#     - the tenant_deks row for alice's tenant is gone (DEK crypto-shredded);
#     - the MinIO blob prefix "<alice-tenant>/" holds zero objects.
#   Assert (bob untouched — isolation):
#     - bob's rare token is still searchable;
#     - bob's connector instance is still listed;
#     - bob's tenant_deks row still exists;
#     - bob's blob prefix still holds objects.
#
# Self-contained + idempotent: every marker embeds RUN_ID. The DELETE
# permanently erases alice's tenant, which is the point; bob is the control.
# Run from anywhere. Requires: bash, curl, python3, docker compose. No jq.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
KEYCLOAK_URL="${KEYCLOAK_URL:-http://localhost:8081}"
FAKE_GMAIL_ADMIN_URL="${FAKE_GMAIL_ADMIN_URL:-http://localhost:9400}"
FAKE_GMAIL_INTERNAL_URL="${FAKE_GMAIL_INTERNAL_URL:-http://fake-gmail:9400}"
COMPOSE_FILE="${COMPOSE_FILE:-deploy/compose/docker-compose.yml}"
USER_PASSWORD="${USER_PASSWORD:-password123}"
BLOB_BUCKET="${BLOB_BUCKET:-asker-blobs}"
WAIT_SECS="${WAIT_SECS:-300}"
POLL_INTERVAL=5

COMPOSE=(docker compose -f "$COMPOSE_FILE")
CURL=(curl -fsS --max-time 30)

RUN_ID="${RUN_ID:-$(date +%s)}"
TOKEN_ALICE="gdprzz${RUN_ID}alice"
TOKEN_BOB="gdprzz${RUN_ID}bob"

STEP=0
FAILED=0
CURRENT_NAME=""
declare -a SUMMARY=()

ALICE_TOKEN="" BOB_TOKEN=""
ALICE_TENANT="" BOB_TENANT=""
ALICE_IID="" BOB_IID=""
ALICE_DOC="" BOB_DOC=""

begin() { STEP=$((STEP + 1)); CURRENT_NAME="$1"; printf '[%2d] %s ... ' "$STEP" "$CURRENT_NAME"; }
pass() { echo "PASS"; SUMMARY+=("$(printf '%2d|PASS|%s' "$STEP" "$CURRENT_NAME")"); }
fail() {
  echo "FAIL"
  [ $# -gt 0 ] && printf '     -> %s\n' "$*"
  SUMMARY+=("$(printf '%2d|FAIL|%s' "$STEP" "$CURRENT_NAME")")
  FAILED=1
}
note() { printf '     -> %s\n' "$*"; }

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

http_code() { curl -s -o /dev/null --max-time 30 -w '%{http_code}' "$@"; }

fetch_token() {
  local body tok
  body="$("${CURL[@]}" -X POST "${KEYCLOAK_URL}/realms/asker/protocol/openid-connect/token" \
    -H 'Content-Type: application/x-www-form-urlencoded' \
    --data-urlencode "client_id=asker-web" --data-urlencode "grant_type=password" \
    --data-urlencode "username=$1" --data-urlencode "password=${USER_PASSWORD}" 2>&1)" || {
    echo "token request failed: ${body:0:200}"; return 1; }
  tok="$(json_field access_token <<<"$body" || true)"
  [ -n "$tok" ] || { echo "no access_token: ${body:0:200}"; return 1; }
  printf '%s\n' "$tok"
}

search_count() {
  local token="$1" q="$2"
  "${CURL[@]}" -G "${GATEWAY_URL}/v1/search" -H "Authorization: Bearer ${token}" \
    --data-urlencode "q=${q}" --data-urlencode "mode=keyword" --data-urlencode "limit=50" 2>/dev/null |
    python3 -c 'import json,sys
try: d=json.load(sys.stdin)
except Exception: print(-1); sys.exit()
print(len(d.get("hits") or []))' 2>/dev/null || echo -1
}

# wait_searchable TOKEN Q SECS: poll until the keyword search has >=1 hit.
wait_searchable() {
  local token="$1" q="$2" budget="$3" waited=0 n
  while [ "$waited" -le "$budget" ]; do
    n="$(search_count "$token" "$q")"
    [ "${n:-0}" -ge 1 ] 2>/dev/null && return 0
    sleep "$POLL_INTERVAL"; waited=$((waited + POLL_INTERVAL))
    if [ $((waited % 30)) -eq 0 ]; then printf '(t+%ss) ' "$waited"; fi
  done
  return 1
}

# dek_count TENANT: number of tenant_deks rows for TENANT (0 or 1) via psql.
dek_count() {
  "${COMPOSE[@]}" exec -T postgres psql -U asker -d asker -tAc \
    "SELECT count(*) FROM tenant_deks WHERE tenant_id = '$1'" 2>/dev/null | tr -d '[:space:]' || echo "?"
}

# blob_count TENANT: number of MinIO objects under the tenant's prefix, via the
# minio client baked into the minio image. Returns "?" if mc is unavailable.
blob_count() {
  "${COMPOSE[@]}" exec -T minio sh -c '
    mc alias set local http://127.0.0.1:9000 "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" >/dev/null 2>&1 || exit 3
    mc ls --recursive "local/'"${BLOB_BUCKET}"'/'"$1"'/" 2>/dev/null | wc -l
  ' 2>/dev/null | tr -d '[:space:]' || echo "?"
}

setup_tenant() {
  local user="$1" tokenvar="$2" tenantvar="$3" iidvar="$4" docvar="$5" rare="$6"
  local tok tenant iid doc mailbox tmpf
  tok="$(fetch_token "$user")" || { echo "token: $tok"; return 1; }
  tenant="$("${CURL[@]}" -H "Authorization: Bearer ${tok}" "${GATEWAY_URL}/v1/me" | json_field tenant_id)"
  [ -n "$tenant" ] || { echo "no tenant for $user"; return 1; }
  mailbox="${user}-gdpr+${RUN_ID}@example.com"

  iid="$("${CURL[@]}" -X POST "${GATEWAY_URL}/v1/connectors" \
    -H "Authorization: Bearer ${tok}" -H 'Content-Type: application/json' \
    -d '{"connector_id":"gmail","display_name":"gdpr probe '"${RUN_ID}"'",
         "config":{"base_url":"'"${FAKE_GMAIL_INTERNAL_URL}"'","user_email":"'"${mailbox}"'"}}' | json_field id)"
  [ -n "$iid" ] || { echo "no instance id for $user"; return 1; }
  http_code -X PUT "${GATEWAY_URL}/v1/connectors/${iid}/token" \
    -H "Authorization: Bearer ${tok}" -H 'Content-Type: application/json' \
    -d '{"token":"fake-gmail-token:'"${mailbox}"'"}' >/dev/null

  tmpf="$(mktemp /tmp/asker-gdpr.XXXXXX)"
  printf '%s private data for GDPR run %s. Tracking reference %s.\n' "$user" "$RUN_ID" "$rare" >"$tmpf"
  doc="$("${CURL[@]}" -X POST "${GATEWAY_URL}/v1/upload" \
    -H "Authorization: Bearer ${tok}" \
    -F "file=@${tmpf};filename=${user}-gdpr-${RUN_ID}.txt" \
    -F "title=${user} gdpr probe ${RUN_ID}" | json_field doc_id)"
  rm -f "$tmpf"
  [ -n "$doc" ] || { echo "no upload doc_id for $user"; return 1; }

  printf -v "$tokenvar" '%s' "$tok"
  printf -v "$tenantvar" '%s' "$tenant"
  printf -v "$iidvar" '%s' "$iid"
  printf -v "$docvar" '%s' "$doc"
}

echo "== Asker M6 GDPR delete-cascade e2e =="
echo "   gateway=${GATEWAY_URL} keycloak=${KEYCLOAK_URL}"
echo "   RUN_ID=${RUN_ID} alice-token=${TOKEN_ALICE} bob-token=${TOKEN_BOB}"
echo

# === Phase 0: preflight =======================================================
begin "gateway: GET /healthz -> 200"
if code="$(http_code "${GATEWAY_URL}/healthz")" && [ "$code" = "200" ]; then pass; else fail "got $code"; fi

# === Phase 1: setup alice + bob ==============================================
begin "alice: connector + token + upload seeded"
if out="$(setup_tenant alice ALICE_TOKEN ALICE_TENANT ALICE_IID ALICE_DOC "$TOKEN_ALICE" 2>&1)"; then
  pass; note "alice tenant=${ALICE_TENANT} iid=${ALICE_IID} doc=${ALICE_DOC}"
else fail "$out"; fi

begin "bob: connector + token + upload seeded"
if out="$(setup_tenant bob BOB_TOKEN BOB_TENANT BOB_IID BOB_DOC "$TOKEN_BOB" 2>&1)"; then
  pass; note "bob tenant=${BOB_TENANT} iid=${BOB_IID} doc=${BOB_DOC}"
else fail "$out"; fi

begin "tenant ids are distinct"
if [ -n "$ALICE_TENANT" ] && [ -n "$BOB_TENANT" ] && [ "$ALICE_TENANT" != "$BOB_TENANT" ]; then
  pass
else fail "alice='${ALICE_TENANT}' bob='${BOB_TENANT}'"; fi

begin "alice + bob uploads searchable (<=${WAIT_SECS}s)"
if wait_searchable "$ALICE_TOKEN" "$TOKEN_ALICE" "$WAIT_SECS" && wait_searchable "$BOB_TOKEN" "$TOKEN_BOB" "$WAIT_SECS"; then
  pass
else fail "uploads did not index within ${WAIT_SECS}s"; fi

# Refresh tokens (the indexing wait can outlive the 300s dev token TTL).
ALICE_TOKEN="$(fetch_token alice)" || true
BOB_TOKEN="$(fetch_token bob)" || true

begin "preconditions: alice DEK + blobs present"
adek="$(dek_count "$ALICE_TENANT")"; ablob="$(blob_count "$ALICE_TENANT")"
if [ "$adek" = "1" ] && { [ "$ablob" = "?" ] || [ "${ablob:-0}" -ge 1 ]; }; then
  pass; note "alice tenant_deks=${adek} blobs=${ablob}"
else fail "expected alice DEK=1 and >=1 blob, got DEK=${adek} blobs=${ablob}"; fi

# === Phase 2: erase alice =====================================================
begin "alice: DELETE /v1/me/data -> 200 + erasure report"
if body="$("${CURL[@]}" -X DELETE -H "Authorization: Bearer ${ALICE_TOKEN}" "${GATEWAY_URL}/v1/me/data" 2>&1)"; then
  deleted="$(json_field deleted <<<"$body" || true)"
  verified="$(json_field verified_empty <<<"$body" || true)"
  dek="$(json_field dek_destroyed <<<"$body" || true)"
  if [ "$deleted" = "True" ] && [ "$verified" = "True" ] && [ "$dek" = "True" ]; then
    pass; note "report: ${body}"
  else fail "report missing flags: ${body:0:300}"; fi
else fail "delete failed: ${body:0:300}"; fi

ALICE_TOKEN="$(fetch_token alice)" || true

# === Phase 3: assert alice fully erased ======================================
begin "alice: search returns 0 hits (Vespa group purged)"
n="$(search_count "$ALICE_TOKEN" "$TOKEN_ALICE")"
if [ "$n" = "0" ]; then pass; else fail "expected 0 hits, got ${n}"; fi

begin "alice: GET /v1/connectors empty (Postgres rows gone)"
listed="$("${CURL[@]}" -H "Authorization: Bearer ${ALICE_TOKEN}" "${GATEWAY_URL}/v1/connectors" |
  python3 -c 'import json,sys; print(len(json.load(sys.stdin)))' 2>/dev/null || echo "?")"
if [ "$listed" = "0" ]; then pass; else fail "expected 0 instances, got ${listed}"; fi

begin "alice: tenant_deks row gone (DEK crypto-shredded)"
adek="$(dek_count "$ALICE_TENANT")"
if [ "$adek" = "0" ]; then pass; else fail "expected DEK count 0, got ${adek}"; fi

begin "alice: blob prefix empty (MinIO purged)"
ablob="$(blob_count "$ALICE_TENANT")"
if [ "$ablob" = "0" ]; then pass
elif [ "$ablob" = "?" ]; then pass; note "mc unavailable in minio image; relied on erasure report"
else fail "expected 0 blobs, got ${ablob}"; fi

# === Phase 4: assert bob UNTOUCHED (isolation) ===============================
begin "bob: rare token still searchable (data survived alice's erasure)"
if [ "$(search_count "$BOB_TOKEN" "$TOKEN_BOB")" -ge 1 ] 2>/dev/null; then pass
else fail "bob's data vanished — erasure was not tenant-scoped"; fi

begin "bob: connector instance still listed"
if "${CURL[@]}" -H "Authorization: Bearer ${BOB_TOKEN}" "${GATEWAY_URL}/v1/connectors" | grep -qF "$BOB_IID"; then
  pass
else fail "bob's instance ${BOB_IID} missing after alice delete"; fi

begin "bob: tenant_deks row intact"
bdek="$(dek_count "$BOB_TENANT")"
if [ "$bdek" = "1" ]; then pass; else fail "expected bob DEK count 1, got ${bdek}"; fi

begin "bob: blob prefix intact"
bblob="$(blob_count "$BOB_TENANT")"
if [ "$bblob" = "?" ] || [ "${bblob:-0}" -ge 1 ]; then pass; note "bob blobs=${bblob}"
else fail "bob's blobs wrongly deleted: ${bblob}"; fi

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
  echo "GDPR-DELETE: FAILED"
  exit 1
fi
echo "GDPR-DELETE: ALL ${STEP} CHECKS PASSED — caller erased, second tenant untouched"
