#!/usr/bin/env bash
# Asker M1 end-to-end exit test.
#
# THE M1 acceptance test (spec §Milestones, M1 exit): for three tenants it
# seeds synthetic Gmail mailboxes through the fake-gmail admin API, runs the
# full connector -> ingest -> enrich -> index pipeline, and asserts:
#   - correct, hybrid-ranked, filterable search results per tenant;
#   - pairwise tenant isolation (the sacred check);
#   - source-edit freshness (< 30 min budget, actual seconds reported);
#   - delete/tombstone propagation;
#   - the upload connector path;
#   - rate-limit sanity (no 5xx under a small burst).
#
# The suite is self-contained, idempotent, and safe to run against a SHARED
# dev stack: every mailbox, seed, and probe token embeds RUN_ID, and all
# tenant-data assertions are scoped to this run's markers (metadata from/to
# carries the run-unique mailbox), so foreign documents from other suites or
# previous runs never break it. It never restarts compose services.
#
# Scale knob: E2E_EMAIL_COUNT (default 10000) total emails across 3 tenants.
#
# Run from anywhere. Requires: bash, curl, python3. No jq.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
KEYCLOAK_URL="${KEYCLOAK_URL:-http://localhost:8081}"
# fake-gmail admin/API as reachable from THIS shell (host port mapping).
FAKE_GMAIL_URL="${FAKE_GMAIL_URL:-http://localhost:9400}"
# fake-gmail as reachable from INSIDE the compose network: this is the
# base_url the connector instances are configured with (ADR-008).
FAKE_GMAIL_INTERNAL_URL="${FAKE_GMAIL_INTERNAL_URL:-http://fake-gmail:9400}"
KC_ADMIN_USER="${KC_ADMIN_USER:-admin}"
KC_ADMIN_PASS="${KC_ADMIN_PASS:-admin}"

E2E_EMAIL_COUNT="${E2E_EMAIL_COUNT:-10000}"
PER_TENANT=$((E2E_EMAIL_COUNT / 3))
# Sync timeout scales with corpus size; the pipeline drain and freshness
# budgets are generous because the dev VM is small and the stack is shared.
E2E_SYNC_TIMEOUT="${E2E_SYNC_TIMEOUT:-$((300 + PER_TENANT * 3))}"
E2E_DRAIN_TIMEOUT="${E2E_DRAIN_TIMEOUT:-900}"
E2E_FRESHNESS_BUDGET="${E2E_FRESHNESS_BUDGET:-1800}"

if [ "$PER_TENANT" -lt 10 ]; then
  echo "E2E_EMAIL_COUNT=${E2E_EMAIL_COUNT} too small: need >= 10 emails per tenant" >&2
  exit 2
fi

RUN_ID="$(date +%s)"
USERS=(alice bob carol)
USER_PASS="password123"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/m1-e2e.XXXXXX")"

# Per-tenant state (parallel arrays indexed 0=alice 1=bob 2=carol).
MAILBOX=()      # run-unique fake-gmail mailbox
SEEDVAL=()      # deterministic per-tenant generator seed
ISO_TOKEN=()    # run+tenant-unique isolation marker token
ISO_ID=()       # message id of the injected isolation message
INSTANCE=()     # gateway connector instance id
TOKENS=()       # current OIDC access tokens
TENANT=()       # tenant_id from /v1/me
P0_ID=(); P0_SUBJ=(); P0_TS=(); P0_TOK=(); P0_CTOK=()  # probe for correctness checks
P1_ID=(); P1_TOK=()                                    # probe for the freshness edit
P2_ID=(); P2_TOK=()                                    # probe for the delete check
for i in 0 1 2; do
  MAILBOX[i]="${USERS[i]}-${RUN_ID}@example.com"
  SEEDVAL[i]=$((RUN_ID * 10 + i + 1))
  ISO_TOKEN[i]="iso${RUN_ID}${USERS[i]}"
  # Pre-seed the per-tenant arrays so that, under `set -u`, a step that fails
  # during setup leaves later steps to report a clean failure rather than
  # aborting the whole run on an unbound-variable error.
  TOKENS[i]=""; TENANT[i]=""; INSTANCE[i]=""
done

# Measured latencies (seconds), reported in the final summary.
DRAIN_SECS=("?" "?" "?")
FRESH_SECS="?"
DELETE_SECS="?"
UPLOAD_SECS="?"

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

# rfc3339 EPOCH_SECONDS: print the UTC RFC3339 rendering.
rfc3339() {
  python3 -c '
import datetime, sys
print(datetime.datetime.fromtimestamp(int(sys.argv[1]), datetime.timezone.utc)
      .strftime("%Y-%m-%dT%H:%M:%SZ"))
' "$1"
}

