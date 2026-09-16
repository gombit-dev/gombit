#!/usr/bin/env bash
# Tests for microbench-staleness.sh (PERF-14 / #293) without a real benchmark
# run. Each case builds a throwaway git repo with real commits touching the
# snapshot dir and/or the measured code, runs the script inside that repo, and
# asserts on its stdout AND that it exits 0 — the hard constraint is that this
# warning never gates the build.
#
#   bash benchmarks/scripts/microbench-staleness_test.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="$ROOT/benchmarks/scripts/microbench-staleness.sh"

fail=0
note() { echo "FAIL: $*" >&2; fail=1; }

SNAP_DIR="benchmarks/results/latest"

# new_repo prints the path to a fresh empty git repo.
new_repo() {
	local dir
	dir="$(mktemp -d)"
	git -C "$dir" init -q
	git -C "$dir" config user.email "t@example.com"
	git -C "$dir" config user.name "t"
	git -C "$dir" config commit.gpgsign false
	echo "$dir"
}

commit_code() { # $1=dir $2=msg
	mkdir -p "$1/framework"
	echo "package framework // $2" >"$1/framework/a.go"
	git -C "$1" add -A
	git -C "$1" commit -q -m "$2"
}

commit_snapshot() { # $1=dir $2=msg [$3=measured_at commit for metadata]
	mkdir -p "$1/$SNAP_DIR"
	echo "{}" >"$1/$SNAP_DIR/microbench.json"
	if [ -n "${3:-}" ]; then
		printf '{"groups":{"microbench":{"gombit":{"git_commit":"%s"}}}}\n' "$3" >"$1/$SNAP_DIR/metadata.json"
	else
		echo "{}" >"$1/$SNAP_DIR/metadata.json"
	fi
	git -C "$1" add -A
	git -C "$1" commit -q -m "$2"
}

run_case() { # $1=dir -> "<exit>|<stdout>"
	local dir="$1" out rc
	set +e
	out="$(cd "$dir" && bash "$SCRIPT" 2>&1)"
	rc=$?
	set -e
	printf '%s|%s' "$rc" "$out"
}

# ---- 1. stale: snapshot committed, THEN the code changed ----
dir="$(new_repo)"
commit_code "$dir" "c1: initial framework"
commit_snapshot "$dir" "c2: refresh snapshot"
snap="$(git -C "$dir" rev-parse HEAD)"
commit_code "$dir" "c3: later framework change"
code="$(git -C "$dir" rev-parse HEAD)"
res="$(run_case "$dir")"; rc="${res%%|*}"; out="${res#*|}"
[ "$rc" = "0" ] || note "stale case exited $rc, want 0 (must never gate)"
case "$out" in *"::warning::"*"stale"*) : ;; *) note "stale case emitted no staleness ::warning::; got: $out" ;; esac
case "$out" in *"make benchmark-micro benchmark-report"*) : ;; *) note "stale warning omits the remedy; got: $out" ;; esac
case "$out" in *"$snap"*) : ;; *) note "stale warning omits the snapshot commit $snap; got: $out" ;; esac
case "$out" in *"$code"*) : ;; *) note "stale warning omits the code commit $code; got: $out" ;; esac
rm -rf "$dir"

# ---- 2. fresh: code changed, THEN the snapshot was refreshed ----
dir="$(new_repo)"
commit_code "$dir" "c1: framework change"
commit_snapshot "$dir" "c2: refresh snapshot after"
res="$(run_case "$dir")"; rc="${res%%|*}"; out="${res#*|}"
[ "$rc" = "0" ] || note "fresh(after) case exited $rc, want 0"
case "$out" in *"::warning::"*) note "fresh(after) case emitted a warning; got: $out" ;; *) : ;; esac
rm -rf "$dir"

# ---- 3. fresh: one commit touches both code and snapshot ----
dir="$(new_repo)"
mkdir -p "$dir/framework" "$dir/$SNAP_DIR"
echo "package framework" >"$dir/framework/a.go"
echo "{}" >"$dir/$SNAP_DIR/microbench.json"
echo "{}" >"$dir/$SNAP_DIR/metadata.json"
git -C "$dir" add -A
git -C "$dir" commit -q -m "c1: code + snapshot together"
res="$(run_case "$dir")"; rc="${res%%|*}"; out="${res#*|}"
[ "$rc" = "0" ] || note "fresh(same-commit) case exited $rc, want 0"
case "$out" in *"::warning::"*) note "fresh(same-commit) case emitted a warning; got: $out" ;; *) : ;; esac
rm -rf "$dir"

# ---- 4. no committed snapshot -> honest skip, no warning, exit 0 ----
dir="$(new_repo)"
commit_code "$dir" "c1: framework only, no snapshot"
res="$(run_case "$dir")"; rc="${res%%|*}"; out="${res#*|}"
[ "$rc" = "0" ] || note "no-snapshot case exited $rc, want 0"
case "$out" in *"::warning::"*) note "no-snapshot case emitted a warning (false positive); got: $out" ;; *) : ;; esac
case "$out" in *"nothing to compare"*) : ;; *) note "no-snapshot case did not degrade honestly; got: $out" ;; esac
rm -rf "$dir"

# ---- 5. stale + recorded provenance commit surfaced in the message ----
dir="$(new_repo)"
commit_code "$dir" "c1: framework"
measured="$(git -C "$dir" rev-parse HEAD)"
commit_snapshot "$dir" "c2: refresh snapshot" "$measured"
commit_code "$dir" "c3: later framework change"
res="$(run_case "$dir")"; rc="${res%%|*}"; out="${res#*|}"
[ "$rc" = "0" ] || note "provenance case exited $rc, want 0"
case "$out" in *"::warning::"*) : ;; *) note "provenance case did not warn; got: $out" ;; esac
case "$out" in *"measured at ${measured}"*) : ;; *) note "provenance case did not surface the measured-at commit; got: $out" ;; esac
rm -rf "$dir"

if [ "$fail" -ne 0 ]; then
	echo "microbench-staleness_test: FAILED" >&2
	exit 1
fi
echo "microbench-staleness_test: PASS"
