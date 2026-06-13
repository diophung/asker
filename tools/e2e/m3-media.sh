#!/usr/bin/env bash
# Asker M3 media end-to-end test — THE M3 exit criterion.
#
# Runs against a LIVE dev stack (make dev-up): connectors/upload -> hub ->
# ingest (pass-through) -> enrich (OCR/CLIP/Whisper/ffmpeg) -> index-writer ->
# Vespa, and the query service's text + CLIP arms. It uploads the committed,
# tiny media fixtures (tools/e2e/fixtures/media/, made by gen-media-fixtures.sh)
# as alice and asserts they become searchable with the right media metadata:
#
#   1. SPOKEN PHRASE -> VIDEO @ TIMESTAMP (the exit criterion): a speech .mp4 is
#      transcribed (whisper-tiny) into time-anchored asr chunks; searching a
#      rare word from the phrase returns the VIDEO doc with modality "asr" and a
#      plausible start_ms/end_ms inside the clip.
#   2. TEXT -> IMAGE (CLIP arm): a text query describing one image's color
#      ranks the matching image above the distractor, and the IMAGE hit carries
#      a thumbnail_key that GET /v1/media?key=... streams back as image bytes.
#   3. OCR: the word drawn on an image is found with modality "ocr".
#   4. TENANT ISOLATION: bob sees ZERO of alice's media for the same queries.
#
# The FIRST media doc triggers a whisper-tiny model download, so the ASR poll is
# generous (default 600s, env-overridable). House style mirrors smoke.sh /
# m1-e2e.sh: bash strict mode, numbered checks, PASS/FAIL summary, python3 for
# JSON (no jq), env-overridable URLs, a cleanup trap, non-zero exit on failure.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
KEYCLOAK_URL="${KEYCLOAK_URL:-http://localhost:8081}"
FIXTURE_DIR="${FIXTURE_DIR:-${REPO_ROOT}/tools/e2e/fixtures/media}"

# Scale knobs (env-overridable). The ASR timeout is large: the first media doc
# downloads whisper-tiny before it can transcribe anything.
ASR_TIMEOUT="${M3_ASR_TIMEOUT:-600}"
IMG_TIMEOUT="${M3_IMG_TIMEOUT:-300}"
POLL_INTERVAL="${M3_POLL_INTERVAL:-5}"

RUN_ID="$(date +%s)-$$"
TMP="$(mktemp -d)"
CURL=(curl -fsS --max-time 30)

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

note() { printf '     -> %s\n' "$*"; }

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

# fetch_token USER: print an access token via the dev password grant.
fetch_token() {
  local body tok
  body="$("${CURL[@]}" -X POST "${KEYCLOAK_URL}/realms/asker/protocol/openid-connect/token" \
    -H 'Content-Type: application/x-www-form-urlencoded' \
    --data-urlencode "client_id=asker-web" \
    --data-urlencode "grant_type=password" \
    --data-urlencode "username=$1" \
    --data-urlencode "password=${2:-password123}" 2>&1)" || {
    echo "token request failed for $1: ${body:0:200}" >&2
    return 1
  }
  tok="$(json_field access_token <<<"$body" || true)"
  if [ -z "$tok" ]; then
    echo "no access_token for $1: ${body:0:200}" >&2
    return 1
  fi
  printf '%s\n' "$tok"
}

# manifest FIELD: read a value from the fixtures manifest via a python path
# expression (e.g. "video.phrase", "video.duration_ms", "images.0.word").
manifest() {
  python3 -c '
import json, sys
with open(sys.argv[1]) as f:
    d = json.load(f)
for part in sys.argv[2].split("."):
    d = d[int(part)] if part.isdigit() else d[part]
print(d)
' "$FIXTURE_DIR/manifest.json" "$1"
}

