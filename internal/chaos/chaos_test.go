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
	// PostgresDSN is CHAOS_POSTGRES_DSN ("" when Postgres is not configured,
	// or CHAOS_POSTGRES=0 turned it off).
	PostgresDSN string
	// Selected says the run named this scenario (CHAOS_SCENARIO): a replay.
	// A selected scenario that cannot run fails instead of skipping, since a
	// skip would make the replay pass.
	Selected bool

	mismatches *[]string // for the failure report
	drawn      *[]string // what the scenario drew, for the failure report
}

// Drew records a random choice the scenario made (the database, the fault,
// its boundary or deadline): logged, and repeated in the failure report so
// the artifact says what was injected even when the scenario stops early.
func (e Environment) Drew(t *testing.T, format string, args ...any) {
	t.Helper()
	line := fmt.Sprintf(format, args...)
	if e.drawn != nil {
		*e.drawn = append(*e.drawn, line)
	}
	t.Logf("drew: %s", line)
}

// Mismatch reports a violated invariant in the failure format: what was
// expected and what was observed. The failure report repeats it.
func (e Environment) Mismatch(t *testing.T, what, expected, observed string) {
	t.Helper()
	msg := fmt.Sprintf("%s\n\nexpected:\n  %s\n\nobserved:\n  %s", what, expected, observed)
	if e.mismatches != nil {
		*e.mismatches = append(*e.mismatches, msg)
	}
	t.Errorf("%s", msg)
}

// Fatalf stops the scenario, keeping the message for the failure report
// (t.Fatalf alone would leave the report without it).
func (e Environment) Fatalf(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if e.mismatches != nil {
		*e.mismatches = append(*e.mismatches, "stopped: "+msg)
	}
	t.Fatalf("%s", msg)
}

// RequirePostgres skips the scenario when Postgres is not configured, or,
// when the scenario was selected (a replay), fails it: a replay that skips
// would pass without reproducing anything.
func (e Environment) RequirePostgres(t *testing.T) {
	t.Helper()
	if e.PostgresDSN != "" {
		return
	}
	if e.Selected {
		e.Fatalf(t, "this scenario needs Postgres: set CHAOS_POSTGRES_DSN to replay it (a skip would pass without reproducing anything)")
	}
	t.Skip("set CHAOS_POSTGRES_DSN for the Postgres scenarios")
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
	dsn, err := postgresDSN(os.Getenv("CHAOS_POSTGRES"), os.Getenv("CHAOS_POSTGRES_DSN"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	postgres = dsn
	// First line of every run: the seed that reproduces it.
	fmt.Printf("CHAOS_SEED=%d\n", seed)
	os.Exit(m.Run())
}

// postgres is the run's Postgres DSN, "" when it has none.
var postgres string

// postgresDSN applies CHAOS_POSTGRES to CHAOS_POSTGRES_DSN. Whether Postgres
// is configured changes what a database scenario runs on, so every replay
// command pins it: CHAOS_POSTGRES=1 requires the DSN (a replay without it
// would silently run on SQLite), CHAOS_POSTGRES=0 ignores it, and unset
// takes whatever the DSN says.
func postgresDSN(mode, dsn string) (string, error) {
	switch mode {
	case "":
		return dsn, nil
	case "0":
		return "", nil
	case "1":
		if dsn == "" {
			return "", fmt.Errorf("chaos: CHAOS_POSTGRES=1: this run needs Postgres, set CHAOS_POSTGRES_DSN (the failure being replayed ran with it)")
		}
		return dsn, nil
	}
	return "", fmt.Errorf("chaos: CHAOS_POSTGRES must be 0 or 1, got %q", mode)
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
	name := os.Getenv("CHAOS_SCENARIO")
	if name != "" {
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
	env := Environment{Seed: seed, PostgresDSN: postgres, Selected: name != ""}
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
			var mismatches, drawn []string
			iterEnv.mismatches, iterEnv.drawn = &mismatches, &drawn
			t.Run(fmt.Sprintf("%s/iter-%d", s.Name, it), func(t *testing.T) {
				t.Cleanup(func() {
					if t.Failed() {
						report(t, s, it, drawn, mismatches)
					}
				})
				s.Run(t, iterEnv)
			})
		}
	}
}

// report logs the failure block and, with CHAOS_REPORT_DIR set, writes it
// to a file there (the nightly workflow uploads that directory).
func report(t *testing.T, s Scenario, iteration int, drawn, mismatches []string) {
	var observed strings.Builder
	if len(drawn) > 0 {
		observed.WriteString("\ndrawn:\n")
		for _, d := range drawn {
			observed.WriteString("  " + d + "\n")
		}
	}
	for _, m := range mismatches {
		observed.WriteString("\n" + m + "\n")
	}
	if len(mismatches) == 0 {
		observed.WriteString("\n(no message was recorded: the scenario failed through t directly; see the test output)\n")
	}
	// The replay pins whether Postgres was configured (never the DSN
	// itself): with it, a database scenario may have drawn Postgres, and a
	// replay without it would run SQLite and pass.
	pg, pgNote := "0", "postgres: not configured"
	if postgres != "" {
		pg, pgNote = "1", "postgres: configured (export CHAOS_POSTGRES_DSN before replaying; CHAOS_POSTGRES=1 refuses to run without it)"
	}
	block := fmt.Sprintf(`CHAOS FAILURE

scenario: %s
component: %s
seed: %d
iteration: %d
package: github.com/gombit-dev/gombit/internal/chaos
test: %s
%s
%s
replay:
  CHAOS_POSTGRES=%s CHAOS_SEED=%d CHAOS_SCENARIO=%s CHAOS_ITERATION=%d make test-chaos
`, s.Name, s.Component, seed, iteration, t.Name(), pgNote, observed.String(), pg, seed, s.Name, iteration)
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
