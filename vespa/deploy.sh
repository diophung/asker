#!/usr/bin/env bash
# Deploys the Vespa application package in vespa/app to a running config server.
#
# Flow:
#   1. Wait for the config server health endpoint to report "up".
#   2. Stage a copy of vespa/app and substitute the @EMBEDDING_DIM@ and
#      @CLIP_DIM@ template tokens (in schemas etc.) with ${EMBEDDING_DIM} and
#      ${CLIP_DIM}.
#   3. Zip the staged copy (services.xml at archive root).
#   4. POST the zip to /application/v2/tenant/default/prepareandactivate.
#   5. Wait for the query/document-API container to report "up" (the container
#      port only comes alive after the first application activation).
#
# Idempotent: re-running with the same or a changed package is safe; Vespa
# simply prepares and activates a new session.
#
# Environment overrides:
#   VESPA_CFG_URL      config server base URL   (default http://localhost:19071)
#   VESPA_QUERY_URL    query container base URL (default http://localhost:8082)
#   WAIT_TIMEOUT_SECS  health-wait timeout per endpoint, seconds (default 180)
#   EMBEDDING_DIM      embedding vector dimensionality substituted for
#                      @EMBEDDING_DIM@ in the application package (default 1024,
#                      bge-m3; local dev .env uses 384). MUST match the TEI
#                      model's output dimension and the services' EMBEDDING_DIM.
#   CLIP_DIM           CLIP image/text vector dimensionality substituted for
#                      @CLIP_DIM@ in the application package (default 512,
#                      ViT-B/32; M3, ADR-013). MUST match the clip service's
#                      model output dimension and the services' CLIP_DIM.
set -euo pipefail

VESPA_CFG_URL="${VESPA_CFG_URL:-http://localhost:19071}"
VESPA_QUERY_URL="${VESPA_QUERY_URL:-http://localhost:8082}"
WAIT_TIMEOUT_SECS="${WAIT_TIMEOUT_SECS:-180}"
EMBEDDING_DIM="${EMBEDDING_DIM:-1024}"
CLIP_DIM="${CLIP_DIM:-512}"
# Query-container JVM heap, substituted into services.xml's @VESPA_CONTAINER_JVM@.
# Default = the committed small dev heap (so `make dev-up` is unchanged); the
# two-host deploy raises it on the 128GB PC via deploy/compose/.env.pc.
VESPA_CONTAINER_JVM="${VESPA_CONTAINER_JVM:--Xms256m -Xmx768m}"

if ! [[ "${EMBEDDING_DIM}" =~ ^[1-9][0-9]*$ ]]; then
    echo "ERROR: EMBEDDING_DIM must be a positive integer, got '${EMBEDDING_DIM}'" >&2
    exit 1
fi

if ! [[ "${CLIP_DIM}" =~ ^[1-9][0-9]*$ ]]; then
    echo "ERROR: CLIP_DIM must be a positive integer, got '${CLIP_DIM}'" >&2
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP_DIR="${SCRIPT_DIR}/app"

if [[ ! -f "${APP_DIR}/services.xml" ]]; then
    echo "ERROR: application package not found: ${APP_DIR}/services.xml is missing" >&2
    exit 1
fi

for tool in curl zip; do
    if ! command -v "${tool}" >/dev/null 2>&1; then
        echo "ERROR: required tool '${tool}' is not installed" >&2
        exit 1
    fi
done

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT
APP_ZIP="${TMP_DIR}/asker-vespa-app.zip"
RESPONSE_BODY="${TMP_DIR}/deploy-response.json"