# upload USER FILE TITLE: POST /v1/upload as USER; on 202 print the doc_id.
# Body lands in $TMP/upload.json for diagnostics.
upload() {
  local user="$1" file="$2" title="$3" tok code ct
  tok="$(fetch_token "$user")" || return 1
  case "$file" in
    *.mp4) ct="video/mp4" ;;
    *.png) ct="image/png" ;;
    *) ct="application/octet-stream" ;;
  esac
  code="$(curl -s -o "$TMP/upload.json" -w '%{http_code}' --max-time 120 \
    -X POST "${GATEWAY_URL}/v1/upload" \
    -H "Authorization: Bearer ${tok}" \
    -F "file=@${file};type=${ct}" -F "title=${title}")" || code="000"
  if [ "$code" != "202" ]; then
    echo "upload HTTP ${code}: $(head -c 200 "$TMP/upload.json" 2>/dev/null || true)" >&2
    return 1
  fi
  json_field doc_id <"$TMP/upload.json"
}

# search_as TOKEN PARAM...: GET /v1/search; body to $TMP/search.json. Retries
# on 429 (the per-tenant rate limit may be shared with concurrent suites).
search_as() {
  local token="$1" code attempt p
  shift
  local args=()
  for p in "$@"; do args+=(--data-urlencode "$p"); done
  for attempt in 1 2 3 4 5; do
    code="$(curl -s -o "$TMP/search.json" -w '%{http_code}' --max-time 30 -G \
      -H "Authorization: Bearer ${token}" "${GATEWAY_URL}/v1/search" "${args[@]}")" || code="000"
    [ "$code" = "200" ] && return 0
    [ "$code" = "429" ] && { sleep 3; continue; }
    break
  done
  echo "search HTTP ${code}: $(head -c 200 "$TMP/search.json" 2>/dev/null || true)" >&2
  return 1
}

# Extraction over a /v1/search response in $TMP/search.json. The media fields
# (start_ms/end_ms/modality/thumbnail_key) are the pinned REST hit shape.
cat >"$TMP/extract.py" <<'PYEOF'
"""Ops over a /v1/search response (stdin).

doc_hit DOC_ID FIELD : value of FIELD on the hit whose doc_id == DOC_ID
                       ("" if no such hit); FIELD may be metadata.X.
has_doc  DOC_ID      : "1" if a hit has that doc_id, else "0"
count                : number of hits
rank     DOC_A DOC_B : "above" if DOC_A precedes DOC_B in hit order,
                       "below" if after, "only_a"/"only_b"/"neither"
"""
import json
import sys


def hits(d):
    return d.get("hits") or []


def find(d, doc_id):
    for h in hits(d):
        if h.get("doc_id") == doc_id:
            return h
    return None


op = sys.argv[1]
d = json.load(sys.stdin)
if op == "count":
    print(len(hits(d)))
elif op == "has_doc":
    print("1" if find(d, sys.argv[2]) else "0")
elif op == "doc_hit":
    h = find(d, sys.argv[2])
    if h is None:
        print("")
        sys.exit(0)
    f = sys.argv[3]
    if f.startswith("metadata."):
        print((h.get("metadata") or {}).get(f[len("metadata."):], ""))
    else:
        v = h.get(f, "")
        print("" if v is None else v)
elif op == "rank":
    order = [h.get("doc_id") for h in hits(d)]
    a, b = sys.argv[2], sys.argv[3]
    ia = order.index(a) if a in order else -1
    ib = order.index(b) if b in order else -1
    if ia >= 0 and ib >= 0:
        print("above" if ia < ib else "below")
    elif ia >= 0:
        print("only_a")
    elif ib >= 0:
        print("only_b")
    else:
        print("neither")
else:
    sys.exit("unknown op " + op)
PYEOF

xt() { python3 "$TMP/extract.py" "$@" <"$TMP/search.json"; }

