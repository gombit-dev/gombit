//go:build chaos

package chaos

import (
	"fmt"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestRandIsAPureFunctionOfTheKey: the random source is a pure function of
// (seed, scenario, iteration): the same three give the same values, and a
// different one gives different values. (TestScenarioReplays checks that the
// scenarios themselves replay.)
func TestRandIsAPureFunctionOfTheKey(t *testing.T) {
	draw := func(seed int64, name string, it int) []uint64 {
		r := rngFor(seed, name, it)
		out := make([]uint64, 16)
		for i := range out {
			out[i] = r.Uint64()
		}
		return out
	}
	same := func(a, b []uint64) bool {
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	base := draw(928471923, "database/failure-boundary", 147)
	if !same(base, draw(928471923, "database/failure-boundary", 147)) {
		t.Fatal("the same seed, scenario, and iteration drew differently: replay is broken")
	}
	for name, other := range map[string][]uint64{
		"another seed":      draw(928471924, "database/failure-boundary", 147),
		"another scenario":  draw(928471923, "database/concurrent-writers", 147),
		"another iteration": draw(928471923, "database/failure-boundary", 148),
	} {
		if same(base, other) {
			t.Errorf("%s drew the same values: iterations are not independent", name)
		}
	}
}

// TestScenariosDrawOnlyFromEnvRand: a scenario that takes randomness from
// anywhere but env.Rand (math/rand's globals, crypto/rand, the clock as a
// seed) cannot be replayed from its seed, so no scenario file may import a
// random source.
func TestScenariosDrawOnlyFromEnvRand(t *testing.T) {
	files, err := filepath.Glob("scenario*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no scenario files found (%v)", err)
	}
	for _, file := range files {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasSuffix(path, "/rand") || strings.HasPrefix(path, "math/rand") {
				t.Errorf("%s imports %s: scenarios must draw only from env.Rand, or a seed cannot replay them", file, path)
			}
		}
	}
}

// runOnce runs scenario s at (seed, iteration) as a subtest and returns what
// it drew and every failure it reported.
func runOnce(t *testing.T, s Scenario, seed int64, it int, label string) (drawn, failures []string) {
	t.Helper()
	t.Run(label, func(t *testing.T) {
		env := Environment{Seed: seed, Iteration: it, Rand: rngFor(seed, s.Name, it), PostgresDSN: postgres}
		env.drawn, env.mismatches = &drawn, &failures
		s.Run(t, env)
	})
	return drawn, failures
}

// TestScenarioReplays: every registered scenario, run twice at the same
// (seed, iteration), draws the same configuration and reports the same
// failures. The cancellation
// scenario is replayed on an iteration that cancels during an insert, and
// the Postgres scenario runs when CHAOS_POSTGRES_DSN is set (it skips
// otherwise, and says so).
func TestScenarioReplays(t *testing.T) {
	const replaySeed = 928471923
	for _, s := range scenarios {
		t.Run(s.Name, func(t *testing.T) {
			iterations := []int{0, 1}
			if s.Name == "context/cancellation-stress" {
				// Find an iteration that cancels during an insert, where
				// the cancellation must reach the statement.
				found := false
				for it := 2; it < 40 && !found; it++ {
					drawn, _ := runOnce(t, s, replaySeed, it, fmt.Sprintf("probe-%d", it))
					if len(drawn) > 0 && strings.Contains(drawn[0], "cancel during insert") {
						iterations, found = append(iterations, it), true
					}
				}
				if !found {
					t.Fatal("no iteration in 2..39 cancels during an insert")
				}
			}
			for _, it := range iterations {
				d1, f1 := runOnce(t, s, replaySeed, it, fmt.Sprintf("iter-%d-run-1", it))
				if len(d1) == 0 {
					t.Skipf("the scenario drew nothing (skipped: %s needs CHAOS_POSTGRES_DSN?)", s.Name)
				}
				d2, f2 := runOnce(t, s, replaySeed, it, fmt.Sprintf("iter-%d-run-2", it))
				if strings.Join(d1, "\n") != strings.Join(d2, "\n") {
					t.Errorf("iteration %d drew %q, then %q on replay", it, d1, d2)
				}
				if strings.Join(f1, "\n") != strings.Join(f2, "\n") {
					t.Errorf("iteration %d reported %q, then %q on replay", it, f1, f2)
				}
			}
		})
	}
}

// TestPostgresModeIsPinned: CHAOS_POSTGRES, which every replay command
// sets, decides the Postgres configuration: 1 requires the DSN (a replay of
// a Postgres run cannot silently fall back to SQLite), 0 ignores it, unset
// follows it, anything else is refused.
func TestPostgresModeIsPinned(t *testing.T) {
	const dsn = "postgres://h/db"
	for _, tc := range []struct {
		mode, dsn, want string
		fails           bool
	}{
		{"", dsn, dsn, false},
		{"", "", "", false},
		{"1", dsn, dsn, false},
		{"1", "", "", true},
		{"0", dsn, "", false},
		{"yes", dsn, "", true},
	} {
		got, err := postgresDSN(tc.mode, tc.dsn)
		if (err != nil) != tc.fails || got != tc.want {
			t.Errorf("CHAOS_POSTGRES=%q with DSN %q = %q, %v; want %q (error: %v)", tc.mode, tc.dsn, got, err, tc.want, tc.fails)
		}
	}
}
