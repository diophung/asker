#!/usr/bin/env bash
# Exercise the real search helper against a synthetic pre-index result cache.
# No network, containers or models are used by this regression.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/asker-media-contract.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT
GATEWAY_URL=http://fake.invalid

# Load only the actual helper so this test never runs the live media suite.
awk '/^search_as\(\) \{$/ {emit=1} emit {print} emit && /^\}$/ {exit}' \
  "$ROOT/tools/e2e/m3-media.sh" >"$TMP/helper.sh"
[[ -s "$TMP/helper.sh" ]]
# shellcheck disable=SC1091
source "$TMP/helper.sh"
fetch_token() {
  [[ "$1" = alice ]] || return 1
  printf '%s' synthetic-token
}

EMPTY='{"hits":[]}'
READY='{"hits":[{"doc_id":"synthetic-image","type":"IMAGE","thumbnail_key":"synthetic/thumb.jpg","modality":"ocr"}]}'

curl() {
  local output="" bypass=0 authorized=0 query="" limit="" url="" response cache
  while (( $# )); do
    case "$1" in
      -o) output="$2"; shift ;;
      -H)
        [[ "$2" != 'Cache-Control: no-cache' ]] || bypass=1
        [[ "$2" != 'Authorization: Bearer synthetic-token' ]] || authorized=1
        shift ;;
      --data-urlencode)
        case "$2" in
          q=*) query="${2#q=}" ;;
          limit=*) limit="${2#limit=}" ;;
          *) echo 'unexpected query parameter' >&2; return 1 ;;
        esac
        shift ;;
      -w|--max-time) shift ;;
      -s|-G) ;;
      http://*) url="$1" ;;
      *) echo 'unexpected curl argument' >&2; return 1 ;;
    esac
    shift
  done
  [[ "$authorized" = 1 && "$query" = TESTOCR && "$url" = "$GATEWAY_URL/v1/search" ]] || return 1
  [[ "$limit" = 20 || "$limit" = 21 ]] || return 1
  [[ -n "$output" ]] || return 1
  cache="$TMP/cache-$limit"
  if [[ "$bypass" = 0 && -f "$cache" ]]; then
    response="$(cat "$cache")"
  elif [[ -f "$TMP/indexed" ]]; then
    response="$READY"
  else
    response="$EMPTY"
  fi
  if [[ "$bypass" = 0 ]]; then printf '%s' "$response" >"$cache"; fi
  printf '%s' "$response" >"$output"
  printf '200'
}

# Reproduce the failure first: the cold limit=20 poll is cached; the next
# limit=21 poll sees the indexed image, but an immediate limit=20 assertion
# still sees the old empty response without cache bypass.
control_search() {
  curl -s -o "$TMP/control.json" -w '%{http_code}' --max-time 30 -G \
    -H 'Authorization: Bearer synthetic-token' "$GATEWAY_URL/v1/search" \
    --data-urlencode q=TESTOCR --data-urlencode "limit=$1" >/dev/null
}
control_search 20
[[ "$(cat "$TMP/control.json")" = "$EMPTY" ]]
touch "$TMP/indexed"
control_search 21
[[ "$(cat "$TMP/control.json")" = "$READY" ]]
control_search 20
[[ "$(cat "$TMP/control.json")" = "$EMPTY" ]]
echo 'PASS: control reproduces stale cold-poll response after successful indexing'

# The production helper must retain authentication/query parameters, observe
# the cold empty result, then obtain the image's OCR and thumbnail fields on
# both the ready poll and the original-limit post-index assertion.
rm "$TMP/indexed"
search_as alice q=TESTOCR limit=20
[[ "$(cat "$TMP/search.json")" = "$EMPTY" ]]
touch "$TMP/indexed"
search_as alice q=TESTOCR limit=21
[[ "$(cat "$TMP/search.json")" = "$READY" ]]
search_as alice q=TESTOCR limit=20
[[ "$(cat "$TMP/search.json")" = "$READY" ]]
# Bypassing lookup must also avoid overwriting the existing empty cache entry.
[[ "$(cat "$TMP/cache-20")" = "$EMPTY" ]]
echo 'PASS: real search helper gets fresh OCR/thumbnail data without changing cache entries'