# fetch_token USER: print an access token via the dev password grant.
fetch_token() {
  local body tok
  body="$("${CURL[@]}" -X POST "${KEYCLOAK_URL}/realms/asker/protocol/openid-connect/token" \
    -H 'Content-Type: application/x-www-form-urlencoded' \
    --data-urlencode "client_id=asker-web" \
    --data-urlencode "grant_type=password" \
    --data-urlencode "username=$1" \
    --data-urlencode "password=${USER_PASS}" 2>&1)" || {
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

# fetch_admin_token: Keycloak master-realm admin token (dev console creds).
fetch_admin_token() {
  local body tok
  body="$("${CURL[@]}" -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
    -H 'Content-Type: application/x-www-form-urlencoded' \
    --data-urlencode "client_id=admin-cli" \
    --data-urlencode "grant_type=password" \
    --data-urlencode "username=${KC_ADMIN_USER}" \
    --data-urlencode "password=${KC_ADMIN_PASS}" 2>&1)" || {
    echo "admin token request failed: ${body:0:200}"
    return 1
  }
  tok="$(json_field access_token <<<"$body" || true)"
  if [ -z "$tok" ]; then
    echo "no access_token in admin response: ${body:0:200}"
    return 1
  fi
  printf '%s\n' "$tok"
}

# refresh_tokens: refetch all three user tokens (dev access tokens live 300s;
# several phases run longer than that).
refresh_tokens() {
  local i t
  for i in 0 1 2; do
    t="$(fetch_token "${USERS[i]}")" || {
      echo "refresh_tokens: ${USERS[i]}: $t" >&2
      return 1
    }
    TOKENS[i]="$t"
  done
}

# search_as TOKEN PARAM...: GET /v1/search with the given query params
# (PARAMs are "key=value", url-encoded by curl). Body lands in $TMP/search.json.
# Retries on 429 (the per-tenant rate limit is shared with concurrent suites).
search_as() {
  local token="$1" code attempt p
  shift
  local args=()
  for p in "$@"; do
    args+=(--data-urlencode "$p")
  done
  for attempt in 1 2 3 4 5; do
    code="$(curl -s -o "$TMP/search.json" -w '%{http_code}' --max-time 30 -G \
      -H "Authorization: Bearer ${token}" "${GATEWAY_URL}/v1/search" "${args[@]}")" || code="000"
    if [ "$code" = "200" ]; then
      return 0
    fi
    if [ "$code" = "429" ]; then
      sleep 3
      continue
    fi
    break
  done
  echo "search HTTP ${code}: $(head -c 200 "$TMP/search.json" 2>/dev/null || true)" >&2
  return 1
}

# xt OP [ARGS]: run a search-response extraction op against $TMP/search.json.
xt() {
  python3 "$TMP/extract.py" "$@" <"$TMP/search.json"
}

cat >"$TMP/extract.py" <<'PYEOF'
"""Assertions/extractions over a /v1/search response (stdin).

"Marker" ops scope to THIS run's documents: a hit counts as ours only when
the run-unique mailbox email appears in metadata from/to (every seeded,
injected, and edited fake-gmail message carries it). This is what makes the
suite immune to foreign documents on a shared stack.
"""
import json
import sys


def hits(d):
    return d.get("hits") or []


def marker(d, mb):
    mb = mb.lower()
    out = []
    for h in hits(d):
        md = h.get("metadata") or {}
        if mb in (md.get("from", "") + " " + md.get("to", "")).lower():
            out.append(h)
    return out


op = sys.argv[1]
d = json.load(sys.stdin)
if op == "count":  # global hit count
    print(len(hits(d)))
elif op == "total":
    print(d.get("total", 0))
elif op == "degraded":
    print(d.get("degraded", ""))
elif op == "marker_count":  # argv[2]=mailbox
    print(len(marker(d, sys.argv[2])))
elif op == "marker_field":  # argv[2]=mailbox argv[3]=field (or metadata.X)
    m = marker(d, sys.argv[2])
    if not m:
        print("")
        sys.exit(0)
    h, f = m[0], sys.argv[3]
    if f.startswith("metadata."):
        print((h.get("metadata") or {}).get(f[len("metadata."):], ""))
    else:
        print(h.get(f, ""))
elif op == "field":  # argv[2]=field of the first hit
    hs = hits(d)
    print(hs[0].get(sys.argv[2], "") if hs else "")
elif op == "shape":  # "<count> <all-EMAIL yes/no> <scores-descending yes/no>"
    hs = hits(d)
    ok_t = all(h.get("type") == "EMAIL" for h in hs)
    sc = [h.get("score", 0) for h in hs]
    desc = all(sc[i] >= sc[i + 1] - 1e-9 for i in range(len(sc) - 1))
    print(len(hs), "yes" if ok_t else "no", "yes" if desc else "no")
else:
    sys.exit("unknown op " + op)
PYEOF

cat >"$TMP/probe.py" <<'PYEOF'
"""Parse one fake-gmail users.messages.get JSON (stdin) into probe fields.

argv[1] is the owning mailbox. Prints tab-separated:
  id, subject, internalDate (epoch s), rare token (qzxNNNNN), contact token
where "contact token" is the surname token of the non-owner participant —
a single-token participant-filter value (the query service matches
participant filters as substrings of individual tokens).
"""
import base64
import json
import re
import sys

m = json.load(sys.stdin)
mb = sys.argv[1].lower()
payload = m.get("payload") or {}
hdr = {h["name"].lower(): h["value"] for h in (payload.get("headers") or [])}
data = (payload.get("body") or {}).get("data", "")
body = base64.urlsafe_b64decode(data + "=" * (-len(data) % 4)).decode("utf-8", "replace")
tok = re.findall(r"qzx\d{5}", body)
frm, to = hdr.get("from", ""), hdr.get("to", "")
contact = to if mb in frm.lower() else frm
local = contact.split("@", 1)[0]
ctok = local.split(".")[-1] if local else ""
ts = int(m.get("internalDate", "0")) // 1000
print("\t".join([m.get("id", ""), hdr.get("subject", ""), str(ts),
                 tok[0] if tok else "", ctok]))
