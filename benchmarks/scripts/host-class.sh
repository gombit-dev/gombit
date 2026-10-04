#!/usr/bin/env bash
# Check BENCHMARK_HOST_CLASS before a benchmark measures anything (issue #291).
# The declaration only matters once the report renders, so a typo caught there
# would cost the whole run.
#
# This is the one shell copy of the allowed set. The Makefile's benchmark
# targets run this file, and run-crud-all.sh and footprint-all.sh source it and
# call check_host_class before their first `compose up`. The Go producers
# enforce metadata.HostClasses, and TestHostClassShellListMatchesGo fails if
# host_classes below drifts from it.
#
#   bash benchmarks/scripts/host-class.sh   # exit 0, or 2 with a message

# The empty entry is "unset": no declaration, which runs and gets the banner.
host_classes=("" "dedicated" "developer")

check_host_class() {
  local value="${BENCHMARK_HOST_CLASS-}" class
  for class in "${host_classes[@]}"; do
    [ "$value" = "$class" ] && return 0
  done
  echo "error: BENCHMARK_HOST_CLASS=\"$value\": must be \"dedicated\", \"developer\", or unset" >&2
  return 2
}

if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  check_host_class
fi
