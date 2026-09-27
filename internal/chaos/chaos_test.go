//go:build chaos

package chaos

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Scenario is one randomized resilience scenario. Run gets an Environment
// whose Rand is the only source of randomness it may use, so a seed replays
// it exactly.
type Scenario struct {
	Name      string // e.g. "database/failure-boundary"
	Component string // e.g. "database"
	Run       func(t *testing.T, env Environment)
}

// Environment is what a scenario runs with.
type Environment struct {
	Seed      int64
	Iteration int
	// Rand derives from (Seed, scenario name, Iteration): every choice a
	// scenario makes comes from here.
	Rand *mrand.Rand
	// PostgresDSN is CHAOS_POSTGRES_DSN ("" skips the Postgres scenarios).
	PostgresDSN string
}

// Mismatch reports a violated invariant in the failure format: what was
// expected and what was observed.
func (Environment) Mismatch(t *testing.T, what, expected, observed string) {
	t.Helper()
	t.Errorf("%s\n\nexpected:\n  %s\n\nobserved:\n  %s", what, expected, observed)
}

// scenarios are registered by the scenario files' init functions.
var scenarios []Scenario

func register(s Scenario) { scenarios = append(scenarios, s) }

// seed is this run's seed, chosen (and printed) before anything runs.
var seed int64

func TestMain(m *testing.M) {
	s, err := chooseSeed(os.Getenv("CHAOS_SEED"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	seed = s
	// First line of every run: the seed that reproduces it.
	fmt.Printf("CHAOS_SEED=%d\n", seed)
	os.Exit(m.Run())
}

// chooseSeed parses CHAOS_SEED, or draws a fresh one.
func chooseSeed(env string) (int64, error) {
	if env != "" {
		n, err := strconv.ParseInt(env, 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("chaos: CHAOS_SEED must be a non-negative base-10 integer, got %q", env)
		}
		return n, nil
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint64(b[:]) >> 1), nil
}

// rngFor is the random source for one scenario iteration (or, with
// name "order", an iteration's scenario order).
func rngFor(seed int64, name string, iteration int) *mrand.Rand {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%s#%d", name, iteration)
	// #nosec G404 G115 -- deliberately seeded and reproducible (INV-8), never
	// security; seed is non-negative (chooseSeed), so the conversion is exact.
	return mrand.New(mrand.NewPCG(uint64(seed), h.Sum64()))
}

// envInt reads a base-10 integer at least min from the environment.
func envInt(t *testing.T, name string, def, min int) int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min || (len(v) > 1 && v[0] == '0') {
		t.Fatalf("chaos: %s must be a base-10 integer of at least %d (no leading zeros), got %q", name, min, v)
	}
	return n
}

// TestChaos runs the selected scenarios for CHAOS_ITERATIONS iterations
// (default 20), in a random order per iteration.
func TestChaos(t *testing.T) {
	iterations := envInt(t, "CHAOS_ITERATIONS", 20, 1) // 0 would run nothing and pass
	onlyIteration := -1
	if os.Getenv("CHAOS_ITERATION") != "" {
		onlyIteration = envInt(t, "CHAOS_ITERATION", 0, 0)
		if onlyIteration >= iterations {
			iterations = onlyIteration + 1
		}
	}
	selected := scenarios
	if name := os.Getenv("CHAOS_SCENARIO"); name != "" {
		selected = nil
		for _, s := range scenarios {
			if s.Name == name {
				selected = append(selected, s)
			}
		}
		if len(selected) == 0 {
			names := make([]string, len(scenarios))
			for i, s := range scenarios {
				names[i] = s.Name
			}
			t.Fatalf("chaos: no scenario %q; scenarios: %s", name, strings.Join(names, ", "))
		}
	}
	env := Environment{Seed: seed, PostgresDSN: os.Getenv("CHAOS_POSTGRES_DSN")}
	t.Logf("seed %d, %d iteration(s), %d scenario(s)", seed, iterations, len(selected))

	for it := 0; it < iterations; it++ {
		if onlyIteration >= 0 && it != onlyIteration {
			continue
		}
		order := append([]Scenario(nil), selected...)
		rngFor(seed, "order", it).Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		for _, s := range order {
			s := s
			iterEnv := env
			iterEnv.Iteration = it
			iterEnv.Rand = rngFor(seed, s.Name, it)
			t.Run(fmt.Sprintf("%s/iter-%d", s.Name, it), func(t *testing.T) {
				t.Cleanup(func() {
					if t.Failed() {
						report(t, s, it)
					}
				})
				s.Run(t, iterEnv)
			})
		}
	}
}

// report logs the failure block and, with CHAOS_REPORT_DIR set, writes it
// to a file there (the nightly workflow uploads that directory).
func report(t *testing.T, s Scenario, iteration int) {
	block := fmt.Sprintf(`CHAOS FAILURE

scenario: %s
component: %s
seed: %d
iteration: %d
package: github.com/gombit-dev/gombit/internal/chaos
test: %s

replay:
  CHAOS_SEED=%d CHAOS_SCENARIO=%s CHAOS_ITERATION=%d make test-chaos
`, s.Name, s.Component, seed, iteration, t.Name(), seed, s.Name, iteration)
	t.Log("\n" + block)
	dir := os.Getenv("CHAOS_REPORT_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o750); err != nil { // #nosec G703 -- the operator's CHAOS_REPORT_DIR
		t.Logf("chaos: report dir: %v", err)
		return
	}
	file := filepath.Join(dir, fmt.Sprintf("%s-iter-%d.txt", safeName.ReplaceAllString(s.Name, "_"), iteration))
	if err := os.WriteFile(file, []byte(block), 0o600); err != nil { // #nosec G703 -- CHAOS_REPORT_DIR plus a sanitized file name
		t.Logf("chaos: report: %v", err)
	}
}

var safeName = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)
