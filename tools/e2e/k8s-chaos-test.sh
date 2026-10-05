#!/usr/bin/env bash
# Offline regression tests: only owned fake child processes and synthetic JSON.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=tools/e2e/k8s-chaos-lib.sh
source "$ROOT/tools/e2e/k8s-chaos-lib.sh"
TEST_DIR="$(mktemp -d "${TMPDIR:-/tmp}/asker-k8s-contracts.XXXXXX")"
declare -a OWNED=()
cleanup_test() {
  local pid
  for pid in "${OWNED[@]:-}"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
    [ -n "$pid" ] && wait "$pid" 2>/dev/null || true
  done
  rm -rf "$TEST_DIR"
}
trap cleanup_test EXIT

cat >"$TEST_DIR/fake-forward" <<'FAKE'
#!/usr/bin/env bash
set -eu
state="$1"
count=0
[ ! -f "$state/count" ] || count="$(cat "$state/count")"
count=$((count + 1))
printf '%s\n' "$count" >"$state/count"
printf '%s\n' "$$" >"$state/child"
if [ "$count" = 1 ] && [ -f "$state/first-exits" ]; then exit 1; fi
if [ -f "$state/ignore-term" ]; then trap '' TERM; else trap 'exit 0' TERM; fi
while true; do sleep 0.05; done
FAKE
chmod +x "$TEST_DIR/fake-forward"

await_count() {
  local directory="$1" expected="$2" count i
  for ((i=0; i<100; i++)); do
    count="$(cat "$directory/count" 2>/dev/null || true)"
    if [ "$count" = "$expected" ]; then return 0; fi
    sleep 0.05
  done
  echo "FAIL: child was not started/restarted" >&2
  return 1
}
assert_dead() {
  if kill -0 "$1" 2>/dev/null; then echo "FAIL: owned child survived cleanup" >&2; return 1; fi
}

mkdir "$TEST_DIR/recovery"
touch "$TEST_DIR/recovery/first-exits"
k8s_supervise_forward 15 "$TEST_DIR/fake-forward" "$TEST_DIR/recovery" 2>"$TEST_DIR/recovery.log" &
supervisor=$!
OWNED+=("$supervisor")
await_count "$TEST_DIR/recovery" 2
child="$(cat "$TEST_DIR/recovery/child")"
kill -0 "$child"
# The same recovery is required if chaos deletes the pod backing a forward.
kill "$child"
await_count "$TEST_DIR/recovery" 3
child="$(cat "$TEST_DIR/recovery/child")"
kill "$supervisor"
wait "$supervisor" 2>/dev/null || true
assert_dead "$supervisor"
assert_dead "$child"
echo "PASS: startup failure, later pod loss and supervisor cleanup"

mkdir "$TEST_DIR/deadline"
touch "$TEST_DIR/deadline/ignore-term"
k8s_supervise_forward 1 "$TEST_DIR/fake-forward" "$TEST_DIR/deadline" 2>"$TEST_DIR/deadline.log" &
supervisor=$!
OWNED+=("$supervisor")
await_count "$TEST_DIR/deadline" 1
child="$(cat "$TEST_DIR/deadline/child")"
if wait "$supervisor" 2>/dev/null; then echo "FAIL: lifetime expiration succeeded" >&2; exit 1; fi
assert_dead "$child"
grep -q 'lifetime expired' "$TEST_DIR/deadline.log"
echo "PASS: finite lifetime terminates even a TERM-ignoring child"

if (k8s_supervise_forward 0 "$TEST_DIR/fake-forward" "$TEST_DIR/deadline") 2>/dev/null; then
  echo "FAIL: unbounded lifetime accepted" >&2; exit 1
fi
echo "PASS: invalid lifetime rejected"

[[ "$(k8s_http_code bash -c 'printf 000; exit 7')" = 000 ]]
[[ "$(k8s_http_code bash -c 'printf 200')" = 200 ]]
[[ "$(k8s_http_code bash -c 'printf 503')" = 503 ]]
echo "PASS: failed curl yields one retryable 000 and HTTP codes are retained"