PYEOF

cat >"$TMP/listids.py" <<'PYEOF'
"""Print message ids from a users.messages.list response (stdin), skipping
argv[1] (the injected isolation message) when given."""
import json
import sys

skip = sys.argv[1] if len(sys.argv) > 1 else ""
for r in json.load(sys.stdin).get("messages") or []:
    if r.get("id") and r["id"] != skip:
        print(r["id"])
PYEOF

cat >"$TMP/sync.py" <<'PYEOF'
"""Find instance argv[1] in a GET /v1/connectors response (stdin); print
"<phase> <docsEmitted> <lastError|->". protojson renders int64 as a string."""
import json
import sys

want = sys.argv[1]
for e in json.load(sys.stdin):
    if (e.get("instance") or {}).get("id") == want:
        sy = e.get("sync") or {}
        err = (sy.get("lastError") or "-").replace("\n", " ")[:120] or "-"
        print(sy.get("phase", "NONE"), int(sy.get("docsEmitted") or 0), err)
        break
else:
    print("MISSING 0 instance-not-found")
PYEOF

# gmail_get MAILBOX PATH: authenticated fake-gmail Gmail API GET.
gmail_get() {
  "${CURL[@]}" -H "Authorization: Bearer fake-gmail-token:$1" \
    "${FAKE_GMAIL_URL}/gmail/v1/users/me$2"
}

# wait_hits NAME USER QUERY MAILBOX WANT TIMEOUT: poll /v1/search as USER for
# QUERY until the hit count meets WANT ("pos": >=1, "zero": ==0). When MAILBOX
# is non-empty the count is marker-scoped to it; otherwise it is the global
# hit count (only safe for run-unique tokens). The varying limit busts the
# 60s query result cache (limit is part of the cache key) without changing
# which documents match. Sets WAIT_OK, WAIT_ELAPSED, WAIT_END, WAIT_LAST.
wait_hits() {
  local name="$1" user="$2" query="$3" mbox="$4" want="$5" timeout="$6"
  local start now elapsed=0 i=0 tok count lim met
  WAIT_OK=0
  WAIT_ELAPSED=0
  WAIT_END=0
  WAIT_LAST="?"
  start=$(date +%s)
  tok="$(fetch_token "$user")" || {
    echo "     .. ${name}: cannot fetch ${user} token: $tok"
    return 0
  }
  while :; do
    lim=$((30 + i % 70))
    count="?"
    if search_as "$tok" "q=${query}" "limit=${lim}" 2>/dev/null; then
      if [ -n "$mbox" ]; then
        count="$(xt marker_count "$mbox" 2>/dev/null || echo '?')"
      else
        count="$(xt count 2>/dev/null || echo '?')"
      fi
    fi
    WAIT_LAST="$count"
    met=0
    case "$want" in
      pos) [ "$count" != "?" ] && [ "$count" -ge 1 ] && met=1 ;;
      zero) [ "$count" = "0" ] && met=1 ;;
    esac
    now=$(date +%s)
    elapsed=$((now - start))
    if [ "$met" = "1" ]; then
      WAIT_OK=1
      WAIT_ELAPSED="$elapsed"
      WAIT_END="$now"
      echo "     .. ${name}: condition met after ${elapsed}s (hits=${count})"
      return 0
    fi
    if [ "$elapsed" -ge "$timeout" ]; then
      echo "     .. ${name}: TIMED OUT after ${elapsed}s (last hits=${count}, wanted ${want})"
      return 0
    fi
    i=$((i + 1))
    if [ $((i % 10)) -eq 0 ]; then
      echo "     .. ${name}: waiting (${elapsed}s elapsed, hits=${count}, want ${want})"
    fi
    if [ $((i % 60)) -eq 59 ]; then
      tok="$(fetch_token "$user" || true)" # tokens expire after 300s
    fi
    sleep 3
  done
}

cleanup() {
  # Best-effort: delete this run's connector instances. Mailboxes, seeds and
  # probe tokens are all run-scoped, so no other reset is needed.
  set +e
  local i t
  for i in 0 1 2; do
    [ -n "${INSTANCE[i]:-}" ] || continue
    t="$(fetch_token "${USERS[i]}" 2>/dev/null)" &&
      curl -s --max-time 30 -X DELETE -H "Authorization: Bearer ${t}" \
        "${GATEWAY_URL}/v1/connectors/${INSTANCE[i]}" >/dev/null 2>&1
  done
  rm -rf "$TMP"
}
trap cleanup EXIT

echo "== Asker M1 e2e test =="
echo "   gateway=${GATEWAY_URL} keycloak=${KEYCLOAK_URL} fake-gmail=${FAKE_GMAIL_URL}"
echo "   run_id=${RUN_ID} emails=${E2E_EMAIL_COUNT} (${PER_TENANT}/tenant)"
echo "   sync_timeout=${E2E_SYNC_TIMEOUT}s drain_timeout=${E2E_DRAIN_TIMEOUT}s freshness_budget=${E2E_FRESHNESS_BUDGET}s"
echo

