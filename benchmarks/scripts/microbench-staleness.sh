#!/usr/bin/env bash
# PERF-14 (#293): warn — never gate — when the committed microbench snapshot
# predates the code it measures.
#
# `benchmark-report-drift` already verifies README ≡ f(snapshot); nothing
# verifies snapshot ≡ f(main). This closes that gap for the cheap-to-refresh
# microbench group: it detects when the measured code changed more recently than
# the snapshot was last refreshed and emits a GitHub `::warning::` naming the
# commits and the one-line remedy.
#
# HARD CONSTRAINT (BENCH-1 §3 non-goal: no perf-regression gates on noisy shared
# runners): this is a staleness signal about *provenance*, never a comparison of
# measured values, and it MUST exit 0 in every case — including when stale. It
# compares commits, not numbers. crud/footprint are deliberately out of scope:
# refreshing them is hours of Docker, so a per-PR warning nobody can act on would
# be noise; the microbench group refreshes in ~40s of Go.
#
# Mechanism (and a note on #293's literal spec). #293 proposed comparing
# metadata.json's groups.microbench.gombit.git_commit (the commit the bench ran
# at, added by #292) against the last commit touching the measured paths. But
# this repo squash-merges, which discards a PR's branch commits — so the recorded
# provenance commit is almost never reachable from main, and a pure
# provenance-commit ancestry check would degrade to "skip" on essentially every
# real PR (inert). Instead we key the same ancestor test on the commit that last
# touched the snapshot files (benchmarks/results/latest/), which IS in history:
# if that commit is a strict ancestor of the last commit touching the measured
# code, the code changed after the snapshot was refreshed -> stale. The recorded
# provenance commit is still surfaced in the warning when present.
#
# Operates on the current git repository ($PWD). Requires full history for the
# `git log -1 -- <paths>` lookups; the calling CI job checks out fetch-depth: 0.
# Env overrides (used by the test): MICROBENCH_METADATA, MICROBENCH_SNAPSHOT_DIR.
#
#   bash benchmarks/scripts/microbench-staleness.sh
#
# set -u/pipefail but never -e: a failure here must not fail the build.
set -uo pipefail

METADATA="${MICROBENCH_METADATA:-benchmarks/results/latest/metadata.json}"
SNAPSHOT_DIR="${MICROBENCH_SNAPSHOT_DIR:-benchmarks/results/latest}"

# Code whose changes move the microbench framework-tax numbers: the measured
# runtime, the harness, and the parser/schema.
PATHS=(framework benchmarks/micro benchmarks/internal/microbench)

warn() { printf '::warning::%s\n' "$*"; }
skip() { printf 'microbench-staleness: %s\n' "$*"; }

# The commit that last refreshed the committed snapshot, and the commit that last
# changed the measured code. Both are resolved from the checked-out history, so
# both survive squash-merge (unlike the recorded provenance commit).
snapshot_commit="$(git log -1 --format=%H -- "$SNAPSHOT_DIR" 2>/dev/null)"
code_commit="$(git log -1 --format=%H -- "${PATHS[@]}" 2>/dev/null)"

if [ -z "$snapshot_commit" ]; then
	skip "no committed snapshot under $SNAPSHOT_DIR (nothing to compare); skipping."
	exit 0
fi
if [ -z "$code_commit" ]; then
	skip "could not resolve the last commit touching ${PATHS[*]} (shallow clone? need fetch-depth: 0); skipping."
	exit 0
fi

# The commit the snapshot's microbench data was actually measured at (#292), used
# only to enrich the message. Absent/pre-#292/malformed metadata -> empty, and
# the message simply omits the clause (honest degrade, never a false "fresh").
measured_at=""
if command -v python3 >/dev/null 2>&1; then
	measured_at="$(
		python3 - "$METADATA" <<'PY' 2>/dev/null
import json, sys
try:
    with open(sys.argv[1]) as f:
        d = json.load(f)
except Exception:
    sys.exit(0)
mb = d.get("groups", {}).get("microbench", {})
if isinstance(mb, dict):
    entry = mb.get("gombit", {})
    if isinstance(entry, dict):
        print(entry.get("git_commit", "") or "")
PY
	)"
fi
measured_clause=""
[ -n "$measured_at" ] && measured_clause=" (microbench data measured at ${measured_at})"

# Equal commit: a single change touched both the code and the snapshot -> fresh.
if [ "$snapshot_commit" = "$code_commit" ]; then
	skip "snapshot was refreshed in the same commit as the measured code (${code_commit}); fresh."
	exit 0
fi

# `--is-ancestor A B` is true when A is an ancestor of B. Equality is handled
# above, so this fires only for a *strict* ancestor: the snapshot refresh is
# older than the code change -> stale. A snapshot at or ahead of the code is not
# an ancestor -> no warning.
if git merge-base --is-ancestor "$snapshot_commit" "$code_commit" 2>/dev/null; then
	warn "microbench snapshot is stale: last refreshed at ${snapshot_commit}${measured_clause}, but ${PATHS[*]} last changed at ${code_commit}. Refresh with: make benchmark-micro benchmark-report (~40s, no Docker)."
	exit 0
fi

skip "snapshot refresh ${snapshot_commit} is at or ahead of the last measured-code change (${code_commit}); fresh."
exit 0
