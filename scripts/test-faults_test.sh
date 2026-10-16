#!/usr/bin/env bash
# Tests for test-faults.sh. Stubs `go` on PATH (it logs its arguments and
# can be told to fail) so the suite's selection is checked hermetically: the
# harness runs whole, every package declaring a TestFault_ test is found,
# database runs happen only with a DSN and only for packages that define the
# flag (each with its own prefix), DSNs are masked in the output, FAULT_COUNT
# reaches every run, each run's -timeout grows with FAULT_COUNT (or is
# FAULT_TIMEOUT), and any failing `go test` fails the script, never
# silently.
#
#   bash scripts/test-faults_test.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

fail=0
note() { echo "FAIL: $*" >&2; fail=1; }

fakebin="$(mktemp -d)"
trap 'rm -rf "$fakebin" "${CALL_LOG:-}"' EXIT
# `go list` (package discovery) goes to the real toolchain; everything else
# is logged.
REAL_GO="$(command -v go)"
export REAL_GO
cat > "$fakebin/go" <<'FAKE'
#!/usr/bin/env bash
if [ "$1" = list ]; then
  if [ -n "${LIST_FAILS:-}" ]; then
    echo "go: list failed" >&2
    exit 1
  fi
  exec "$REAL_GO" "$@"
fi
echo "$*" >> "$CALL_LOG"
if [ -n "${GO_SLEEP:-}" ]; then
  sleep "$GO_SLEEP"
fi
if [ -n "${FAIL_MATCH:-}" ] && [[ "$*" == *"$FAIL_MATCH"* ]]; then
  echo "--- FAIL: TestFault_Something" >&2
  exit 1
fi
FAKE
chmod +x "$fakebin/go"

# suite runs the script with the fake go and prints its stdout; the calls
# land in $CALL_LOG, fresh for each run (set here, in the parent shell, as
# suite itself runs inside $(...)).
CALL_LOG=""
suite() {
  PATH="$fakebin:$PATH" bash scripts/test-faults.sh
}
newlog() {
  [ -z "$CALL_LOG" ] || rm -f "$CALL_LOG"
  CALL_LOG="$(mktemp)"
  export CALL_LOG
}
unset FAULT_POSTGRES_DSN FAULT_MYSQL_DSN FAULT_COUNT FAULT_TIMEOUT

# ---- SQLite only: the harness whole, then every TestFault_ package ----
newlog
out="$(suite)" || note "suite without DSNs failed: $out"
calls="$(cat "$CALL_LOG")"
grep -qx 'test -race -count=1 -timeout=600s -run . ./internal/faulttest' <<<"$calls" || note "the harness does not run whole: $calls"
for pkg in ./framework ./auth ./admin ./cli ./dev; do
  grep -E '^test -race -count=1 -timeout=600s -run \^TestFault_ ' <<<"$calls" | grep -qw -- "$pkg" ||
    note "TestFault_ package $pkg not selected: $calls"
done
if grep -q 'integration' <<<"$calls"; then
  note "database runs without a DSN: $calls"
fi

# ---- with DSNs: one database run per package that defines the flags ----
pg='postgres://u:secret-pg@127.0.0.1:5432/db?sslmode=disable'
my='u:secret-my@tcp(127.0.0.1:3306)/db?parseTime=true'
newlog
out="$(FAULT_POSTGRES_DSN="$pg" FAULT_MYSQL_DSN="$my" FAULT_COUNT=7 suite)" || note "suite with DSNs failed: $out"
calls="$(cat "$CALL_LOG")"
for prefix in framework auth admin faulttest; do
  grep -qF -- "-$prefix.postgres-dsn $pg -$prefix.mysql-dsn $my" <<<"$calls" ||
    note "no database run with -$prefix.* flags: $calls"
done
grep -qF -- '-tags integration -race -count=7 -timeout=600s -run . ./internal/faulttest' <<<"$calls" ||
  note "the harness's database tests do not run whole: $calls"
