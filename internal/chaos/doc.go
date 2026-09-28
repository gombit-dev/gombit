// Package chaos is Gombit's stochastic resilience suite: `make test-chaos`.
//
// Where `make test-faults` pins every fault at one exact point, this suite
// draws the fault, its boundary, and the interleaving at random, many times
// over, to look for the cases nobody wrote down. It is never part of PR CI.
//
// Every run is reproducible from one seed (INV-8). CHAOS_SEED is honored,
// or printed first when chosen at random. Every choice a scenario makes
// (database, fault, boundary, sizes, deadlines) derives from (seed,
// scenario, iteration) through env.Rand.
//
// What the seed does not fix is the Go scheduler: which of several
// concurrent goroutines reaches a scripted COMMIT first, say. A scenario
// with concurrency states an invariant that holds for every interleaving,
// so a replay reproduces the failure's conditions even when the thread order
// differs.
//
// A failure prints a CHAOS FAILURE block with the seed, scenario,
// iteration, what was drawn, what was expected and observed, and the
// command that replays it. The command pins whether Postgres was configured
// (CHAOS_POSTGRES=1 refuses to run without CHAOS_POSTGRES_DSN;
// CHAOS_POSTGRES=0 ignores it), and a selected scenario that cannot run
// fails rather than skips, so a replay never passes by not running. A
// failure that does not reproduce from its seed is a bug in this harness,
// not a flake to rerun.
//
// The tests are behind the `chaos` build tag, so `go test ./...` never runs
// them:
//
//	make test-chaos                                    # every scenario, random seed
//	CHAOS_SEED=928471923 make test-chaos               # replay a run
//	CHAOS_SCENARIO=database/failure-boundary CHAOS_ITERATION=147 make test-chaos
//	CHAOS_POSTGRES_DSN=postgres://... make test-chaos  # add the Postgres scenarios
//	CHAOS_POSTGRES=1 CHAOS_SEED=... CHAOS_SCENARIO=... CHAOS_ITERATION=... make test-chaos  # a printed replay
package chaos
