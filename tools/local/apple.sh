#!/usr/bin/env bash
# Standalone Apple text-search lane; never touches the dev/two-host projects.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ACTION="${1:-status}"
PROFILE="${2:-apple48}"
case "$PROFILE" in apple48|apple64) ;; *) echo "Profile must be apple48 or apple64" >&2; exit 2 ;; esac
ENV_FILE="${ASKER_APPLE_ENV_FILE:-$ROOT/deploy/compose/.env.$PROFILE.example}"
if [ ! -f "$ENV_FILE" ]; then echo "Missing trusted profile file: $ENV_FILE" >&2; exit 1; fi
# A profile is trusted shell configuration, like the existing deploy .env files.
set -a
# shellcheck disable=SC1090
. "$ENV_FILE"
set +a
VENV="$ROOT/services/local-inference/.venv"
PY="$VENV/bin/python"
export ASKER_APPLE_STATE_DIR="$VENV/asker-state"
COMPOSE=(docker compose --project-name "$ASKER_APPLE_PROJECT" --env-file "$ENV_FILE" -f "$ROOT/deploy/compose/docker-compose.apple.yml")

usage() {
  cat <<'EOF'
Usage: tools/local/apple.sh ACTION [apple48|apple64]
  setup       Create a Python 3.12 venv and install pinned native dependencies.
  native-up   Start resident embeddings and the optional existing reranker.
              Starting fetches model weights if they are not cached.
  native-down Stop only native processes recorded by this launcher.
  build       Build the three Apple app images serially (no models).
  prepare     Generate isolated realm/nginx files without starting services.
  up          Build app images, then start native models and isolated search.
  deploy      Deploy the unchanged Vespa application into the Apple project.
  down        Stop the Apple containers/native models; retain indexed volumes.
  status      Show this project's containers and native model identities.
  config      Print the resolved Compose configuration without starting it.
ASKER_APPLE_ENV_FILE can name a trusted private .env.local profile override.
EOF
}

require_native() {
  if [ "$(uname -s)" != Darwin ] || [ "$(uname -m)" != arm64 ]; then
    echo "Native Apple inference requires macOS arm64." >&2; exit 1
  fi
  if [ ! -x "$PY" ]; then echo "Run '$0 setup $PROFILE' first." >&2; exit 1; fi
}

prepare() {
  mkdir -p "$ASKER_APPLE_STATE_DIR"
  "$PY" - "$ROOT" <<'PY'
import json, os, pathlib
root = pathlib.Path(__import__('sys').argv[1])
state = pathlib.Path(os.environ['ASKER_APPLE_STATE_DIR'])
realm = json.loads((root / 'deploy/compose/keycloak/realm-asker.json').read_text())
for client in realm['clients']:
    if client['clientId'] == 'asker-web':
        origins = ['http://' + host + ':' + os.environ['ASKER_APPLE_WEB_PORT']
                   for host in ('localhost', '127.0.0.1')]
        client['redirectUris'] = [origin + '/*' for origin in origins]
        client['webOrigins'] = origins
(state / 'realm-asker.json').write_text(json.dumps(realm, indent=2) + '\n')
nginx = (root / 'web/nginx.conf').read_text().replace('localhost:8081',
        'localhost:' + os.environ['ASKER_APPLE_KEYCLOAK_PORT'])
(state / 'nginx.conf').write_text(nginx)
PY
}

managed_pid() {
  local service="$1" module="$2" pid command
  [ -f "$ASKER_APPLE_STATE_DIR/$service.pid" ] || return 1
  if [ -f "$ASKER_APPLE_STATE_DIR/$service.launchd.plist" ]; then
    pid="$("$PY" "$ROOT/tools/local/native_service.py" pid "$service" --root "$ROOT")" || return 1
  else
    pid="$(cat "$ASKER_APPLE_STATE_DIR/$service.pid")"
  fi
  case "$pid" in ''|*[!0-9]*) return 1 ;; esac
  kill -0 "$pid" 2>/dev/null || return 1
  command="$(ps -p "$pid" -o command=)"
  case "$command" in *"$PY -m $module"*) echo "$pid" ;; *) return 1 ;; esac
}

wait_health() {
  local url="$1" service="$2" i
  for ((i=0; i<300; i++)); do
    if curl -fsS --max-time 2 "$url" >/dev/null 2>&1; then return 0; fi
    if ! managed_pid "$service" "$3" >/dev/null; then
      echo "$service exited; inspect $ASKER_APPLE_STATE_DIR/$service.log" >&2; return 1
    fi
    sleep 2
  done
  echo "$service is not ready; inspect $ASKER_APPLE_STATE_DIR/$service.log" >&2
  return 1
}

