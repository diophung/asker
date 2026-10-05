#!/usr/bin/env bash
# Pure/fake-process-testable boundaries for the kind chaos runner.

# A pre-activation Vespa :8080 connection can terminate kubectl port-forward.
# Keep one owned child at a time, retry its exit, and retain a finite lifetime.
k8s_supervise_forward() {
  local timeout="$1" child="" deadline
  shift
  if ! [[ "$timeout" =~ ^[1-9][0-9]*$ ]] || (( timeout > 3600 )) || (( $# == 0 )); then
    echo "Invalid port-forward lifetime or command" >&2
    exit 2
  fi
  deadline=$((SECONDS + timeout))
  stop_child() {
    if [ -n "$child" ]; then
      kill "$child" 2>/dev/null || true
      # Even a child ignoring TERM must not survive supervisor cleanup.
      for _ in 1 2 3 4 5 6 7 8 9 10; do
        kill -0 "$child" 2>/dev/null || break
        sleep 0.1
      done
      kill -KILL "$child" 2>/dev/null || true
      wait "$child" 2>/dev/null || true
      child=""
    fi
  }
  trap stop_child EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  while (( SECONDS < deadline )); do
    "$@" &
    child=$!
    while kill -0 "$child" 2>/dev/null && (( SECONDS < deadline )); do
      sleep 0.1
    done
    if (( SECONDS >= deadline )); then
      echo "Port-forward lifetime expired after ${timeout}s" >&2
      stop_child
      exit 1
    fi
    wait "$child" 2>/dev/null || true
    child=""
    sleep 1
  done
  echo "Port-forward lifetime expired after ${timeout}s" >&2
  stop_child
  exit 1
}

# curl emits its -w code even on transport failure. Do not append another 000:
# the resulting 000000 would bypass the chaos runner's connection-error retry.
k8s_http_code() {
  local code
  if code="$("$@")" && [[ "$code" =~ ^[1-5][0-9][0-9]$ ]]; then
    printf '%s\n' "$code"
  else
    echo "000"
  fi
}

# Input comes only from authenticated GET /v1/me. An optional operator assertion
# may constrain the verified tenant, but can never choose another user's group.
k8s_verified_tenant() {
  python3 -c '
import json, re, sys
try:
    tenant = json.load(sys.stdin).get("tenant_id")
    expected = sys.argv[1]
    if not isinstance(tenant, str) or not re.fullmatch(r"[A-Za-z0-9._-]{1,128}", tenant):
        raise ValueError()
    if tenant in (".", "..") or (expected and tenant != expected):
        raise ValueError()
except (ValueError, TypeError, AttributeError):
    sys.exit(1)
print(tenant)
' "${1:-}"
}