# --- 1. Preflight --------------------------------------------------------------

begin "gateway: GET /healthz -> 200"
if code="$(http_code "${GATEWAY_URL}/healthz")" && [ "$code" = "200" ]; then
  pass
else
  fail "expected 200, got ${code}"
fi

begin "fake-gmail: GET /healthz -> 200"
if code="$(http_code "${FAKE_GMAIL_URL}/healthz")" && [ "$code" = "200" ]; then
  pass
else
  fail "expected 200, got ${code} (is the dev stack up?)"
fi

begin "keycloak: OIDC discovery has issuer"
if body="$("${CURL[@]}" "${KEYCLOAK_URL}/realms/asker/.well-known/openid-configuration" 2>&1)" &&
  issuer="$(json_field issuer <<<"$body")" && [ -n "$issuer" ]; then
  pass
else
  fail "missing issuer; response: ${body:0:200}"
fi

begin "keycloak: ensure user carol exists (admin API, idempotent)"
carol_payload='{"username":"carol","enabled":true,"email":"carol@example.com","emailVerified":true,"firstName":"Carol","lastName":"Asker","credentials":[{"type":"password","value":"password123","temporary":false}]}'
if ! admin_tok="$(fetch_admin_token)"; then
  fail "$admin_tok"
elif code="$(curl -s -o "$TMP/kc.json" -w '%{http_code}' --max-time 30 \
  -X POST "${KEYCLOAK_URL}/admin/realms/asker/users" \
  -H "Authorization: Bearer ${admin_tok}" -H 'Content-Type: application/json' \
  -d "$carol_payload")" && { [ "$code" = "201" ] || [ "$code" = "409" ]; }; then
  pass
  printf '     -> carol %s\n' "$([ "$code" = "201" ] && echo created || echo "already exists (409)")"
else
  fail "POST /admin/realms/asker/users -> ${code}: $(head -c 200 "$TMP/kc.json")"
fi

begin "keycloak/gateway: tokens for alice, bob, carol; distinct tenant_ids"
tenants_ok=1
diag=""
for i in 0 1 2; do
  if ! t="$(fetch_token "${USERS[i]}")"; then
    tenants_ok=0
    diag="${USERS[i]}: $t"
    break
  fi
  TOKENS[i]="$t"
  if ! body="$("${CURL[@]}" -H "Authorization: Bearer ${t}" "${GATEWAY_URL}/v1/me" 2>&1)" ||
    ! TENANT[i]="$(json_field tenant_id <<<"$body")" || [ -z "${TENANT[i]}" ]; then
    tenants_ok=0
    diag="${USERS[i]}: /v1/me failed: ${body:0:160}"
    break
  fi
done
if [ "$tenants_ok" = "1" ] &&
  [ "${TENANT[0]}" != "${TENANT[1]}" ] &&
  [ "${TENANT[0]}" != "${TENANT[2]}" ] &&
  [ "${TENANT[1]}" != "${TENANT[2]}" ]; then
  pass
  printf '     -> tenants: alice=%s bob=%s carol=%s\n' "${TENANT[0]}" "${TENANT[1]}" "${TENANT[2]}"
else
  fail "${diag:-tenant ids not distinct: ${TENANT[0]:-} ${TENANT[1]:-} ${TENANT[2]:-}}"
fi

begin "gateway: GET /v1/search without token -> 401"
if code="$(http_code -G "${GATEWAY_URL}/v1/search" --data-urlencode "q=test")" && [ "$code" = "401" ]; then
  pass
else
  fail "expected 401, got ${code}"
fi

# --- 2. Provision per-tenant mailboxes + connectors ----------------------------

