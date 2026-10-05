#!/usr/bin/env bash
# Exercise the actual M1 helpers with fake HTTP/time boundaries. No network,
# containers, models or documents are used or modified by this regression.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck disable=SC1091
source "$ROOT/tools/e2e/m1-e2e.sh"
GATEWAY_URL=http://gateway.invalid
KEYCLOAK_URL=http://keycloak.invalid
CLOCK="$TMP/clock"
printf 1000 >"$CLOCK"
SCENARIO=ready
EXPECT_MARKER=marker123
EXPECT_USER=alice
MBOX=alice-run@example.com
TARGET=expected-message
WRONG='{"hits":[{"doc_id":"other-doc","type":"EMAIL","score":1,"metadata":{"to":"alice-run@example.com","message_id":"other-message"}}],"total":1}'
FOREIGN='{"hits":[{"doc_id":"foreign-doc","type":"EMAIL","score":1,"metadata":{"to":"bob-run@example.com","message_id":"expected-message"}}],"total":1}'
READY='{"hits":[{"doc_id":"indexed-doc","title":"Expected marker123","snippet":"<hi>marker123</hi>","type":"EMAIL","score":1,"metadata":{"to":"alice-run@example.com","message_id":"expected-message"}}],"total":1}'
EMPTY='{"hits":[],"total":0}'
HYBRID='{"hits":[{"doc_id":"semantic-a","type":"EMAIL","score":2},{"doc_id":"semantic-b","type":"EMAIL","score":1}],"total":2}'

# File-backed fake clock/counters also work inside command substitutions.
date() {
  if [[ "$1" = +%s ]]; then cat "$CLOCK"; else command date "$@"; fi
}
sleep() { printf '%s' "$(( $(cat "$CLOCK") + $1 ))" >"$CLOCK"; }
next_count() {
  local path="$1" count=0
  [[ ! -f "$path" ]] || count="$(cat "$path")"
  count=$((count + 1))
  printf '%s' "$count" >"$path"
  printf '%s' "$count"
}

curl() {
  local output="" auth="" bypass=0 url="" username="" query="" mode="" types="" n code=200 response="$EMPTY"
  local participant="" from="" to=""
  while (( $# )); do
    case "$1" in
      -o) output="$2"; shift ;;
      -H)
        case "$2" in
          'Authorization: Bearer '*) auth="${2#Authorization: Bearer }" ;;
          'Cache-Control: no-cache') bypass=1 ;;
          'Content-Type: application/x-www-form-urlencoded') ;;
          *) echo 'unexpected header' >&2; return 1 ;;
        esac
        shift ;;
      --data-urlencode)
        case "$2" in
          username=*) username="${2#username=}" ;;
          q=*) query="${2#q=}" ;;
          mode=*) mode="${2#mode=}" ;;
          types=*) types="${2#types=}" ;;
          participant=*) participant="${2#participant=}" ;;
          from=*) from="${2#from=}" ;;
          to=*) to="${2#to=}" ;;
          limit=*|client_id=*|grant_type=*|password=*) ;;
          *) echo 'unexpected parameter' >&2; return 1 ;;
        esac
        shift ;;
      -w|--max-time|-X) shift ;;
      -s|-fsS|-G) ;;
      http://*) url="$1" ;;
      *) echo 'unexpected curl argument' >&2; return 1 ;;
    esac
    shift
  done
  if [[ "$url" = "$KEYCLOAK_URL/realms/asker/protocol/openid-connect/token" ]]; then
    [[ "$username" = alice || "$username" = bob || "$username" = carol ]] || return 1
    n="$(next_count "$TMP/grants-$username")"
    if [[ "$SCENARIO" = slow-grant ]]; then sleep 301; fi
    printf '{"access_token":"fake-%s-%s","expires_in":300}' "$username" "$n"
    return 0
  fi
  [[ "$url" = "$GATEWAY_URL/v1/search" && -n "$output" && "$bypass" = 1 ]] || return 1
  [[ "$auth" = "$(python3 "$TMP/token.py" get "$TMP/tokens/$EXPECT_USER.json" "$(date +%s)")" ]] || return 1
  [[ "$auth" = fake-"$EXPECT_USER"-* ]] || return 1
  n="$(next_count "$TMP/searches")"
  case "$SCENARIO" in
    auth-retry) [[ "$n" != 1 ]] || code=401 ;;
    auth-reject) code=401 ;;
    rate-retry) if [[ "$n" = 1 ]]; then sleep 300; code=429; fi ;;
    hybrid)
      [[ "$query" = tighter && "$mode" = hybrid && "$types" = EMAIL && "$participant" = smith && "$from" = start && "$to" = end ]] || return 1
      response="$HYBRID" ;;
    *)
      [[ "$query" = "\"$EXPECT_MARKER\"" && "$mode" = keyword ]] || return 1
      case "$SCENARIO" in
        positive) case "$n" in 1) response="$WRONG" ;; 2) response="$FOREIGN" ;; *) response="$READY" ;; esac ;;
        negative) case "$n" in 1) response="$READY" ;; 2) response="$WRONG" ;; *) response="$EMPTY" ;; esac ;;
        missing) response="$WRONG" ;;
        late) sleep 11; response="$READY" ;;
        degraded) response='{"hits":[],"total":0,"degraded":"vespa-unavailable"}' ;;
        isolated) [[ "$EXPECT_USER" = bob ]] || return 1; response="$EMPTY" ;;
        ready) response="$READY" ;;
        *) return 1 ;;
      esac ;;
  esac
  printf '%s' "$response" >"$output"
  printf '%s' "$code"
}