start_native() {
  require_native
  prepare
  if ! managed_pid embed local_inference.main >/dev/null; then
    check_port "$LOCAL_EMBED_PORT"
    "$PY" "$ROOT/tools/local/native_service.py" start embed --root "$ROOT"
  fi
  echo "Waiting for actual model load and encode warm-up (uncached weights may download)."
  wait_health "http://127.0.0.1:$LOCAL_EMBED_PORT/health" embed local_inference.main
  "$PY" - <<'PY'
import json, os, urllib.request
with urllib.request.urlopen('http://127.0.0.1:' + os.environ['LOCAL_EMBED_PORT'] + '/identity', timeout=3) as response:
    identity = json.load(response)
expected = {'model': os.environ['LOCAL_EMBED_MODEL'], 'dimension': int(os.environ['EMBEDDING_DIM']),
            'batchSize': int(os.environ['LOCAL_EMBED_BATCH_SIZE']), 'maxLength': int(os.environ['LOCAL_EMBED_MAX_LENGTH'])}
if os.environ.get('LOCAL_EMBED_REVISION'):
    expected['resolvedRevision'] = os.environ['LOCAL_EMBED_REVISION']
if os.environ['LOCAL_EMBED_DEVICE'] != 'auto':
    expected['device'] = os.environ['LOCAL_EMBED_DEVICE']
if any(identity.get(key) != value for key, value in expected.items()):
    raise SystemExit('Running native encoder differs from this profile; stop only the managed native process before switching.')
admission = identity.get('admission') or {}
if admission.get('maxWaiting') != int(os.environ.get('LOCAL_EMBED_MAX_QUEUE', '3')) or admission.get('maxWaitMs') != int(os.environ.get('LOCAL_EMBED_ADMISSION_WAIT_MS', '600')):
    raise SystemExit('Running native encoder admission differs from this profile; use native-down before switching.')
PY
  if [ "$ASKER_APPLE_RERANK_ENABLED" = true ]; then
    if ! managed_pid rerank reranker.main >/dev/null; then
      check_port "$ASKER_APPLE_RERANK_PORT"
      "$PY" "$ROOT/tools/local/native_service.py" start rerank --root "$ROOT"
    fi
    wait_health "http://127.0.0.1:$ASKER_APPLE_RERANK_PORT/health" rerank reranker.main
  fi
  # Process environment takes precedence over --env-file during Compose
  # interpolation. Preserve the declared corpus/version prefix and append the
  # actual loaded model recipe identities; never echo the private namespace.
  QUERY_CACHE_NAMESPACE="$(PYTHONPATH="$ROOT/services/local-inference" "$PY" - <<'PY'
import json, os, urllib.request
from local_inference.cache import bind_namespace, validate_reranker_identity
def identity(port):
    with urllib.request.urlopen('http://127.0.0.1:' + port + '/identity', timeout=3) as response:
        return json.load(response)
embedding = identity(os.environ['LOCAL_EMBED_PORT'])
reranker = identity(os.environ['ASKER_APPLE_RERANK_PORT']) if os.environ['ASKER_APPLE_RERANK_ENABLED'] == 'true' else None
if reranker is not None:
    validate_reranker_identity(reranker, model=os.environ['RERANKER_MODEL'],
        revision=os.environ.get('RERANKER_REVISION') or None,
        device=os.environ['RERANKER_DEVICE'], precision=os.environ['RERANKER_PRECISION'],
        batch_size=int(os.environ['RERANKER_BATCH_SIZE']), max_length=int(os.environ['RERANKER_MAX_LENGTH']))
    admission = reranker.get('admission') or {}
    if admission.get('maxWaiting') != int(os.environ.get('RERANKER_MAX_QUEUE', '3')) or admission.get('maxWaitMs') != int(os.environ.get('RERANKER_ADMISSION_WAIT_MS', '300')):
        raise SystemExit('Running reranker admission differs from this profile; use native-down before switching.')
print(bind_namespace(os.environ['QUERY_CACHE_NAMESPACE'], embedding, reranker))
PY
)"
  export QUERY_CACHE_NAMESPACE
  curl -fsS --max-time 3 "http://127.0.0.1:$LOCAL_EMBED_PORT/identity"
  echo
}