for i in 0 1 2; do
  user="${USERS[i]}"
  mbox="${MAILBOX[i]}"

  begin "${user}: seed ${PER_TENANT} emails + isolation probe into ${mbox}"
  seed_ok=0
  diag=""
  if body="$("${CURL[@]}" -X POST "${FAKE_GMAIL_URL}/admin/users/${mbox}/seed" \
    -H 'Content-Type: application/json' \
    -d "{\"count\":${PER_TENANT},\"seed\":${SEEDVAL[i]}}" 2>&1)" &&
    seeded="$(json_field seeded <<<"$body")" && [ "$seeded" = "$PER_TENANT" ]; then
    # Inject the run+tenant-unique isolation message: rare seeded tokens
    # (qzxNNNNN) repeat across mailboxes by construction, so cross-tenant
    # zero-hit checks need a token that exists in exactly one tenant.
    if iso_body="$("${CURL[@]}" -X POST "${FAKE_GMAIL_URL}/admin/users/${mbox}/messages" \
      -H 'Content-Type: application/json' \
      -d "{\"subject\":\"Isolation probe ${ISO_TOKEN[i]}\",\"body\":\"Tenant isolation marker ${ISO_TOKEN[i]} for run ${RUN_ID}.\",\"from\":\"isolation.probe@example.com\",\"to\":\"${mbox}\"}" 2>&1)" &&
      ISO_ID[i]="$(json_field id <<<"$iso_body")" && [ -n "${ISO_ID[i]}" ]; then
      seed_ok=1
    else
      diag="isolation message inject failed: ${iso_body:0:200}"
    fi
  else
    diag="seed failed: ${body:0:200}"
  fi
  if [ "$seed_ok" = "1" ]; then
    pass
    printf '     -> seed=%s iso_token=%s\n' "${SEEDVAL[i]}" "${ISO_TOKEN[i]}"
  else
    fail "$diag"
  fi

  begin "${user}: pick 3 probe messages (id/subject/date/rare-token via Gmail API)"
  probe_ok=0
  diag=""
  if list="$(gmail_get "$mbox" "/messages?maxResults=10" 2>&1)" &&
    ids="$(python3 "$TMP/listids.py" "${ISO_ID[i]:-}" <<<"$list")" &&
    [ "$(wc -l <<<"$ids" | tr -d ' ')" -ge 3 ]; then
    probe_ok=1
    n=0
    while IFS= read -r mid && [ "$n" -lt 3 ]; do
      if ! msg="$(gmail_get "$mbox" "/messages/${mid}" 2>&1)" ||
        ! line="$(python3 "$TMP/probe.py" "$mbox" <<<"$msg")"; then
        probe_ok=0
        diag="messages.get ${mid} failed: ${msg:0:160}"
        break
      fi
      IFS=$'\t' read -r pid psubj pts ptok pctok <<<"$line"
      if [ -z "$ptok" ]; then
        probe_ok=0
        diag="no qzx rare token in body of message ${mid}"
        break
      fi
      case "$n" in
        0)
          P0_ID[i]="$pid" P0_SUBJ[i]="$psubj" P0_TS[i]="$pts"
          P0_TOK[i]="$ptok" P0_CTOK[i]="$pctok"
          ;;
        1) P1_ID[i]="$pid" P1_TOK[i]="$ptok" ;;
        2) P2_ID[i]="$pid" P2_TOK[i]="$ptok" ;;
      esac
      n=$((n + 1))
    done <<<"$ids"
  else
    diag="messages.list failed or <3 messages: ${list:0:160}"
  fi
  if [ "$probe_ok" = "1" ]; then
    pass
    printf '     -> probe0: token=%s participant-token=%s subject=%.40s\n' \
      "${P0_TOK[i]}" "${P0_CTOK[i]}" "${P0_SUBJ[i]}"
  else
    fail "$diag"
  fi

  begin "${user}: create gmail connector + PUT fake token"
  conn_ok=0
  diag=""
  create_payload="$(printf '{"connector_id":"gmail","display_name":"m1-e2e %s %s","config":{"base_url":"%s","user_email":"%s"}}' \
    "$user" "$RUN_ID" "$FAKE_GMAIL_INTERNAL_URL" "$mbox")"
  if body="$("${CURL[@]}" -X POST "${GATEWAY_URL}/v1/connectors" \
    -H "Authorization: Bearer ${TOKENS[i]}" -H 'Content-Type: application/json' \
    -d "$create_payload" 2>&1)" &&
    INSTANCE[i]="$(json_field id <<<"$body")" && [ -n "${INSTANCE[i]}" ]; then
    code="$(http_code -X PUT "${GATEWAY_URL}/v1/connectors/${INSTANCE[i]}/token" \
      -H "Authorization: Bearer ${TOKENS[i]}" -H 'Content-Type: application/json' \
      -d "{\"token\":\"fake-gmail-token:${mbox}\"}")"
    if [ "$code" = "204" ]; then
      conn_ok=1
    else
      diag="PUT token -> ${code}"
    fi
  else
    diag="create connector failed: ${body:0:200}"
  fi
  if [ "$conn_ok" = "1" ]; then
    pass
    printf '     -> instance=%s\n' "${INSTANCE[i]}"
  else
    fail "$diag"
  fi
done

# --- 3. Wait for full sync ------------------------------------------------------

TARGET_DOCS=$((PER_TENANT + 1)) # seeded corpus + the injected isolation message
echo
echo "-- waiting for full sync: ${TARGET_DOCS} docs emitted per tenant (timeout ${E2E_SYNC_TIMEOUT}s) --"
sync_ok=0
sync_diag=""
sync_start=$(date +%s)
iter=0
while :; do
  alldone=1
  statusline=""
  for i in 0 1 2; do
    [ -n "${INSTANCE[i]:-}" ] || {
      alldone=0
      statusline+="${USERS[i]}:no-instance "
      continue
    }
    if body="$(curl -fsS --max-time 30 -H "Authorization: Bearer ${TOKENS[i]}" \
      "${GATEWAY_URL}/v1/connectors" 2>/dev/null)"; then
      read -r phase docs lasterr <<<"$(python3 "$TMP/sync.py" "${INSTANCE[i]}" <<<"$body")"
    else
      phase="HTTP-ERR" docs=0 lasterr="-"
    fi
    statusline+="${USERS[i]}:${phase}:${docs}/${TARGET_DOCS} "
    if [ "$docs" -lt "$TARGET_DOCS" ]; then
      alldone=0
    fi
    sync_diag="${USERS[i]} phase=${phase} docs=${docs} last_error=${lasterr}"
  done
  elapsed=$(($(date +%s) - sync_start))
  if [ "$alldone" = "1" ]; then
    sync_ok=1
    echo "   sync complete after ${elapsed}s: ${statusline}"
    break
  fi
  if [ "$elapsed" -ge "$E2E_SYNC_TIMEOUT" ]; then
    echo "   sync TIMED OUT after ${elapsed}s: ${statusline}"
    break
  fi
  if [ $((iter % 6)) -eq 0 ]; then
    echo "   sync progress (${elapsed}s): ${statusline}"
  fi
  iter=$((iter + 1))
  if [ $((iter % 30)) -eq 0 ]; then
    refresh_tokens || true # access tokens live 300s
  fi
  sleep 5