reset_searches() { rm -f "$TMP/searches"; }

# The cache survives command substitution; refresh is driven by elapsed time,
# before the first request near expiry, rather than a polling iteration count.
a="$(fetch_token alice)"
b="$(fetch_token alice)"
[[ "$a" = "$b" && "$(cat "$TMP/grants-alice")" = 1 ]]
printf 1269 >"$CLOCK"
[[ "$(fetch_token alice)" = "$a" ]]
printf 1270 >"$CLOCK"
[[ "$(fetch_token alice)" != "$a" && "$(cat "$TMP/grants-alice")" = 2 ]]
printf 1900 >"$CLOCK"
[[ "$(fetch_token alice)" = fake-alice-3 ]]
python3 - "$TMP/tokens/alice.json" <<'PY'
import os
import stat
import sys
assert stat.S_IMODE(os.stat(sys.argv[1]).st_mode) == 0o600
assert stat.S_IMODE(os.stat(os.path.dirname(sys.argv[1])).st_mode) == 0o700
PY
echo 'PASS: elapsed-time token refresh survives subshells; cache files are private'

# A provider lifetime must not extend a JWT's actual expiry. Slow/invalid
# grants fail without sending an expired bearer or printing token contents.
jwt="$(python3 - <<'PY'
import base64
import json
payload = base64.urlsafe_b64encode(json.dumps({'exp': 2005}).encode()).decode().rstrip('=')
print('header.' + payload + '.signature')
PY
)"
python3 "$TMP/token.py" save "$TMP/tokens/jwt.json" 1900 1900 \
  <<<"{\"access_token\":\"$jwt\",\"expires_in\":300}" >/dev/null
[[ "$(python3 "$TMP/token.py" get "$TMP/tokens/jwt.json" 1974)" = "$jwt" ]]
if python3 "$TMP/token.py" get "$TMP/tokens/jwt.json" 1975 >/dev/null 2>&1; then exit 1; fi
if python3 "$TMP/token.py" save "$TMP/tokens/invalid.json" 1900 1900 \
  <<<'{"access_token":"private-value","expires_in":"300"}' >"$TMP/invalid-log" 2>&1; then exit 1; fi