# wait_for_doc TOKEN DOC_ID QUERY TIMEOUT LABEL: poll /v1/search for QUERY until
# a hit with doc_id == DOC_ID appears (or TIMEOUT). The limit varies to bust the
# 60s result cache without changing matches. Returns 0 once found, 1 on timeout.
wait_for_doc() {
  local token="$1" doc_id="$2" query="$3" timeout="$4" label="$5"
  local start now elapsed=0 i=0 lim has
  start=$(date +%s)
  while :; do
    lim=$((20 + i % 50))
    has="0"
    if search_as "$token" "q=${query}" "limit=${lim}" 2>/dev/null; then
      has="$(xt has_doc "$doc_id" 2>/dev/null || echo 0)"
    fi
    [ "$has" = "1" ] && return 0
    now=$(date +%s)
    elapsed=$((now - start))
    [ "$elapsed" -ge "$timeout" ] && {
      echo "${label}: TIMED OUT after ${elapsed}s (doc not searchable)" >&2
      return 1
    }
    i=$((i + 1))
    if [ $((i % 6)) -eq 0 ]; then
      printf '     .. %s: waiting %ss (q="%s")\n' "$label" "$elapsed" "$query"
    fi
    # Tokens expire after 300s; refresh on long ASR polls.
    if [ $((i % 50)) -eq 49 ]; then
      token="$(fetch_token alice 2>/dev/null || echo "$token")"
    fi
    sleep "$POLL_INTERVAL"
  done
}

cleanup() {
  rm -rf "$TMP"
}
trap cleanup EXIT

echo "== Asker M3 media e2e =="
echo "   gateway=${GATEWAY_URL} keycloak=${KEYCLOAK_URL}"
echo "   fixtures=${FIXTURE_DIR}"
echo "   run_id=${RUN_ID} asr_timeout=${ASR_TIMEOUT}s img_timeout=${IMG_TIMEOUT}s poll=${POLL_INTERVAL}s"
echo

# --- 0. Fixtures present -------------------------------------------------------

begin "fixtures: speech.mp4 + blue.png + red.png + manifest.json present"
missing=""
for f in speech.mp4 blue.png red.png manifest.json; do
  [ -s "${FIXTURE_DIR}/${f}" ] || missing="${missing} ${f}"
done
if [ -z "$missing" ]; then
  pass
  note "total fixture bytes: $(find "$FIXTURE_DIR" -type f -exec ls -l {} + | awk '{s+=$5} END{print s}')"
else
  fail "missing/empty:${missing} (run tools/e2e/gen-media-fixtures.sh)"
fi

# Load known content from the manifest.
PHRASE="$(manifest video.phrase 2>/dev/null || echo '')"
ASR_WORD="$(manifest video.asr_words.0 2>/dev/null || echo 'roadmap')"
CLIP_MS="$(manifest video.duration_ms 2>/dev/null || echo '0')"
VIDEO_TITLE="m3-${RUN_ID}-$(manifest video.title 2>/dev/null | tr ' ' '-' || echo video)"
BLUE_WORD="$(manifest images.0.word 2>/dev/null || echo 'OCEAN')"
RED_WORD="$(manifest images.1.word 2>/dev/null || echo 'SUNSET')"
BLUE_TITLE="m3-${RUN_ID}-blue"
RED_TITLE="m3-${RUN_ID}-red"
note "phrase: \"${PHRASE}\"  asr-word: ${ASR_WORD}  clip: ${CLIP_MS}ms"
note "images: blue/${BLUE_WORD}  red/${RED_WORD}"

# --- 1. Preflight --------------------------------------------------------------

begin "gateway: GET /healthz -> 200"
if code="$(http_code "${GATEWAY_URL}/healthz")" && [ "$code" = "200" ]; then
  pass
else
  fail "expected 200, got ${code} (is the dev stack up? make dev-up)"
fi

