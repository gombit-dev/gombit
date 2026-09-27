#!/usr/bin/env bash
# Tests for chaos-run.sh. Stubs `go` on PATH: a fake `go test -json` prints
# the seed line the suite prints first and, when told to, writes a failure
# report the way the suite does and exits 1. Checks the artifacts and the
# summary a maintainer reproduces a nightly failure from.
#
#   bash scripts/chaos-run_test.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

fail=0
note() { echo "FAIL: $*" >&2; fail=1; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin"
cat >"$work/bin/go" <<'FAKE'
#!/usr/bin/env bash
if [ "$1" = version ]; then echo "go version fake"; exit 0; fi
# Record what the suite would have seen.
env | grep '^CHAOS_' | sort >"$ENV_LOG" || true
if [ -n "${NO_SEED:-}" ]; then
  echo '{"Action":"output","Output":"# build failed\n"}'
  exit 2
fi
echo '{"Action":"output","Package":"github.com/gombit-dev/gombit/internal/chaos","Output":"CHAOS_SEED=4242\n"}'
if [ -n "${FAIL_ITERATION:-}" ]; then
  cat >"$CHAOS_REPORT_DIR/database_failure-boundary-iter-$FAIL_ITERATION.txt" <<REPORT
CHAOS FAILURE

scenario: database/failure-boundary
seed: 4242
iteration: $FAIL_ITERATION

replay:
  CHAOS_SEED=4242 CHAOS_SCENARIO=database/failure-boundary CHAOS_ITERATION=$FAIL_ITERATION make test-chaos
REPORT
  echo '{"Action":"fail"}'
  exit 1
fi
echo '{"Action":"pass"}'
FAKE
chmod +x "$work/bin/go"
export ENV_LOG="$work/env.log"

run() { PATH="$work/bin:$PATH" bash scripts/chaos-run.sh; }

# ---- a passing run: seed found, passed, exit 0 ----
a="$work/pass"
if ! out="$(CHAOS_ARTIFACTS="$a" run)"; then note "a passing run exited non-zero: $out"; fi
grep -q 'Seed: `4242`' "$a/summary.md" || note "the summary lacks the seed: $(cat "$a/summary.md")"
grep -q 'passed' "$a/summary.md" || note "the summary does not say passed"
[ -s "$a/test.json" ] || note "no test.json"

# ---- a failing run: exit 1, replay command in the summary and the step summary ----
a="$work/fail"
summary="$work/step-summary.md"
if out="$(CHAOS_ARTIFACTS="$a" FAIL_ITERATION=7 GITHUB_STEP_SUMMARY="$summary" run)"; then
  note "a failing run exited 0"
fi
want='CHAOS_SEED=4242 CHAOS_SCENARIO=database/failure-boundary CHAOS_ITERATION=7 make test-chaos'
grep -qF "$want" "$a/summary.md" || note "the summary lacks the replay command: $(cat "$a/summary.md")"
grep -qF "$want" "$summary" || note "the step summary lacks the replay command"
[ -f "$a/reports/database_failure-boundary-iter-7.txt" ] || note "the failure report is not in the artifacts"

# ---- replay commands come in iteration order ----
a="$work/order"
CHAOS_ARTIFACTS="$a" FAIL_ITERATION=14 run >/dev/null || true
FAIL_ITERATION=2 CHAOS_REPORT_DIR="$a/reports" PATH="$work/bin:$PATH" go test >/dev/null 2>&1 || true
CHAOS_ARTIFACTS="$a" FAIL_ITERATION=14 run >/dev/null || true
first="$(grep -m1 -oE 'CHAOS_ITERATION=[0-9]+' "$a/summary.md")"
[ "$first" = "CHAOS_ITERATION=2" ] || note "replays are not in iteration order: $(cat "$a/summary.md")"

# ---- empty workflow inputs are unset, not passed as empty; the DSN is masked ----
a="$work/inputs"
CHAOS_ARTIFACTS="$a" CHAOS_SEED= CHAOS_SCENARIO= CHAOS_ITERATIONS=5 CHAOS_POSTGRES_DSN='postgres://u:secret@h/db' run >/dev/null || true
if grep -qE '^CHAOS_(SEED|SCENARIO)=' "$ENV_LOG"; then
  note "empty inputs reached the suite: $(cat "$ENV_LOG")"
fi
grep -qx 'CHAOS_ITERATIONS=5' "$ENV_LOG" || note "CHAOS_ITERATIONS did not reach the suite: $(cat "$ENV_LOG")"
grep -qx "CHAOS_REPORT_DIR=$a/reports" "$ENV_LOG" || note "the suite was not pointed at the report directory: $(cat "$ENV_LOG")"
if grep -q secret "$a/environment.txt"; then
  note "the DSN leaked into environment.txt"
fi
grep -q 'CHAOS_POSTGRES_DSN=(set, masked)' "$a/environment.txt" || note "environment.txt does not record the DSN as set"

# ---- a run that never started: no seed, says so, fails ----
a="$work/broken"
if CHAOS_ARTIFACTS="$a" NO_SEED=1 run >/dev/null; then note "a run that never started exited 0"; fi
grep -q 'not found' "$a/summary.md" || note "the summary does not say the seed is missing"
grep -q 'the run itself failed' "$a/summary.md" || note "the summary does not say the run failed outside a scenario"

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "chaos-run.sh: ok"
