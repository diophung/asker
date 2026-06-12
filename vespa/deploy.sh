#!/usr/bin/env bash
# Deploys the Vespa application package in vespa/app to a running config server.
#
# Flow:
#   1. Wait for the config server health endpoint to report "up".
#   2. Zip vespa/app (services.xml at archive root).
#   3. POST the zip to /application/v2/tenant/default/prepareandactivate.
#   4. Wait for the query/document-API container to report "up" (the container
#      port only comes alive after the first application activation).
#
# Idempotent: re-running with the same or a changed package is safe; Vespa
# simply prepares and activates a new session.
#
# Environment overrides:
#   VESPA_CFG_URL      config server base URL   (default http://localhost:19071)
#   VESPA_QUERY_URL    query container base URL (default http://localhost:8082)
#   WAIT_TIMEOUT_SECS  health-wait timeout per endpoint, seconds (default 180)
set -euo pipefail

VESPA_CFG_URL="${VESPA_CFG_URL:-http://localhost:19071}"
VESPA_QUERY_URL="${VESPA_QUERY_URL:-http://localhost:8082}"
WAIT_TIMEOUT_SECS="${WAIT_TIMEOUT_SECS:-180}"

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

echo "==> Zipping application package from ${APP_DIR}"
# Zip from inside the app dir so services.xml sits at the archive root.
# Exclude dotfiles (e.g. .DS_Store) at any depth.
(cd "${APP_DIR}" && zip -q -r "${APP_ZIP}" . -x '.*' -x '*/.*')

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
