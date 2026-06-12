#!/usr/bin/env bash
# Asker M0 end-to-end smoke test.
#
# This is THE M0 acceptance test: it verifies every piece of the dev stack is
# healthy, that the gateway enforces OIDC authn and derives tenant_id from the
# verified token, and that Vespa streaming search isolates tenants by group.
#
# Run from anywhere: paths are resolved relative to the repo root.
# Requires: bash, curl, python3, docker compose. No jq.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
KEYCLOAK_URL="${KEYCLOAK_URL:-http://localhost:8081}"
VESPA_URL="${VESPA_URL:-http://localhost:8082}"
VESPA_CFG_URL="${VESPA_CFG_URL:-http://localhost:19071}"
TEI_URL="${TEI_URL:-http://localhost:8083}"
MINIO_URL="${MINIO_URL:-http://localhost:9000}"
COMPOSE_FILE="${COMPOSE_FILE:-deploy/compose/docker-compose.yml}"

COMPOSE=(docker compose -f "$COMPOSE_FILE")
CURL=(curl -fsS --max-time 30)

SMOKE_TENANT_A="smoke-tenant-a"
SMOKE_TENANT_B="smoke-tenant-b"
SMOKE_DOC_ID="smoke-1"
SMOKE_TOKEN="xyzzyplugh"
SMOKE_DOC_URL="${VESPA_URL}/document/v1/asker/doc/group/${SMOKE_TENANT_A}/${SMOKE_DOC_ID}"

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

# jwt_claim TOKEN CLAIM: decode the JWT payload (no signature check) and print
# the claim value ("" if absent).
jwt_claim() {
  python3 -c '
import base64, json, sys
parts = sys.argv[1].split(".")
if len(parts) < 2:
    sys.exit(1)
payload = parts[1] + "=" * (-len(parts[1]) % 4)
claims = json.loads(base64.urlsafe_b64decode(payload))
val = claims.get(sys.argv[2])
print("" if val is None else val)
' "$1" "$2"
}

http_code() {
  curl -s -o /dev/null --max-time 30 -w '%{http_code}' "$@"
}

# health_status URL: print the Vespa /state/v1/health status code (e.g. "up").
health_status() {
  local body
  body="$("${CURL[@]}" "$1" 2>&1)" || {
    echo "${body:0:200}"
    return 1
  }
  python3 -c '
import json, sys
try:
    data = json.load(sys.stdin)
except Exception:
    print("unparseable health response")
    sys.exit(1)
print(data.get("status", {}).get("code", ""))
' <<<"$body"
}

# fetch_token USER PASSWORD: print an access token obtained via the dev
# password grant; on failure print a diagnostic and return non-zero.
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

# query_count GROUP: print the totalCount for the rare-token query scoped to
# the given streaming group.
query_count() {
  local body
  body="$("${CURL[@]}" -G "${VESPA_URL}/search/" \
    --data-urlencode 'yql=select * from sources * where userQuery()' \
    --data-urlencode "query=${SMOKE_TOKEN}" \
    --data-urlencode "streaming.groupname=$1" 2>&1)" || {
    # Diagnostics on stderr only: callers capture stdout as the count.
    echo "query failed: ${body:0:200}" >&2
    return 1
  }
  python3 -c '
import json, sys
try:
    data = json.load(sys.stdin)
except Exception:
    print("unparseable search response", file=sys.stderr)
    sys.exit(1)
print(data.get("root", {}).get("fields", {}).get("totalCount", 0))
' <<<"$body"
}

# wait_for_count GROUP WANT [ATTEMPTS]: poll until the query count matches.
wait_for_count() {
  local group="$1" want="$2" attempts="${3:-15}" got="?" i
  for i in $(seq 1 "$attempts"); do
    got="$(query_count "$group" 2>/dev/null || echo '?')"
    if [ "$got" = "$want" ]; then
      return 0
    fi
    sleep 2
  done
  echo "last count for group $group was '$got' (wanted $want)"
  return 1
}