done

begin "sync: all 3 connectors emitted >= ${TARGET_DOCS} docs"
if [ "$sync_ok" = "1" ]; then
  pass
else
  fail "timed out; last per-tenant state: ${sync_diag}"
fi

# --- 4. Pipeline drain: isolation tokens searchable per tenant ------------------

echo
echo "-- waiting for pipeline drain: per-tenant isolation token searchable --"
for i in 0 1 2; do
  user="${USERS[i]}"
  begin "${user}: seeded token '${ISO_TOKEN[i]}' searchable (pipeline drain)"
  wait_hits "drain-${user}" "$user" "${ISO_TOKEN[i]}" "${MAILBOX[i]}" pos "$E2E_DRAIN_TIMEOUT"
  if [ "$WAIT_OK" = "1" ]; then
    DRAIN_SECS[i]="$WAIT_ELAPSED"
    pass
    printf '     -> searchable after %ss\n' "$WAIT_ELAPSED"
  else
    fail "token not searchable within ${E2E_DRAIN_TIMEOUT}s (last hits=${WAIT_LAST})"
  fi
done

# --- 5. Correctness per tenant ---------------------------------------------------

refresh_tokens || true
for i in 0 1 2; do
  user="${USERS[i]}"
  mbox="${MAILBOX[i]}"

  begin "${user}: rare token '${P0_TOK[i]}' -> exactly 1 doc, title match, <hi> snippet"
  rare_ok=0
  diag=""
  if search_as "${TOKENS[i]}" "q=${P0_TOK[i]}" "limit=50"; then
    mcount="$(xt marker_count "$mbox")"
    mtitle="$(xt marker_field "$mbox" title)"
    msnip="$(xt marker_field "$mbox" snippet)"
    if [ "$mcount" = "1" ] && [ "$mtitle" = "${P0_SUBJ[i]}" ] &&
      [[ "$msnip" == *"<hi>"* ]]; then
      rare_ok=1
    else
      diag="marker hits=${mcount} (want 1); title='${mtitle}' (want '${P0_SUBJ[i]}'); snippet highlight=$([[ "$msnip" == *'<hi>'* ]] && echo yes || echo NO)"
    fi
  else
    diag="search failed"
  fi
  if [ "$rare_ok" = "1" ]; then
    pass
    printf '     -> snippet: %.80s\n' "$msnip"
  else
    fail "$diag"
  fi

  begin "${user}: common word 'tighter' -> multiple hits, all EMAIL, scores descending"
  # The full 10K-email corpus is still indexing asynchronously (TEI embeddings)
  # when this runs, so a tenant's 'tighter' matches can be landing one at a time
  # (seen in CI: one tenant at 1 hit while the others already had >=2). Poll until
  # the assertion holds or a bounded timeout, varying limit to bust the 60s
  # query-result cache.
  scount="?"; stypes="?"; sdesc="?"; common_ok=""; c21_start=$(date +%s); c21_n=0
  while :; do
    if search_as "${TOKENS[i]}" "q=tighter" "types=EMAIL" "limit=$((20 + c21_n % 30))" &&
      read -r scount stypes sdesc <<<"$(xt shape)" &&
      [ "$scount" -ge 2 ] && [ "$stypes" = "yes" ] && [ "$sdesc" = "yes" ]; then
      common_ok=1
      break
    fi
    [ "$(($(date +%s) - c21_start))" -ge "${COMMON_WORD_TIMEOUT:-300}" ] && break
    c21_n=$((c21_n + 1))
    sleep 5
  done
  if [ -n "$common_ok" ]; then
    pass
    printf '     -> %s hits, all EMAIL, descending scores\n' "$scount"
  else
    fail "hits=${scount:-?} all-EMAIL=${stypes:-?} descending=${sdesc:-?}"
  fi

  begin "${user}: mode=keyword and mode=hybrid both return results; hybrid not degraded"
  kw_count=""
  hy_count=""
  hy_degraded="(search failed)"
  if search_as "${TOKENS[i]}" "q=tighter" "mode=keyword" "limit=10"; then
    kw_count="$(xt count)"
  fi
  if search_as "${TOKENS[i]}" "q=tighter" "mode=hybrid" "limit=10"; then
    hy_count="$(xt count)"
    hy_degraded="$(xt degraded)"
  fi
  if [ -n "$kw_count" ] && [ "$kw_count" -ge 1 ] &&
    [ -n "$hy_count" ] && [ "$hy_count" -ge 1 ] && [ -z "$hy_degraded" ]; then
    pass
    printf '     -> keyword=%s hits, hybrid=%s hits, degraded=""\n' "$kw_count" "$hy_count"
  else
    fail "keyword hits=${kw_count:-?}, hybrid hits=${hy_count:-?}, degraded='${hy_degraded}'"
  fi

  begin "${user}: filters (types, participant, date) include and exclude correctly"
  f_ok=1
  diag=""
  ts="${P0_TS[i]}"
  day_before="$(rfc3339 $((ts - 86400)))"
  day_after="$(rfc3339 $((ts + 86400)))"
  # Each case: query the probe rare token with one filter; expect the
  # marker-scoped count (1 = our doc included, 0 = excluded).
  filter_cases=(
    "types=EMAIL include|1|types=EMAIL"
    "types=FILE exclude|0|types=FILE"
    "participant '${P0_CTOK[i]}' include|1|participant=${P0_CTOK[i]}"
    "participant bogus exclude|0|participant=zzznobody${RUN_ID}"
    "date window include|1|from=${day_before}|to=${day_after}"
    "date before exclude|0|to=${day_before}"
    "date after exclude|0|from=${day_after}"
  )
  for case_spec in "${filter_cases[@]}"; do
    IFS='|' read -r cname cwant cp1 cp2 <<<"$case_spec"
    params=("q=${P0_TOK[i]}" "limit=50" "$cp1")
    [ -n "${cp2:-}" ] && params+=("$cp2")
    if ! search_as "${TOKENS[i]}" "${params[@]}"; then
      f_ok=0
      diag="${cname}: search failed"
      break
    fi
    got="$(xt marker_count "$mbox")"
    if [ "$got" != "$cwant" ]; then
      f_ok=0
      diag="${cname}: marker hits=${got}, want ${cwant}"
      break
    fi
  done
  if [ "$f_ok" = "1" ]; then
    pass
    printf '     -> 7 filter cases (types/participant/date, include+exclude)\n'
  else
    fail "$diag"
  fi
