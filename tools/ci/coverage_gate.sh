#!/usr/bin/env bash
# coverage_gate.sh <coverage.out>
#
# Enforces per-package statement-coverage floors on a Go coverage profile:
#   platform/tenancy   == 100.0%
#   platform/config    >=  75.0%
#   platform/telemetry >=  75.0%
#
# Per-package numbers are computed by filtering the profile down to one
# package's files and reading the "total:" line of `go tool cover -func`,
# which weights by statement count (averaging per-function percentages
# would not).
set -euo pipefail

MODULE="github.com/asker/asker"

if [ $# -ne 1 ]; then
  echo "usage: $0 <coverage.out>" >&2
  exit 2
fi

# Resolve the profile to an absolute path, then run from the repo root so
# `go tool cover` can resolve module file paths.
profile="$1"
if [ ! -f "$profile" ]; then
  echo "error: coverage profile not found: $profile" >&2
  exit 2
fi
profile="$(cd "$(dirname "$profile")" && pwd)/$(basename "$profile")"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

# pkg_pct <pkg>: print the package's statement coverage (e.g. "87.5"), or
# "ABSENT" if the profile contains no entries for it. Matches only files
# directly in the package directory (one Go package per directory).
pkg_pct() {
  local pkg="$1" tmp lines
  tmp="$(mktemp)"
  head -n 1 "$profile" >"$tmp" # "mode:" line
  grep -E "^${MODULE}/${pkg}/[^/]+\.go:" "$profile" >>"$tmp" || true
  lines="$(wc -l <"$tmp" | tr -d '[:space:]')"
  if [ "$lines" -le 1 ]; then
    rm -f "$tmp"
    echo "ABSENT"
    return 0
  fi
  go tool cover -func="$tmp" | awk '$1 == "total:" { gsub(/%/, "", $NF); print $NF }'
  rm -f "$tmp"
}

# meets <pct> <op> <floor>: float comparison; "ABSENT" never passes.
meets() {
  local pct="$1" op="$2" floor="$3"
  if [ "$pct" = "ABSENT" ]; then
    return 1
  fi
  awk -v p="$pct" -v f="$floor" -v op="$op" 'BEGIN {
    if (op == "==") exit (p == f) ? 0 : 1
    exit (p >= f) ? 0 : 1
  }'
}

# Gated packages: "<pkg> <op> <floor>"
GATES=(
  "platform/tenancy == 100.0"
  "platform/config >= 75.0"
  "platform/telemetry >= 75.0"
)

violations=0
echo "Coverage gate (profile: $profile)"
echo
printf '%-22s %-12s %-10s %s\n' "PACKAGE" "REQUIRED" "ACTUAL" "STATUS"
for gate in "${GATES[@]}"; do
  read -r pkg op floor <<<"$gate"
  pct="$(pkg_pct "$pkg")"
  if [ "$pct" = "ABSENT" ]; then
    actual="absent"
    status="FAIL (no coverage data — package missing or untested)"
    violations=$((violations + 1))
  elif meets "$pct" "$op" "$floor"; then
    actual="${pct}%"
    status="OK"
  else
    actual="${pct}%"
    status="FAIL"
    violations=$((violations + 1))
  fi
  printf '%-22s %-12s %-10s %s\n' "$pkg" "${op} ${floor}%" "$actual" "$status"
done
echo

total="$(go tool cover -func="$profile" | awk '$1 == "total:" { print $NF }')"
echo "Overall statement coverage: ${total:-unknown}"

if [ "$violations" -gt 0 ]; then
  echo "coverage gate: FAILED (${violations} violation(s))" >&2
  exit 1
fi
echo "coverage gate: PASSED"