cleanup() {
  # Best-effort removal of the smoke probe document (idempotent).
  curl -fsS --max-time 30 -X DELETE "$SMOKE_DOC_URL" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "== Asker M0 smoke test =="
echo "   gateway=${GATEWAY_URL} keycloak=${KEYCLOAK_URL} vespa=${VESPA_URL}"
echo "   vespa-cfg=${VESPA_CFG_URL} tei=${TEI_URL} minio=${MINIO_URL}"
echo "   compose=${COMPOSE_FILE}"
echo

# --- a. Infra health ---------------------------------------------------------

begin "postgres: pg_isready"
if out="$("${COMPOSE[@]}" exec -T postgres pg_isready -U asker 2>&1)"; then
  pass
else
  fail "$out"
fi

begin "redis: PING"
if out="$("${COMPOSE[@]}" exec -T redis redis-cli ping 2>&1)" && [ "$out" = "PONG" ]; then
  pass
else
  fail "expected PONG, got: $out"
fi

begin "redpanda: rpk cluster health"
if out="$("${COMPOSE[@]}" exec -T redpanda rpk cluster health 2>&1)" &&
  grep -Eq '^Healthy:[[:space:]]+true' <<<"$out"; then
  pass
else
  fail "cluster not healthy: $out"
fi

begin "minio: GET /minio/health/live -> 200"
if code="$(http_code "${MINIO_URL}/minio/health/live")" && [ "$code" = "200" ]; then
  pass
else
  fail "expected 200, got $code"
fi

begin "keycloak: OIDC discovery has issuer"
if body="$("${CURL[@]}" "${KEYCLOAK_URL}/realms/asker/.well-known/openid-configuration" 2>&1)" &&
  issuer="$(json_field issuer <<<"$body")" && [ -n "$issuer" ]; then
  pass
  printf '     -> issuer: %s\n' "$issuer"
else
  fail "missing issuer; response: ${body:0:200}"
fi

begin "vespa: config server health up"
if status="$(health_status "${VESPA_CFG_URL}/state/v1/health")" && [ "$status" = "up" ]; then
  pass
else
  fail "expected up, got: ${status:-<none>}"
fi

begin "vespa: container health up"
if status="$(health_status "${VESPA_URL}/state/v1/health")" && [ "$status" = "up" ]; then
  pass
else
  fail "expected up, got: ${status:-<none>} (did vespa/deploy.sh run?)"
fi

# --- b. TEI ------------------------------------------------------------------

begin "tei: GET /health -> 200"
if code="$(http_code "${TEI_URL}/health")" && [ "$code" = "200" ]; then
  pass
else
  fail "expected 200, got $code"
fi

begin "tei: POST /embed returns a float vector"
dim=""
if body="$("${CURL[@]}" -X POST "${TEI_URL}/embed" \
  -H 'Content-Type: application/json' \
  -d '{"inputs":"hello world"}' 2>&1)" &&
  dim="$(python3 -c '
import json, sys
try:
    vecs = json.load(sys.stdin)
    vec = vecs[0]
    assert isinstance(vec, list) and len(vec) > 0, "empty vector"
    assert all(isinstance(x, (int, float)) and not isinstance(x, bool) for x in vec), \
        "non-numeric components"
except Exception as exc:
    print(f"bad embed response: {exc}")
    sys.exit(1)
print(len(vec))
' <<<"$body")"; then
  pass
  printf '     -> embedding dim: %s\n' "$dim"
else
  fail "${dim:-${body:0:200}}"
fi

# --- c. Gateway authn --------------------------------------------------------

begin "gateway: GET /healthz -> 200"
if code="$(http_code "${GATEWAY_URL}/healthz")" && [ "$code" = "200" ]; then
  pass
else
  fail "expected 200, got $code"
fi

begin "gateway: GET /v1/me without token -> 401"
if code="$(http_code "${GATEWAY_URL}/v1/me")" && [ "$code" = "401" ]; then
  pass
else
  fail "expected 401, got $code"
fi

ALICE_TOKEN=""
ALICE_TENANT=""

begin "keycloak: password grant for alice"
if ALICE_TOKEN="$(fetch_token alice password123)"; then
  pass
else
  fail "$ALICE_TOKEN"
  ALICE_TOKEN=""
fi

begin "gateway: /v1/me as alice -> 200, tenant_id == token sub"
alice_sub=""
body=""
if [ -z "$ALICE_TOKEN" ]; then
  fail "skipped: no alice token"
elif body="$("${CURL[@]}" -H "Authorization: Bearer ${ALICE_TOKEN}" "${GATEWAY_URL}/v1/me" 2>&1)" &&
  ALICE_TENANT="$(json_field tenant_id <<<"$body")" && [ -n "$ALICE_TENANT" ] &&
  alice_sub="$(jwt_claim "$ALICE_TOKEN" sub)" && [ "$ALICE_TENANT" = "$alice_sub" ]; then
  pass
  printf '     -> alice tenant_id: %s\n' "$ALICE_TENANT"
else
  fail "tenant_id='${ALICE_TENANT:-}' sub='${alice_sub:-}' response: ${body:0:200}"
  ALICE_TENANT=""
fi

begin "gateway: /v1/me as bob -> different tenant_id than alice"
bob_tenant=""
body=""
if ! BOB_TOKEN="$(fetch_token bob password123)"; then
  fail "could not obtain bob token: $BOB_TOKEN"
elif [ -z "$ALICE_TENANT" ]; then
  fail "skipped: no alice tenant_id to compare against"
elif body="$("${CURL[@]}" -H "Authorization: Bearer ${BOB_TOKEN}" "${GATEWAY_URL}/v1/me" 2>&1)" &&
  bob_tenant="$(json_field tenant_id <<<"$body")" &&
  [ -n "$bob_tenant" ] && [ "$bob_tenant" != "$ALICE_TENANT" ]; then
  pass
  printf '     -> bob tenant_id: %s\n' "$bob_tenant"
else
  fail "bob tenant_id='${bob_tenant:-}' alice tenant_id='${ALICE_TENANT}' response: ${body:0:200}"
fi

# --- d. Tenant isolation probe (Vespa streaming groups) -----------------------

FED=0
begin "vespa: feed probe doc for ${SMOKE_TENANT_A}"
if out="$("${CURL[@]}" -X POST "$SMOKE_DOC_URL" \
  -H 'Content-Type: application/json' \
  -d '{
    "fields": {
      "doc_id": "smoke-1",
      "connector_id": "smoke",
      "type": "FILE",
      "title": "asker smoke probe xyzzyplugh",
      "body": "this body contains the rare token xyzzyplugh for isolation testing",
      "created_at": 1718000000
    }
  }' 2>&1)"; then
  FED=1
  pass