[[ "$(k8s_verified_tenant <<< '{"tenant_id":"alice-verified-sub"}')" = alice-verified-sub ]]
[[ "$(k8s_verified_tenant alice-verified-sub <<< '{"tenant_id":"alice-verified-sub"}')" = alice-verified-sub ]]
for body in '{}' '{"tenant_id":"../bob"}' '{"tenant_id":false}' '{"tenant_id":".."}' 'not JSON'; do
  if k8s_verified_tenant <<<"$body" >/dev/null; then
    echo "FAIL: malformed or unsafe verified tenant accepted" >&2; exit 1
  fi
done
if k8s_verified_tenant chaos-tenant <<< '{"tenant_id":"alice-verified-sub"}' >/dev/null; then
  echo "FAIL: caller assertion selected another tenant" >&2; exit 1
fi
echo "PASS: probe tenant comes from verified gateway identity, mismatches fail closed"

# Exercise the real runner's feed/auth/order/cleanup with fake commands. No
# sockets, cluster or existing local stack are accessed by either fake.
mkdir "$TEST_DIR/bin" "$TEST_DIR/runner"
cat >"$TEST_DIR/bin/kubectl" <<'FAKE'
#!/usr/bin/env bash
set -eu
case "$*" in
  *port-forward*)
    printf '%s\n' "$$" >>"$K8S_FAKE_STATE/pids"
    trap 'exit 0' TERM
    while true; do sleep 0.05; done ;;
  *'get pods'*) printf '%s\n' asker-query-fake asker-gateway-fake ;;
  *) exit 0 ;;
esac
FAKE
cat >"$TEST_DIR/bin/curl" <<'FAKE'
#!/usr/bin/env bash
set -eu
method=GET
output=""
write_code=0
url=""
while (( $# )); do
  case "$1" in
    -X) method="$2"; shift ;;
    -o) output="$2"; shift ;;
    -w) write_code=1; shift ;;
    http://*) url="$1" ;;
  esac
  shift
done
body='{"status":{"code":"up"}}'
case "$url" in
  */prepareandactivate) body='{"deployed":true}' ;;
  */openid-connect/token) body='{"access_token":"synthetic-ci-token"}' ;;
  */v1/me)
    touch "$K8S_FAKE_STATE/verified"
    body='{"tenant_id":"alice-verified-sub"}' ;;
  */document/v1/*)
    [ -f "$K8S_FAKE_STATE/verified" ] || exit 22
    [[ "$url" == */group/alice-verified-sub/chaos-1 ]] || exit 22
    printf '%s\n' "$method" >>"$K8S_FAKE_STATE/doc-operations"
    body='{}' ;;
  */v1/search) body='{"hits":[{"doc_id":"chaos-1"}],"total":1}' ;;
esac
if [ -n "$output" ]; then printf '%s\n' "$body" >"$output"; fi
if [ "$write_code" = 1 ]; then printf 200; elif [ -z "$output" ]; then printf '%s\n' "$body"; fi
FAKE
chmod +x "$TEST_DIR/bin/kubectl" "$TEST_DIR/bin/curl"
PATH="$TEST_DIR/bin:$PATH" KUBECTL="$TEST_DIR/bin/kubectl" K8S_FAKE_STATE="$TEST_DIR/runner" \
  CHAOS_REQUESTS=2 REQUEST_INTERVAL=0.01 PORT_FORWARD_TIMEOUT=30 \
  bash "$ROOT/tools/e2e/k8s-chaos.sh" >"$TEST_DIR/runner.log" 2>&1
grep -q 'K8S CHAOS: ALL' "$TEST_DIR/runner.log"
[[ "$(cat "$TEST_DIR/runner/doc-operations")" = $'POST\nDELETE' ]]
while read -r pid; do assert_dead "$pid"; done <"$TEST_DIR/runner/pids"
echo "PASS: real runner verifies tenant before upsert, preserves chaos gate and cleans all forwards"
