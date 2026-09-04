#!/usr/bin/env bash
# Prefetches Go module dependencies with bounded retries so a transient
# module-proxy failure (e.g. "stream error: INTERNAL_ERROR" from
# proxy.golang.org/sum.golang.org) doesn't fail migration/conformance CI
# jobs whose generated Atlas GORM loader needs the full dependency graph
# already resolved before it runs (see issue #208). Only dependency
# acquisition is retried here — never `go test` — so a genuine test failure
# still fails immediately, with no retry hiding it.
#
#   bash scripts/ci-prefetch-go-modules.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

attempts=3
for attempt in $(seq 1 "$attempts"); do
  if go mod download all; then
    exit 0
  fi

  if [ "$attempt" -eq "$attempts" ]; then
    echo "failed to download Go dependencies after ${attempts} attempts" >&2
    exit 1
  fi

  sleep $((attempt * 5))
done