for pkg in ./cli ./dev; do
  if grep 'integration' <<<"$calls" | grep -qw -- "$pkg"; then
    note "$pkg has no database flags but got a database run: $calls"
  fi
done
if grep -vF -- '-count=7' <<<"$calls" | grep -q .; then
  note "FAULT_COUNT=7 did not reach every run: $calls"
fi
if grep -qE 'secret-(pg|my)' <<<"$out"; then
  note "a DSN was printed unmasked: $out"
fi

# ---- Redis: only packages that define -<pkg>.redis-addr get it ----
newlog
out="$(FAULT_REDIS_ADDR='127.0.0.1:6399' suite)" || note "suite with a Redis address failed: $out"
calls="$(cat "$CALL_LOG")"
grep -qF -- '-run ^TestFault_ ./cache -cache.redis-addr 127.0.0.1:6399' <<<"$calls" ||
  note "the cache package got no Redis run: $calls"
if grep 'integration' <<<"$calls" | grep -qE -- '-(framework|auth|admin)\.'; then
  note "a package without a Redis flag got a database run from FAULT_REDIS_ADDR alone: $calls"
fi
if grep -qF '127.0.0.1:6399' <<<"$out"; then
  note "the Redis address was printed unmasked: $out"
fi

# ---- a failing go test fails the suite, and stops it ----
newlog
if out="$(FAULT_POSTGRES_DSN="$pg" FAIL_MATCH='./internal/faulttest' suite 2>&1)"; then
  note "the suite passed with a failing go test: $out"
fi
if [ "$(wc -l < "$CALL_LOG")" -ne 1 ]; then
  note "the suite kept running after a failure: $(cat "$CALL_LOG")"
fi

# ---- a package without database flags does not end the suite early ----
# (grep finding no flag in ./cli once killed the script under pipefail, with
# no message and before the packages after it ran)
newlog
FAULT_POSTGRES_DSN="$pg" suite >/dev/null || note "suite with only a Postgres DSN failed"
grep -q -- '-framework.postgres-dsn' "$CALL_LOG" || note "packages after ./cli were skipped: $(cat "$CALL_LOG")"
if grep -q -- '-framework.mysql-dsn' "$CALL_LOG"; then
  note "a MySQL flag without FAULT_MYSQL_DSN: $(cat "$CALL_LOG")"
fi

# ---- only this module's packages: never testdata or a nested module ----
if grep -qE 'testdata|benchmarks/apps' "$CALL_LOG"; then
  note "the suite selected a package outside this module: $(cat "$CALL_LOG")"
fi

# ---- a failing package listing fails the suite, loudly ----
newlog
if out="$(LIST_FAILS=1 suite 2>&1)"; then
  note "the suite passed although go list failed: $out"
fi
if [ -s "$CALL_LOG" ]; then
  note "the suite ran tests after go list failed: $(cat "$CALL_LOG")"
fi

# ---- a bad FAULT_COUNT is refused: base 10, positive, no leading zero ----
# (go test parses -count in base 0: 00 runs nothing and passes, 010 is 8)
for bad in zero 0 00 010 -1 ''; do
  [ -n "$bad" ] || continue
  if FAULT_COUNT="$bad" suite >/dev/null 2>&1; then
    note "FAULT_COUNT=$bad was accepted"
  fi
done

# ---- every run's timeout grows with FAULT_COUNT; FAULT_TIMEOUT overrides it ----
# (go test's default -timeout, 10m, covers all the repetitions of a package in
# one binary, and the x100 soak outgrew it)
newlog
FAULT_POSTGRES_DSN="$pg" FAULT_COUNT=14 suite >/dev/null || note "suite with FAULT_COUNT=14 failed"
if grep -vF -- '-count=14 -timeout=600s ' "$CALL_LOG" | grep -q .; then
  note "FAULT_COUNT=14 (40s each, under 10m) did not keep the 10m floor on every run: $(cat "$CALL_LOG")"