check_port() {
  "$PY" - "$1" <<'PY'
import socket, sys
try:
    with socket.socket() as probe:
        probe.bind(('127.0.0.1', int(sys.argv[1])))
except OSError:
    raise SystemExit('Port ' + sys.argv[1] + ' belongs to another process; it was not stopped or adopted.')
PY
}

stop_native() {
  local service module pid
  for service in embed rerank; do
    if [ -f "$ASKER_APPLE_STATE_DIR/$service.launchd.plist" ]; then
      "$PY" "$ROOT/tools/local/native_service.py" stop "$service" --root "$ROOT"
      echo "Stopped managed $service job"
      continue
    fi
    module=local_inference.main
    if [ "$service" = rerank ]; then module=reranker.main; fi
    if pid="$(managed_pid "$service" "$module")"; then
      kill "$pid"
      echo "Stopped managed $service process $pid"
    fi
    rm -f "$ASKER_APPLE_STATE_DIR/$service.pid"
  done
}

check_docker_room() {
  # Read-only check: do not resize/restart Docker Desktop or unrelated projects.
  local total stats project_ids
  total="$(docker info --format '{{.MemTotal}}')"
  stats="$(docker stats --no-stream --format '{{.ID}}|{{.MemUsage}}')"
  project_ids="$(docker ps --no-trunc -q --filter "label=com.docker.compose.project=$ASKER_APPLE_PROJECT")"
  "$PY" - "$total" "$stats" "$ASKER_APPLE_MIN_DOCKER_FREE_GB" "$project_ids" <<'PY'
import re, sys
total = int(sys.argv[1])
units = {'B': 1, 'KiB': 1024, 'MiB': 1024**2, 'GiB': 1024**3,
         'kB': 1000, 'MB': 1000**2, 'GB': 1000**3}
used = 0
project_ids = sys.argv[4].split()
for line in sys.argv[2].splitlines():
    container_id, _, memory = line.partition('|')
    # The lane's current containers are the footprint we are starting/reusing,
    # not additional competing workloads. Preserve other projects' full charge.
    if any(identity.startswith(container_id) for identity in project_ids):
        continue
    match = re.match(r'\s*([\d.]+)([A-Za-z]+)', memory)
    if match:
        used += float(match[1]) * units.get(match[2], 1)
available = (total - used) / 1024**3
minimum = float(sys.argv[3])
print(f'Docker observed room excluding this Apple project: {available:.1f} GiB; the lane requires {minimum:.1f} GiB.')
if available < minimum:
    raise SystemExit('Insufficient Docker room. Use native-up for native-only validation, or adjust Docker memory with human approval. Existing stacks were left running.')
PY
}

build_images() {
  for service in query gateway web; do "${COMPOSE[@]}" build "$service"; done
}

deploy() {
  VESPA_CFG_URL="http://127.0.0.1:$ASKER_APPLE_VESPA_CONFIG_PORT" \
  VESPA_QUERY_URL="http://127.0.0.1:$ASKER_APPLE_VESPA_PORT" \
    bash "$ROOT/vespa/deploy.sh"
}

case "$ACTION" in
  setup)
    command -v uv >/dev/null || { echo "Install uv before native setup." >&2; exit 1; }
    if [ ! -x "$PY" ]; then uv venv --python "${ASKER_APPLE_PYTHON:-3.12}" "$VENV"; fi
    "$PY" -c 'import sys; assert sys.version_info[:2] in ((3, 12), (3, 13)), "Native venv requires Python 3.12 or 3.13"'
    uv pip install --python "$PY" -r "$ROOT/services/local-inference/requirements.txt"
    require_native
    prepare
    ;;
  native-up) start_native ;;
  native-down) stop_native ;;
  prepare) require_native; prepare ;;
  config) "${COMPOSE[@]}" config ;;
  build) build_images ;;
  up)
    require_native
    check_docker_room
    build_images
    start_native
    # Deploy Vespa before query readiness: its query endpoint is created by deploy.
    "${COMPOSE[@]}" up -d --wait --wait-timeout 300 vespa redis keycloak
    deploy
    "${COMPOSE[@]}" up -d --wait --wait-timeout 300 query gateway web
    echo "Asker Apple search: http://localhost:$ASKER_APPLE_WEB_PORT"
    ;;
  deploy) deploy ;;
  down) "${COMPOSE[@]}" down; stop_native ;;
  status)
    "${COMPOSE[@]}" ps || true
    curl -fsS --max-time 3 "http://127.0.0.1:$LOCAL_EMBED_PORT/identity" || true
    echo
    ;;
  -h|--help|help) usage ;;
  *) usage >&2; exit 2 ;;
esac