ALICE_TOKEN=""
begin "keycloak/gateway: token for alice; /v1/me -> 200"
if ALICE_TOKEN="$(fetch_token alice)"; then
  if body="$("${CURL[@]}" -H "Authorization: Bearer ${ALICE_TOKEN}" "${GATEWAY_URL}/v1/me" 2>&1)" &&
    tid="$(json_field tenant_id <<<"$body")" && [ -n "$tid" ]; then
    pass
    note "alice tenant_id: ${tid}"
  else
    fail "/v1/me failed: ${body:0:200}"
    ALICE_TOKEN=""
  fi
else
  fail "$ALICE_TOKEN"
  ALICE_TOKEN=""
fi

begin "gateway: search arm reachable (q=preflight -> 200, hits array)"
if [ -z "$ALICE_TOKEN" ]; then
  fail "skipped: no alice token"
elif search_as "$ALICE_TOKEN" "q=preflight-${RUN_ID}" "limit=1" 2>/dev/null &&
  c="$(xt count 2>/dev/null)" && [ -n "$c" ]; then
  pass
  note "query service answered (hits=${c}); enrich+clip exercised once media flows"
else
  fail "search did not return a parseable response: $(head -c 200 "$TMP/search.json" 2>/dev/null)"
fi

# --- 2. SPOKEN PHRASE -> VIDEO @ TIMESTAMP (the exit criterion) ----------------

VIDEO_DOC=""
begin "upload: speech.mp4 as alice -> 202 {doc_id}"
if [ -z "$ALICE_TOKEN" ]; then
  fail "skipped: no alice token"
elif VIDEO_DOC="$(upload alice "${FIXTURE_DIR}/speech.mp4" "$VIDEO_TITLE")" && [ -n "$VIDEO_DOC" ]; then
  pass
  note "video doc_id: ${VIDEO_DOC}"
else
  fail "upload failed (see stderr)"
  VIDEO_DOC=""
fi

begin "asr: \"${ASR_WORD}\" -> the video doc becomes searchable (<=${ASR_TIMEOUT}s)"
ASR_FOUND=0
if [ -z "$VIDEO_DOC" ]; then
  fail "skipped: no video doc_id"
else
  asr_start=$(date +%s)
  if wait_for_doc "$ALICE_TOKEN" "$VIDEO_DOC" "$ASR_WORD" "$ASR_TIMEOUT" "asr"; then
    ASR_FOUND=1
    asr_secs=$(( $(date +%s) - asr_start ))
    pass
    note "video searchable for \"${ASR_WORD}\" in ${asr_secs}s (first doc downloads whisper-tiny)"
  else
    fail "video not searchable for \"${ASR_WORD}\" within ${ASR_TIMEOUT}s"
  fi
fi

begin "asr: matched hit is the VIDEO doc with type VIDEO + modality \"asr\""
if [ "$ASR_FOUND" != "1" ]; then
  fail "skipped: video not searchable"
else
  htype="$(xt doc_hit "$VIDEO_DOC" type)"
  hmod="$(xt doc_hit "$VIDEO_DOC" modality)"
  if [ "$htype" = "VIDEO" ] && [ "$hmod" = "asr" ]; then
    pass
    note "type=${htype} modality=${hmod}"
  else
    fail "type='${htype}' modality='${hmod}' (want VIDEO/asr)"
  fi
fi

begin "asr: start_ms/end_ms are a plausible segment inside the clip"
if [ "$ASR_FOUND" != "1" ]; then
  fail "skipped: video not searchable"
else
  start_ms="$(xt doc_hit "$VIDEO_DOC" start_ms)"
  end_ms="$(xt doc_hit "$VIDEO_DOC" end_ms)"
  # protojson renders int64 as a string; default to 0 when the field is absent.
  start_ms="${start_ms:-0}"
  end_ms="${end_ms:-0}"
  # Allow modest slack over the measured duration: whisper segment ends can
  # round just past the muxed clip length.
  slack=$(( CLIP_MS + 1500 ))
  if ok="$(python3 -c '