done

# --- 6. Tenant isolation (the core check) ----------------------------------------

refresh_tokens || true
for i in 0 1 2; do
  for j in 0 1 2; do
    [ "$i" = "$j" ] && continue
    begin "isolation: ${USERS[i]}'s token '${ISO_TOKEN[i]}' invisible to ${USERS[j]} (0 hits)"
    if search_as "${TOKENS[j]}" "q=${ISO_TOKEN[i]}" "limit=50" &&
      iso_hits="$(xt count)" && iso_total="$(xt total)" &&
      [ "$iso_hits" = "0" ] && [ "$iso_total" = "0" ]; then
      pass
    else
      fail "hits=${iso_hits:-?} total=${iso_total:-?} (cross-tenant leakage!)"
    fi
  done
done

# --- 7. Freshness: source edit searchable ----------------------------------------

echo
echo "-- freshness: edit a message at the source, stopwatch until searchable --"
FRESH_TOKEN="fresh${RUN_ID}"
begin "freshness: alice's edited message searchable < ${E2E_FRESHNESS_BUDGET}s"
edit_start=$(date +%s)
if body="$("${CURL[@]}" -X PUT "${FAKE_GMAIL_URL}/admin/users/${MAILBOX[0]}/messages/${P1_ID[0]}" \
  -H 'Content-Type: application/json' \
  -d "{\"subject\":\"Edited subject ${FRESH_TOKEN}\",\"body\":\"Edited at the source. New tracking reference ${FRESH_TOKEN}.\"}" 2>&1)"; then
  echo "(edited, polling)"
  wait_hits "freshness" alice "$FRESH_TOKEN" "${MAILBOX[0]}" pos "$E2E_FRESHNESS_BUDGET"
  printf '     %s ... ' "verdict"
  if [ "$WAIT_OK" = "1" ]; then
    FRESH_SECS=$((WAIT_END - edit_start))
    etitle="$(xt marker_field "${MAILBOX[0]}" title)"
    if [ "$etitle" = "Edited subject ${FRESH_TOKEN}" ]; then
      pass
      printf '     -> edit -> searchable in %ss (budget %ss)\n' "$FRESH_SECS" "$E2E_FRESHNESS_BUDGET"
    else
      fail "new token found but title='${etitle}', want 'Edited subject ${FRESH_TOKEN}'"
    fi
  else
    fail "edited doc not searchable within ${E2E_FRESHNESS_BUDGET}s"
  fi
else
  fail "admin edit failed: ${body:0:200}"
fi

begin "freshness: old body token '${P1_TOK[0]}' no longer matches the edited doc"
wait_hits "old-version-gone" alice "${P1_TOK[0]}" "${MAILBOX[0]}" zero 120
if [ "$WAIT_OK" = "1" ]; then
  pass
  printf '     -> old version unsearchable after %ss\n' "$WAIT_ELAPSED"
else
  fail "old token still matches ${WAIT_LAST} of this run's docs after 120s"
fi

# --- 8. Delete / tombstone --------------------------------------------------------

echo
echo "-- delete: remove a message at the source, stopwatch until gone --"
begin "delete: alice's deleted message '${P2_TOK[0]}' -> 0 hits"
delete_start=$(date +%s)
code="$(http_code -X DELETE "${FAKE_GMAIL_URL}/admin/users/${MAILBOX[0]}/messages/${P2_ID[0]}")"
if [ "$code" = "204" ]; then
  echo "(deleted, polling)"
  wait_hits "delete" alice "${P2_TOK[0]}" "${MAILBOX[0]}" zero "$E2E_DRAIN_TIMEOUT"
  printf '     %s ... ' "verdict"
  if [ "$WAIT_OK" = "1" ]; then
    DELETE_SECS=$((WAIT_END - delete_start))
    pass
    printf '     -> delete -> unsearchable in %ss\n' "$DELETE_SECS"
  else
    fail "deleted doc still searchable after ${E2E_DRAIN_TIMEOUT}s (hits=${WAIT_LAST})"
  fi