else
  fail "${out:0:300}"
fi

begin "vespa: query as ${SMOKE_TENANT_A} -> exactly 1 hit"
if [ "$FED" != "1" ]; then
  fail "skipped: feed failed"
elif err="$(wait_for_count "$SMOKE_TENANT_A" 1)"; then
  pass
else
  fail "$err"
fi

begin "vespa: same query as ${SMOKE_TENANT_B} -> 0 hits (isolation)"
if [ "$FED" != "1" ]; then
  fail "skipped: feed failed"
elif count="$(query_count "$SMOKE_TENANT_B")" && [ "$count" = "0" ]; then
  pass
else
  fail "expected 0 hits for ${SMOKE_TENANT_B}, got: $count"
fi

begin "vespa: delete probe doc"
if [ "$FED" != "1" ]; then
  fail "skipped: feed failed"
elif out="$("${CURL[@]}" -X DELETE "$SMOKE_DOC_URL" 2>&1)"; then
  pass
else
  fail "${out:0:300}"
fi

begin "vespa: query after delete -> 0 hits"
if [ "$FED" != "1" ]; then
  fail "skipped: feed failed"
elif err="$(wait_for_count "$SMOKE_TENANT_A" 0)"; then
  pass
else
  fail "$err"
fi

# --- e. Summary ----------------------------------------------------------------

echo
echo "== Summary =="
printf '  %-4s %-6s %s\n' "STEP" "STATUS" "CHECK"
for row in "${SUMMARY[@]}"; do
  IFS='|' read -r num status name <<<"$row"
  printf '  %-4s %-6s %s\n' "$num" "$status" "$name"
done
echo

if [ "$FAILED" -ne 0 ]; then
  echo "SMOKE: FAILED"
  exit 1
fi
echo "SMOKE: ALL ${STEP} CHECKS PASSED"