import sys
s, e, clip, slack = (int(x) for x in sys.argv[1:5])
# A single-phrase clip: the segment starts at/after 0 and within the clip,
# ends after it starts, and stays within the clip duration (plus slack).
print("yes" if (0 <= s < clip and e > s and e <= slack) else "no")
' "$start_ms" "$end_ms" "$CLIP_MS" "$slack")" && [ "$ok" = "yes" ]; then
    pass
    note "RETURNED TIMESTAMP: start_ms=${start_ms} end_ms=${end_ms} (clip=${CLIP_MS}ms)"
  else
    fail "start_ms=${start_ms} end_ms=${end_ms} not a plausible segment in a ${CLIP_MS}ms clip"
  fi
fi

# --- 3. TEXT -> IMAGE (CLIP arm) + thumbnail serving ---------------------------

BLUE_DOC=""
RED_DOC=""
begin "upload: blue.png + red.png as alice -> 202 each"
if [ -z "$ALICE_TOKEN" ]; then
  fail "skipped: no alice token"
elif BLUE_DOC="$(upload alice "${FIXTURE_DIR}/blue.png" "$BLUE_TITLE")" && [ -n "$BLUE_DOC" ] &&
  RED_DOC="$(upload alice "${FIXTURE_DIR}/red.png" "$RED_TITLE")" && [ -n "$RED_DOC" ]; then
  pass
  note "blue=${BLUE_DOC} red=${RED_DOC}"
else
  fail "image upload(s) failed (see stderr)"
fi

# Wait for both images to be indexed. OCR words are unique per image, so we poll
# each by its drawn word (the text/OCR arm), independent of the CLIP arm.
begin "image: both images become searchable (<=${IMG_TIMEOUT}s)"
IMG_READY=0
if [ -z "$BLUE_DOC" ] || [ -z "$RED_DOC" ]; then
  fail "skipped: missing image doc id(s)"
elif wait_for_doc "$ALICE_TOKEN" "$BLUE_DOC" "$BLUE_WORD" "$IMG_TIMEOUT" "blue-img" &&
  wait_for_doc "$ALICE_TOKEN" "$RED_DOC" "$RED_WORD" "$IMG_TIMEOUT" "red-img"; then
  IMG_READY=1
  pass
else
  fail "image(s) not searchable within ${IMG_TIMEOUT}s"
fi

begin "clip text->image: \"a photo of the color blue\" ranks blue above red"
if [ "$IMG_READY" != "1" ]; then
  fail "skipped: images not searchable"
elif search_as "$ALICE_TOKEN" "q=a photo of the color blue" "limit=20" "mode=hybrid" 2>/dev/null; then
  rank="$(xt rank "$BLUE_DOC" "$RED_DOC")"
  case "$rank" in
    above | only_a)
      pass
      note "blue image ranked ${rank} the red image (CLIP arm)"
      ;;
    *)
      fail "expected blue above red, got rank='${rank}' (count=$(xt count))"
      ;;
  esac
else
  fail "search failed: $(head -c 200 "$TMP/search.json" 2>/dev/null)"
fi

# The thumbnail_key is the pinned media field; fetch it through /v1/media.
THUMB_KEY=""
begin "image: the IMAGE hit carries a thumbnail_key"
if [ "$IMG_READY" != "1" ]; then
  fail "skipped: images not searchable"
else
  # Re-query by the blue image's OCR word so its hit is present, then read the
  # thumbnail_key off that hit.
  if search_as "$ALICE_TOKEN" "q=${BLUE_WORD}" "limit=20" 2>/dev/null; then
    htype="$(xt doc_hit "$BLUE_DOC" type)"
    THUMB_KEY="$(xt doc_hit "$BLUE_DOC" thumbnail_key)"
    if [ "$htype" = "IMAGE" ] && [ -n "$THUMB_KEY" ]; then
      pass
      note "type=${htype} thumbnail_key=${THUMB_KEY}"
    else
      fail "type='${htype}' thumbnail_key='${THUMB_KEY}' (want IMAGE + non-empty key)"
      THUMB_KEY=""
    fi
  else
    fail "search failed: $(head -c 200 "$TMP/search.json" 2>/dev/null)"
  fi