else
  fail "admin DELETE -> ${code}"
fi

# --- 9. Upload connector -----------------------------------------------------------

UPLOAD_TOKEN="upl${RUN_ID}"
UPLOAD_TITLE="Upload probe ${UPLOAD_TOKEN}"
upload_doc_id=""
begin "upload: POST /v1/upload as alice -> 202, doc searchable as FILE"
upload_file="$TMP/asker-m1-${RUN_ID}.txt"
printf 'M1 e2e upload probe.\nUnique tracking reference %s.\nRun %s.\n' \
  "$UPLOAD_TOKEN" "$RUN_ID" >"$upload_file"
alice_tok="$(fetch_token alice)" || alice_tok="${TOKENS[0]}"
upload_start=$(date +%s)
code="$(curl -s -o "$TMP/upload.json" -w '%{http_code}' --max-time 60 \
  -X POST "${GATEWAY_URL}/v1/upload" -H "Authorization: Bearer ${alice_tok}" \
  -F "file=@${upload_file};type=text/plain" -F "title=${UPLOAD_TITLE}")"
if [ "$code" = "202" ] && upload_doc_id="$(json_field doc_id <"$TMP/upload.json")" &&
  [ -n "$upload_doc_id" ]; then
  echo "(accepted, polling)"
  wait_hits "upload" alice "$UPLOAD_TOKEN" "" pos 300
  printf '     %s ... ' "verdict"
  if [ "$WAIT_OK" = "1" ]; then
    UPLOAD_SECS=$((WAIT_END - upload_start))
    utype="$(xt field type)"
    udoc="$(xt field doc_id)"
    utitle="$(xt field title)"
    if [ "$utype" = "FILE" ] && [ "$udoc" = "$upload_doc_id" ] &&
      [ "$utitle" = "$UPLOAD_TITLE" ]; then
      pass
      printf '     -> doc_id=%.16s... type=FILE searchable in %ss\n' "$upload_doc_id" "$UPLOAD_SECS"
    else
      fail "hit type='${utype}' doc_id='${udoc}' title='${utitle}' (want FILE/${upload_doc_id}/'${UPLOAD_TITLE}')"
    fi
  else
    fail "uploaded doc not searchable within 300s"
  fi
else
  fail "POST /v1/upload -> ${code}: $(head -c 200 "$TMP/upload.json" 2>/dev/null)"
fi

begin "upload: bob cannot see alice's uploaded doc (0 hits)"
if bob_tok="$(fetch_token bob)" &&
  search_as "$bob_tok" "q=${UPLOAD_TOKEN}" "limit=50" &&
  up_hits="$(xt count)" && [ "$up_hits" = "0" ]; then
  pass
else
  fail "bob sees hits=${up_hits:-?} for '${UPLOAD_TOKEN}' (cross-tenant leakage!)"
fi

# --- 10. Rate-limit sanity ----------------------------------------------------------

begin "rate limit: 30-request burst as bob -> only 200/429, never 5xx"
burst_ok=1
burst_codes=""
bob_tok="${bob_tok:-${TOKENS[1]}}"
for _ in $(seq 1 30); do
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 -G \
    -H "Authorization: Bearer ${bob_tok}" "${GATEWAY_URL}/v1/search" \
    --data-urlencode "q=rlprobe${RUN_ID}" --data-urlencode "limit=1")"
  burst_codes+="${code} "
  case "$code" in
    200 | 429) ;;
    *) burst_ok=0 ;;
  esac
done
if [ "$burst_ok" = "1" ]; then
  pass
  printf '     -> codes: %s\n' "$(tr ' ' '\n' <<<"$burst_codes" | sort | uniq -c | tr '\n' ' ' | tr -s ' ')"
else
  fail "unexpected status in burst: ${burst_codes}"
fi

# --- Summary -------------------------------------------------------------------------

echo
echo "== Summary =="
printf '  %-4s %-6s %s\n' "STEP" "STATUS" "CHECK"
for row in "${SUMMARY[@]}"; do
  IFS='|' read -r num status name <<<"$row"
  printf '  %-4s %-6s %s\n' "$num" "$status" "$name"
done
echo
echo "== Measured latencies =="
printf '  pipeline drain (seed -> searchable): alice=%ss bob=%ss carol=%ss\n' \
  "${DRAIN_SECS[0]}" "${DRAIN_SECS[1]}" "${DRAIN_SECS[2]}"
printf '  freshness (source edit -> searchable): %ss (budget %ss)\n' "$FRESH_SECS" "$E2E_FRESHNESS_BUDGET"
printf '  delete (source delete -> unsearchable): %ss\n' "$DELETE_SECS"
printf '  upload (POST -> searchable): %ss\n' "$UPLOAD_SECS"
echo

if [ "$FAILED" -ne 0 ]; then
  echo "M1 E2E: FAILED (run_id=${RUN_ID}, ${E2E_EMAIL_COUNT} emails)"
  exit 1
fi
echo "M1 E2E: ALL ${STEP} CHECKS PASSED (run_id=${RUN_ID}, ${E2E_EMAIL_COUNT} emails)"
