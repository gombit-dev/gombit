#!/usr/bin/env bash
# Tests for chaos-run.sh. Stubs `go` on PATH: a fake `go test -json` prints
# the seed line the suite prints first and, when told to, writes a failure
# report per iteration in FAIL_ITERATIONS the way the suite does and exits 1.
# Checks the artifacts and the summary a maintainer reproduces a nightly
# failure from.
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
  echo '{"Action":"output","Output":"FAIL\tgithub.com/gombit-dev/gombit/internal/chaos [build failed]\n"}'
  echo 'internal/chaos/chaos_test.go:1:1: undefined: brokenThing' >&2
  exit 2
fi
echo '{"Action":"output","Package":"github.com/gombit-dev/gombit/internal/chaos","Output":"CHAOS_SEED=4242\n"}'
if [ -n "${FAIL_ITERATIONS:-}" ]; then
  pg=0
  if [ -n "${CHAOS_POSTGRES_DSN:-}" ]; then pg=1; fi
  for it in $FAIL_ITERATIONS; do
    cat >"$CHAOS_REPORT_DIR/database_failure-boundary-iter-$it.txt" <<REPORT
CHAOS FAILURE

scenario: database/failure-boundary
seed: 4242
iteration: $it

replay:
  CHAOS_POSTGRES=$pg CHAOS_SEED=4242 CHAOS_SCENARIO=database/failure-boundary CHAOS_ITERATION=$it make test-chaos
REPORT
  done
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

# ---- a failing run: exit 1, replay commands in the summary and the step
# summary, in iteration order (iter-2 before iter-14), no Postgres pinned ----
a="$work/fail"
summary="$work/step-summary.md"
if out="$(CHAOS_ARTIFACTS="$a" FAIL_ITERATIONS="14 2" GITHUB_STEP_SUMMARY="$summary" run)"; then
  note "a failing run exited 0"
fi
want='    CHAOS_POSTGRES=0 CHAOS_SEED=4242 CHAOS_SCENARIO=database/failure-boundary CHAOS_ITERATION=2 make test-chaos'
grep -qxF "$want" "$a/summary.md" || note "the summary lacks the replay command: $(cat "$a/summary.md")"
grep -qxF "$want" "$summary" || note "the step summary lacks the replay command"
[ -f "$a/reports/database_failure-boundary-iter-14.txt" ] || note "the failure report is not in the artifacts"
order="$(grep -oE 'CHAOS_ITERATION=[0-9]+' "$a/summary.md" | tr '\n' ' ')"
[ "$order" = "CHAOS_ITERATION=2 CHAOS_ITERATION=14 " ] || note "replays are not in iteration order: $order"
grep -qxF '    CHAOS_POSTGRES=0 CHAOS_ITERATIONS=20 CHAOS_SEED=4242 make test-chaos' "$a/summary.md" ||
  note "the whole-run replay does not pin Postgres and the iteration count: $(cat "$a/summary.md")"

# ---- the directory describes this run only: a previous run's reports go ----
a="$work/rerun"
CHAOS_ARTIFACTS="$a" FAIL_ITERATIONS=14 run >/dev/null || true
echo keep >"$a/unrelated.txt"
CHAOS_ARTIFACTS="$a" FAIL_ITERATIONS=3 run >/dev/null || true
if grep -q 'CHAOS_ITERATION=14' "$a/summary.md" || [ -e "$a/reports/database_failure-boundary-iter-14.txt" ]; then
  note "the previous run's failure is reported again: $(cat "$a/summary.md")"
fi
grep -q 'CHAOS_ITERATION=3' "$a/summary.md" || note "this run's failure is missing: $(cat "$a/summary.md")"
[ -f "$a/unrelated.txt" ] || note "a file the script does not own was removed"

# ---- with Postgres, every replay carries the DSN (quoted) and the run's
# iteration count; empty workflow inputs are unset, not passed as empty ----
a="$work/postgres"
dsn="postgres://u:p'w@h:5432/db?sslmode=disable&x=1"
CHAOS_ARTIFACTS="$a" CHAOS_SEED= CHAOS_SCENARIO= CHAOS_ITERATIONS=5 CHAOS_POSTGRES_DSN="$dsn" FAIL_ITERATIONS=4 run >/dev/null || true
if grep -qE '^CHAOS_(SEED|SCENARIO)=' "$ENV_LOG"; then
  note "empty inputs reached the suite: $(cat "$ENV_LOG")"
fi
grep -qx 'CHAOS_ITERATIONS=5' "$ENV_LOG" || note "CHAOS_ITERATIONS did not reach the suite: $(cat "$ENV_LOG")"
grep -qx "CHAOS_REPORT_DIR=$a/reports" "$ENV_LOG" || note "the suite was not pointed at the report directory: $(cat "$ENV_LOG")"
quoted="'postgres://u:p'\\''w@h:5432/db?sslmode=disable&x=1'"
grep -qxF "    CHAOS_POSTGRES_DSN=$quoted CHAOS_POSTGRES=1 CHAOS_ITERATIONS=5 CHAOS_SEED=4242 make test-chaos" "$a/summary.md" ||
  note "the whole-run replay lacks the DSN or the iteration count: $(cat "$a/summary.md")"
line="$(grep -F 'CHAOS_ITERATION=4 ' "$a/summary.md" | sed -E 's/^ +//')"
[ "$line" = "CHAOS_POSTGRES_DSN=$quoted CHAOS_POSTGRES=1 CHAOS_SEED=4242 CHAOS_SCENARIO=database/failure-boundary CHAOS_ITERATION=4 make test-chaos" ] ||
  note "the failure's replay lacks the DSN: $line"
# The command must hand the suite the same DSN back.
got="$(eval "${line%% make test-chaos}"' env' | grep '^CHAOS_POSTGRES_DSN=')"
[ "$got" = "CHAOS_POSTGRES_DSN=$dsn" ] || note "the replay does not restore the DSN: $got"
grep -qxF "CHAOS_POSTGRES_DSN=$dsn" "$a/environment.txt" || note "environment.txt does not record the DSN: $(cat "$a/environment.txt")"

# ---- a run that never started: no seed, says so, keeps go's stderr ----
a="$work/broken"
if CHAOS_ARTIFACTS="$a" NO_SEED=1 run >/dev/null; then note "a run that never started exited 0"; fi
grep -q 'not found' "$a/summary.md" || note "the summary does not say the seed is missing"
grep -q 'the run itself failed' "$a/summary.md" || note "the summary does not say the run failed outside a scenario"
grep -q 'undefined: brokenThing' "$a/go-stderr.txt" || note "go's stderr (the build error) was not kept"
grep -q 'go-stderr.txt' "$a/summary.md" || note "the summary does not point at go-stderr.txt"

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "chaos-run.sh: ok"