fi

begin "media: GET /v1/media?key=<thumb> as alice -> 200 image bytes"
if [ -z "$THUMB_KEY" ]; then
  fail "skipped: no thumbnail_key"
else
  code="$(curl -s -o "$TMP/thumb.bin" -w '%{http_code}' --max-time 30 \
    -H "Authorization: Bearer ${ALICE_TOKEN}" \
    -G "${GATEWAY_URL}/v1/media" --data-urlencode "key=${THUMB_KEY}")" || code="000"
  ct="$(file -b --mime-type "$TMP/thumb.bin" 2>/dev/null || echo unknown)"
  size="$(wc -c <"$TMP/thumb.bin" 2>/dev/null | tr -d ' ')"
  if [ "$code" = "200" ] && [ "${size:-0}" -gt 0 ] && case "$ct" in image/*) true ;; *) false ;; esac; then
    pass
    note "fetched ${size} bytes, mime=${ct}"
  else
    fail "HTTP ${code}, ${size:-0} bytes, mime=${ct} (want 200 + image/*)"
  fi
fi

begin "media: GET /v1/media without token -> 401"
if [ -z "$THUMB_KEY" ]; then
  fail "skipped: no thumbnail_key"
else
  code="$(http_code -G "${GATEWAY_URL}/v1/media" --data-urlencode "key=${THUMB_KEY}")"
  if [ "$code" = "401" ]; then
    pass
  else
    fail "expected 401, got ${code}"
  fi
fi

# --- 4. OCR --------------------------------------------------------------------

begin "ocr: \"${BLUE_WORD}\" matches the blue image with modality \"ocr\""
if [ "$IMG_READY" != "1" ]; then
  fail "skipped: images not searchable"
elif search_as "$ALICE_TOKEN" "q=${BLUE_WORD}" "limit=20" "mode=hybrid" 2>/dev/null; then
  has="$(xt has_doc "$BLUE_DOC")"
  hmod="$(xt doc_hit "$BLUE_DOC" modality)"
  if [ "$has" = "1" ] && [ "$hmod" = "ocr" ]; then
    pass
    note "blue image matched \"${BLUE_WORD}\" with modality=${hmod}"
  else
    fail "has_doc=${has} modality='${hmod}' (want 1/ocr)"
  fi
else
  fail "search failed: $(head -c 200 "$TMP/search.json" 2>/dev/null)"
fi

# --- 5. TENANT ISOLATION -------------------------------------------------------

begin "isolation: bob sees ZERO of alice's media (asr word, both OCR words)"
BOB_TOKEN=""
if BOB_TOKEN="$(fetch_token bob)"; then
  leaked=""
  for q in "$ASR_WORD" "$BLUE_WORD" "$RED_WORD"; do
    if search_as "$BOB_TOKEN" "q=${q}" "limit=50" 2>/dev/null; then
      for doc in "$VIDEO_DOC" "$BLUE_DOC" "$RED_DOC"; do
        [ -n "$doc" ] || continue
        if [ "$(xt has_doc "$doc")" = "1" ]; then
          leaked="${leaked} q='${q}'->${doc}"
        fi
      done
    else
      leaked="${leaked} search-as-bob-failed('${q}')"
    fi
  done
  if [ -z "$leaked" ]; then
    pass
    note "bob's searches returned none of alice's media docs"
  else
    fail "TENANT LEAK:${leaked}"
  fi
else
  fail "could not obtain bob token: $BOB_TOKEN"
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

if [ "$FAILED" -ne 0 ]; then
  echo "M3 MEDIA E2E: FAILED (run_id=${RUN_ID})"
  exit 1
fi
echo "M3 MEDIA E2E: ALL ${STEP} CHECKS PASSED (run_id=${RUN_ID})"
