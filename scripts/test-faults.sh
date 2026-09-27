#!/usr/bin/env bash
# The deterministic fault-injection suite: `make test-faults` (CHAOS-4,
# INV-7 — the required fault suite is reproducible and never depends on
# random scheduling). It runs, under the race detector:
#
#   - the internal/faulttest harness's own tests, whole;
#   - every TestFault_* test, in every package that declares one (found by
#     name, so a new fault test joins the suite without editing this file);
#   - with FAULT_POSTGRES_DSN and/or FAULT_MYSQL_DSN set, the same packages
#     again under the `integration` tag against those databases, one
#     package at a time (they share tables such as the auth ones), each with
#     its own -<pkg>.postgres-dsn / -<pkg>.mysql-dsn flags.
#
# Plain bash 3.2+ (macOS) and POSIX tools.
#
# FAULT_COUNT (default 1) repeats every test: FAULT_COUNT=100 is the flake
# soak. Nothing here reaches the internet or needs credentials beyond the
# test databases; plain `go test ./...` is unchanged by it.
#
#   bash scripts/test-faults.sh
#   FAULT_POSTGRES_DSN=postgres://... FAULT_COUNT=100 bash scripts/test-faults.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

count="${FAULT_COUNT:-1}"
case "$count" in
'' | *[!0-9]* | 0)
  echo "test-faults: FAULT_COUNT must be a positive integer, got '$count'" >&2
  exit 2
  ;;
esac

# run echoes a command (database DSNs masked) and runs it.
run() {
  local shown="$*"
  for dsn in "${FAULT_POSTGRES_DSN:-}" "${FAULT_MYSQL_DSN:-}"; do
    if [ -n "$dsn" ]; then
      shown="${shown//"$dsn"/<dsn>}"
    fi
  done
  echo "+ $shown"
  "$@"
}

harness=./internal/faulttest
# The packages of this module (`go list` leaves out testdata and nested
# modules such as the benchmark apps) that declare a TestFault_ test.
# Portable to macOS's bash 3.2 and BSD tools: no mapfile, no xargs -r.
# (Listed first, not piped, so a failing `go list` fails the script.)
dirs="$(go list -buildvcs=false -f '{{.Dir}}' ./...)"
pkgs=()
while IFS= read -r dir; do
  if grep -qsE '^func TestFault_' "$dir"/*_test.go; then
    pkgs+=("./${dir#"$ROOT"/}")
  fi
done <<<"$dirs"
if [ "${#pkgs[@]}" -eq 0 ]; then
  echo "test-faults: no TestFault_ tests found" >&2
  exit 1
fi

run go test -race -count="$count" "$harness"
run go test -race -count="$count" -run '^TestFault_' "${pkgs[@]}"

if [ -z "${FAULT_POSTGRES_DSN:-}" ] && [ -z "${FAULT_MYSQL_DSN:-}" ]; then
  exit 0
fi

# integrationFlag prints the prefix of pkg's -<prefix>.postgres-dsn flag, or
# nothing when the package has no database integration tests.
integrationFlag() {
  { grep -rhoE --include='*_test.go' 'flag\.String\("[a-z]+\.postgres-dsn"' "$1" 2>/dev/null || true; } |
    head -n1 | sed -E 's/.*"([a-z]+)\.postgres-dsn"/\1/'
}

for pkg in "$harness" "${pkgs[@]}"; do
  prefix="$(integrationFlag "$pkg")"
  [ -n "$prefix" ] || continue
  args=()
  if [ -n "${FAULT_POSTGRES_DSN:-}" ]; then
    args+=("-$prefix.postgres-dsn" "$FAULT_POSTGRES_DSN")
  fi
  if [ -n "${FAULT_MYSQL_DSN:-}" ]; then
    args+=("-$prefix.mysql-dsn" "$FAULT_MYSQL_DSN")
  fi
  filter='^TestFault_'
  if [ "$pkg" = "$harness" ]; then
    filter='.' # the harness's database tests run whole
  fi
  run go test -tags integration -race -count="$count" -run "$filter" "$pkg" "${args[@]}"
done
