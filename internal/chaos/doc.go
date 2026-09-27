// Package chaos is Gombit's stochastic resilience suite: `make test-chaos`.
//
// Where `make test-faults` pins every fault at one exact point, this suite
// draws the fault, its boundary, and the interleaving at random, many times
// over, to look for the cases nobody wrote down. It is never part of PR CI.
//
// Every run is reproducible from one seed (INV-8): CHAOS_SEED is honored,
// and printed first when chosen at random, and every random choice derives
// from (seed, scenario, iteration). A failure prints the seed, scenario,
// iteration, what was expected and observed, and the exact command that
// replays it. A failure that does not reproduce from its seed is a bug in
// this harness, not a flake to rerun.
//
// The tests are behind the `chaos` build tag, so `go test ./...` never runs
// them:
//
//	make test-chaos                                    # every scenario, random seed
//	CHAOS_SEED=928471923 make test-chaos               # replay a run
//	CHAOS_SCENARIO=database/failure-boundary CHAOS_ITERATION=147 make test-chaos
//	CHAOS_POSTGRES_DSN=postgres://... make test-chaos  # add the Postgres scenarios
package chaos