fi
newlog
FAULT_POSTGRES_DSN="$pg" FAULT_COUNT=100 suite >/dev/null || note "suite with FAULT_COUNT=100 failed"
if grep -vF -- '-count=100 -timeout=4000s ' "$CALL_LOG" | grep -q .; then
  note "FAULT_COUNT=100 did not give every run 4000s: $(cat "$CALL_LOG")"
fi
# FAULT_TIMEOUT is whole seconds, passed on with an s.
for good in 4000 5400; do
  newlog
  FAULT_POSTGRES_DSN="$pg" FAULT_COUNT=100 FAULT_TIMEOUT="$good" suite >/dev/null ||
    note "suite with FAULT_TIMEOUT=$good failed"
  if grep -vF -- "-count=100 -timeout=${good}s " "$CALL_LOG" | grep -q .; then
    note "FAULT_TIMEOUT=$good did not reach every run as -timeout=${good}s: $(cat "$CALL_LOG")"
  fi
done
# Refused before any test runs: zero, a leading zero, a fraction, a unit (the
# s is added), a sign, a space, and anything else.
for bad in 0 00 010 1.5 90m 4000s -1 +1 ' 10' ten; do
  newlog
  status=0
  FAULT_TIMEOUT="$bad" suite >/dev/null 2>&1 || status=$?
  if [ "$status" -ne 2 ]; then
    note "FAULT_TIMEOUT='$bad' exited $status, want 2 (refused)"
  fi
  if [ -s "$CALL_LOG" ]; then
    note "FAULT_TIMEOUT='$bad' still ran tests: $(cat "$CALL_LOG")"
  fi
done

# ---- shards partition the packages: every one runs, exactly once ----
newlog
FAULT_POSTGRES_DSN="$pg" suite >/dev/null || note "unsharded suite failed"
whole="$(grep -oE '\./[a-z/]+' "$CALL_LOG" | sort)"
sharded=""
for i in 1 2 3; do
  newlog
  FAULT_POSTGRES_DSN="$pg" FAULT_SHARD="$i/3" suite >/dev/null || note "shard $i/3 failed"
  sharded="$sharded"$'\n'"$(grep -oE '\./[a-z/]+' "$CALL_LOG")"
done
sharded="$(grep . <<<"$sharded" | sort)"
if [ "$whole" != "$sharded" ]; then
  note "shards 1..3 do not cover the suite exactly once:
unsharded: $whole
sharded:   $sharded"
fi
for bad in 0/3 4/3 1/0 1 a/b 01/3; do
  if FAULT_SHARD="$bad" suite >/dev/null 2>&1; then
    note "FAULT_SHARD=$bad was accepted"
  fi
done

# ---- compile-only builds the same binaries and runs no test ----
newlog
FAULT_POSTGRES_DSN="$pg" suite >/dev/null || note "suite failed"
full="$(sed -E 's/-run [^ ]+ //; s/-count=[0-9]+ //; s/-timeout=[^ ]+ //' "$CALL_LOG")"
newlog
FAULT_POSTGRES_DSN="$pg" FAULT_COMPILE_ONLY=1 FAULT_COUNT=9 suite >/dev/null || note "compile-only failed"
if grep -v -- "-run ^\$ " "$CALL_LOG" | grep -q .; then
  note "compile-only ran tests: $(cat "$CALL_LOG")"
fi
if [ "$(sed -E 's/-run [^ ]+ //; s/-count=[0-9]+ //; s/-timeout=[^ ]+ //' "$CALL_LOG")" != "$full" ]; then
  note "compile-only did not build what the run uses:
run:     $full
compile: $(cat "$CALL_LOG")"
fi

# ---- a run over its budget fails, and says so ----
newlog
if out="$(GO_SLEEP=2 FAULT_BUDGET_SECONDS=1 FAULT_SHARD=1/9 suite 2>&1)"; then
  note "a run over FAULT_BUDGET_SECONDS passed: $out"
elif ! grep -q 'over budget' <<<"$out"; then
  note "an over-budget run did not say why: $out"
fi
newlog
FAULT_BUDGET_SECONDS=600 suite >/dev/null || note "a run within its budget failed"

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "test-faults.sh: ok"