# Polls a Vespa /state/v1/health endpoint until it reports {"status":{"code":"up"}}
# or WAIT_TIMEOUT_SECS elapses.
wait_for_health() {
    local url="$1" name="$2"
    local started elapsed
    started="$(date +%s)"
    echo "==> Waiting for ${name} at ${url} (timeout ${WAIT_TIMEOUT_SECS}s)"
    while true; do
        if curl -fsS --max-time 5 "${url}" 2>/dev/null \
            | grep -q '"code"[[:space:]]*:[[:space:]]*"up"'; then
            echo "==> ${name} is up"
            return 0
        fi
        elapsed=$(( $(date +%s) - started ))
        if (( elapsed >= WAIT_TIMEOUT_SECS )); then
            echo "ERROR: timed out after ${WAIT_TIMEOUT_SECS}s waiting for ${name} at ${url}" >&2
            return 1
        fi
        echo "    ... ${name} not up yet (${elapsed}s elapsed)"
        sleep 5
    done
}

wait_for_health "${VESPA_CFG_URL}/state/v1/health" "Vespa config server"

# Stage the package and substitute the @EMBEDDING_DIM@ and @CLIP_DIM@ template
# tokens. The committed package is deployed verbatim except for these
# substitutions; vespa/app itself is never modified.
# Guard the free-form JVM value: it is sed-substituted with a '|' delimiter (so
# '|'/'&' would corrupt the rewrite) INTO a double-quoted XML attribute
# (services.xml: <jvm options="..."/>), so '"', '<', '>' would produce malformed
# XML and a confusing late Vespa deploy failure. Legitimate -X/-D heap/property
# args never contain these. Reject early with a clear message instead.
if [[ "${VESPA_CONTAINER_JVM}" == *['|&"<>']* ]]; then
    echo "ERROR: VESPA_CONTAINER_JVM must not contain any of | & \" < >, got '${VESPA_CONTAINER_JVM}'" >&2
    exit 1
fi

STAGE_DIR="${TMP_DIR}/app"
echo "==> Staging application package from ${APP_DIR} (EMBEDDING_DIM=${EMBEDDING_DIM}, CLIP_DIM=${CLIP_DIM}, VESPA_CONTAINER_JVM='${VESPA_CONTAINER_JVM}')"
mkdir -p "${STAGE_DIR}"
cp -R "${APP_DIR}/." "${STAGE_DIR}/"
find "${STAGE_DIR}" -type f -print0 | while IFS= read -r -d '' file; do
    if grep -q '@EMBEDDING_DIM@\|@CLIP_DIM@\|@VESPA_CONTAINER_JVM@' "${file}"; then
        sed -e "s/@EMBEDDING_DIM@/${EMBEDDING_DIM}/g" \
            -e "s/@CLIP_DIM@/${CLIP_DIM}/g" \
            -e "s|@VESPA_CONTAINER_JVM@|${VESPA_CONTAINER_JVM}|g" "${file}" > "${file}.sub" \
            && mv "${file}.sub" "${file}"
    fi
done

echo "==> Zipping staged application package"
# Zip from inside the staged dir so services.xml sits at the archive root.
# Exclude dotfiles (e.g. .DS_Store) at any depth.
(cd "${STAGE_DIR}" && zip -q -r "${APP_ZIP}" . -x '.*' -x '*/.*')

DEPLOY_URL="${VESPA_CFG_URL}/application/v2/tenant/default/prepareandactivate"
echo "==> Deploying application package to ${DEPLOY_URL}"
http_code="$(curl -sS -o "${RESPONSE_BODY}" -w '%{http_code}' \
    -X POST \
    -H 'Content-Type: application/zip' \
    --data-binary "@${APP_ZIP}" \
    "${DEPLOY_URL}")"

if [[ "${http_code}" != 2* ]]; then
    echo "ERROR: deploy failed with HTTP ${http_code}; server response:" >&2
    cat "${RESPONSE_BODY}" >&2
    echo >&2
    exit 1
fi
echo "==> Deploy accepted (HTTP ${http_code})"

wait_for_health "${VESPA_QUERY_URL}/state/v1/health" "Vespa query container"

echo "==> Vespa application deployed and serving"