[[ ! -s "$TMP/invalid-log" && ! -f "$TMP/tokens/invalid.json" ]]
SCENARIO=slow-grant
if fetch_token carol >"$TMP/failed-token" 2>"$TMP/token-error"; then exit 1; fi
[[ ! -s "$TMP/failed-token" && ! -f "$TMP/tokens/carol.json" ]]
python3 - "$TMP/token-error" <<'PY'
import pathlib
import sys
error = pathlib.Path(sys.argv[1]).read_text()
assert 'fake-carol' not in error and 'private-value' not in error
PY
echo 'PASS: JWT expiry is authoritative; expired or malformed grants fail privately'

SCENARIO='auth-retry'
reset_searches
before="$(cat "$TMP/grants-alice")"
search_as alice q=test mode=hybrid limit=10
[[ "$(cat "$TMP/searches")" = 2 && "$(cat "$TMP/grants-alice")" = "$((before + 2))" ]]
SCENARIO='auth-reject'
reset_searches
if search_as alice q=test mode=hybrid limit=10 >/dev/null 2>&1; then exit 1; fi
[[ "$(cat "$TMP/searches")" = 2 ]]
SCENARIO=rate-retry
reset_searches
before="$(cat "$TMP/grants-alice")"
search_as alice q=test mode=hybrid limit=10
[[ "$(cat "$TMP/searches")" = 2 && "$(cat "$TMP/grants-alice")" = "$((before + 1))" ]]
echo 'PASS: 401 refresh is bounded and 429 retries recheck elapsed token lifetime'

SCENARIO=positive
reset_searches
wait_hits drain alice "$EXPECT_MARKER" "$MBOX" pos 30 "$TARGET" >"$TMP/poll-log"
[[ "$WAIT_OK" = 1 && "$WAIT_ELAPSED" = 6 && "$(cat "$TMP/searches")" = 3 ]]
[[ "$(xt target_field "$MBOX" "$TARGET" title)" = 'Expected marker123' ]]
SCENARIO=missing
reset_searches
wait_hits drain alice "$EXPECT_MARKER" "$MBOX" pos 6 "$TARGET" >"$TMP/poll-log"
[[ "$WAIT_OK" = 0 && "$(cat "$TMP/searches")" = 3 ]]
SCENARIO=late
reset_searches
wait_hits drain alice "$EXPECT_MARKER" "$MBOX" pos 10 "$TARGET" >"$TMP/poll-log"
[[ "$WAIT_OK" = 0 ]]
echo 'PASS: drain needs the expected message in its run mailbox within the budget'

SCENARIO=negative
reset_searches
wait_hits delete alice "$EXPECT_MARKER" "$MBOX" zero 30 "$TARGET" >"$TMP/poll-log"
[[ "$WAIT_OK" = 1 && "$WAIT_ELAPSED" = 6 && "$(cat "$TMP/searches")" = 3 ]]
SCENARIO=degraded
reset_searches
wait_hits delete alice "$EXPECT_MARKER" "$MBOX" zero 3 "$TARGET" >"$TMP/poll-log"
[[ "$WAIT_OK" = 0 ]]
echo 'PASS: deletion requires all exact marker hits to vanish; degraded empty results fail'

SCENARIO=isolated
EXPECT_USER=bob
reset_searches
search_marker_as bob "$EXPECT_MARKER" limit=50
[[ "$(xt count)" = 0 && "$(xt total)" = 0 ]]
if search_marker_as bob 'marker" OR anything' limit=50; then exit 1; fi
[[ "$(cat "$TMP/searches")" = 1 ]]
echo 'PASS: isolation uses strict literal keyword membership with verified user auth'

SCENARIO=hybrid
EXPECT_USER=alice
reset_searches
search_as alice q=tighter mode=hybrid types=EMAIL participant=smith from=start to=end limit=20
[[ "$(xt shape)" = '2 yes yes' && "$(xt degraded)" = '' ]]
echo 'PASS: hybrid ranking and mandatory type/participant/date parameters remain exercised'
