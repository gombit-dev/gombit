// Package microbench is the schema, `go test -bench` parser, and encoders for
// the framework-tax microbenchmark (issue #141 §13 A): the per-request
// abstraction cost of each layer — net/http → Gin → Huma → Gombit — across the
// five scenarios (plaintext, json, path-param, valid-post, invalid-post),
// reported as ns/op, B/op, and allocs/op.
//
// Each stack is a separate `go test` process (constructing framework.App
// mutates a process-global; see benchmarks/micro/gombit), so the parser tags
// rows with the stack the orchestrator ran, and rows merge by stack (a stack is
// replaced whole, so a partial re-run can't leave stale scenarios behind).
// Every `-count` ns/op sample is kept, not just the last, so a statistical
// summary (median/CoV, benchstat) is a non-breaking addition rather than lost
// data. Keeping the parse + schema here (not in the shell) lets it be
// unit-tested against captured `go test -bench` output.
package microbench

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// SchemaVersion is bumped on any breaking change to the row shape (issue §9).
const SchemaVersion = 1

// frameworkTaxPrefix is the sub-benchmark name the four-row framework-tax
// scenarios live under: BenchmarkFrameworkTax/<scenario>. Every row for one
// `go test` process shares the -stack argument passed to ParseBenchOutput.
const frameworkTaxPrefix = "BenchmarkFrameworkTax/"

// ablationPrefix is the sub-benchmark name the Gombit per-layer middleware
// ablation benchmark (benchmarks/micro/gombit/ablation_bench_test.go) lives
// under: BenchmarkAblation/gombit-ablation/<layer>/<scenario>. Unlike
// BenchmarkFrameworkTax, a single `go test -bench=BenchmarkAblation` process
// reports many stacks at once (one per progressively-stacked layer), so the
// stack is parsed out of the benchmark name itself (everything between the
// prefix and the final "/<scenario>") rather than taken from the -stack
// argument.
const ablationPrefix = "BenchmarkAblation/"

// Scenarios is the full set every stack must report; a run missing any of these
// is incomplete and rejected rather than published as a partial row set.
var Scenarios = []string{"plaintext", "json", "path-param", "valid-post", "invalid-post"}

// Row is one (stack, scenario) measurement. NsPerOp holds every `-count` sample
// (so median/variance is derivable and nothing is thrown away); B/op and
// allocs/op are deterministic for these benchmarks and stored as scalars.
type Row struct {
	SchemaVersion int       `json:"schema_version"`
	Stack         string    `json:"stack"`
	Scenario      string    `json:"scenario"`
	NsPerOp       []float64 `json:"ns_per_op"`
	BytesPerOp    int64     `json:"bytes_per_op"`
	AllocsPerOp   int64     `json:"allocs_per_op"`
}

// MedianNsPerOp is the median of the ns/op samples (0 if none) — the headline
// statistic, matching the CRUD table's use of the median.
func (r Row) MedianNsPerOp() float64 {
	n := len(r.NsPerOp)
	if n == 0 {
		return 0
	}
	s := append([]float64(nil), r.NsPerOp...)
	sort.Float64s(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// CoVNsPerOp is the coefficient of variation (sample stddev / mean) of the ns/op
// samples — a stability signal. A high value means the run was noisy (a
// non-stationary series on a contended host), so the median should be
// distrusted: e.g. a layering-cost inversion (Huma cheaper than the Gin it sits
// on) is only ever noise. Fewer than two samples or a zero mean → 0.
func (r Row) CoVNsPerOp() float64 {
	n := len(r.NsPerOp)
	if n < 2 {
		return 0
	}
	var sum float64
	for _, x := range r.NsPerOp {
		sum += x
	}
	mean := sum / float64(n)
	if mean == 0 {
		return 0
	}
	var ss float64
	for _, x := range r.NsPerOp {
		d := x - mean
		ss += d * d
	}
	return math.Sqrt(ss/float64(n-1)) / mean
}

// ParseBenchOutput reads `go test -bench` text output and returns a row per
// (stack, scenario) pair found, accumulating every iteration's ns/op into
// NsPerOp. Non-benchmark lines are ignored. Two benchmark name shapes are
// recognized:
//
//   - BenchmarkFrameworkTax/<scenario> — the four-row framework-tax matrix
//     (net/http, gin, huma, gombit). Every line in one `go test` process
//     belongs to the single stack passed as the stack argument, so that
//     argument names the row directly.
//   - BenchmarkAblation/gombit-ablation/<layer>/<scenario> — the Gombit
//     per-layer middleware ablation benchmark
//     (benchmarks/micro/gombit/ablation_bench_test.go). A single `go test`
//     process reports many stacks at once (one per progressively-stacked
//     layer), so each line's stack ("gombit-ablation/<layer>") is parsed out
//     of the benchmark name itself rather than taken from the stack argument.
//
// It errors unless every discovered stack reports every scenario in Scenarios
// (a panic mid-run yields fewer scenarios, which must fail rather than
// publish a partial stack), and unless at least one recognized benchmark line
// was found at all.
func ParseBenchOutput(stack string, r io.Reader) ([]Row, error) {
	if stack == "" {
		return nil, fmt.Errorf("stack is required")
	}
	byStack := map[string]map[string]*Row{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		rowStack, scenario, ok := parseBenchName(stack, fields[0])
		if !ok {
			continue
		}
		ns, bytesPer, allocs, ok := parseMetrics(fields)
		if !ok {
			continue
		}
		byScenario := byStack[rowStack]
		if byScenario == nil {
			byScenario = map[string]*Row{}
			byStack[rowStack] = byScenario
		}
		row := byScenario[scenario]
		if row == nil {
			row = &Row{SchemaVersion: SchemaVersion, Stack: rowStack, Scenario: scenario}
			byScenario[scenario] = row
		}
		row.NsPerOp = append(row.NsPerOp, ns)
		row.BytesPerOp = bytesPer // deterministic across iterations; last wins
		row.AllocsPerOp = allocs
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read bench output: %w", err)
	}
	if len(byStack) == 0 {
		return nil, fmt.Errorf("no BenchmarkFrameworkTax or BenchmarkAblation rows found for stack %q", stack)
	}
	stacks := make([]string, 0, len(byStack))
	for s := range byStack {
		stacks = append(stacks, s)
	}
	sort.Strings(stacks)
	var rows []Row
	for _, st := range stacks {
		byScenario := byStack[st]
		for _, want := range Scenarios {
			if byScenario[want] == nil {
				return nil, fmt.Errorf("stack %q is missing the %q scenario (incomplete run — %d of %d scenarios present)",
					st, want, len(byScenario), len(Scenarios))
			}
		}
		for _, s := range Scenarios {
			rows = append(rows, *byScenario[s])
		}
	}
	return rows, nil
}

// parseBenchName recognizes the two benchmark name shapes ParseBenchOutput
// understands (see its doc comment) and returns the row's stack and scenario.
// ok is false for any other line (including the top-level "BenchmarkAblation"
// and "BenchmarkFrameworkTax" lines Go itself never emits metrics for, and
// any unrelated benchmark).
func parseBenchName(stack, name string) (rowStack, scenario string, ok bool) {
	switch {
	case strings.HasPrefix(name, ablationPrefix):
		rest := stripProcs(strings.TrimPrefix(name, ablationPrefix))
		i := strings.LastIndex(rest, "/")
		if i < 0 {
			return "", "", false
		}
		return rest[:i], rest[i+1:], true
	case strings.HasPrefix(name, frameworkTaxPrefix):
		return stack, stripProcs(strings.TrimPrefix(name, frameworkTaxPrefix)), true
	default:
		return "", "", false
	}
}

// stripProcs removes the trailing -<GOMAXPROCS> Go appends to a benchmark name,
// matching testing.benchmarkName: the suffix is only present when GOMAXPROCS !=
// 1 and is always digits, so it is dropped only when what follows the last '-'
// is entirely numeric. That leaves the hyphenated scenario names (path-param,
// valid-post, invalid-post) intact when Go emits them unsuffixed (GOMAXPROCS=1).
func stripProcs(s string) string {
	i := strings.LastIndex(s, "-")
	if i < 0 || i == len(s)-1 {
		return s
	}
	for _, r := range s[i+1:] {
		if r < '0' || r > '9' {
			return s
		}
	}
	return s[:i]
}

// parseMetrics returns (ns/op, B/op, allocs/op, ok) from the "<value> <unit>"
// pairs in a benchmark line; ok is false if ns/op is absent.
func parseMetrics(fields []string) (ns float64, bytesPer, allocs int64, ok bool) {
	for i := 1; i < len(fields); i++ {
		switch fields[i] {
		case "ns/op":
			if v, err := strconv.ParseFloat(fields[i-1], 64); err == nil {
				ns, ok = v, true
			}
		case "B/op":
			if v, err := strconv.ParseInt(fields[i-1], 10, 64); err == nil {
				bytesPer = v
			}
		case "allocs/op":
			if v, err := strconv.ParseInt(fields[i-1], 10, 64); err == nil {
				allocs = v
			}
		}
	}
	return ns, bytesPer, allocs, ok
}

// Merge returns existing with every row for the incoming stacks removed and the
// incoming rows appended — a stack is replaced as a whole, so a re-run can't
// leave a stale scenario from a previous run. Deterministically sorted.
func Merge(existing, incoming []Row) []Row {
	replacedStack := map[string]bool{}
	for _, r := range incoming {
		replacedStack[r.Stack] = true
	}
	out := make([]Row, 0, len(existing)+len(incoming))
	for _, r := range existing {
		if !replacedStack[r.Stack] {
			out = append(out, r)
		}
	}
	out = append(out, incoming...)
	sortRows(out)
	return out
}

// MergeStack replaces a whole stack *namespace* with incoming and appends it,
// preserving unrelated stacks. It clears every existing row whose stack is
// exactly namespace or lies under it (namespace + "/..."), then adds incoming.
//
// This is what a single benchmark run persists through: a framework-tax run
// (namespace "gin") owns exactly one stack, while an ablation run (namespace
// "gombit-ablation") owns a whole dynamic ladder of `gombit-ablation/<row>`
// stacks derived from the runtime middleware stack. Plain Merge only drops the
// exact stacks present in this run's output, so a layer removed or renamed
// between runs would leave its `gombit-ablation/<old>` row orphaned in the
// authoritative JSON forever. Clearing the namespace makes the persisted
// ladder track the runtime-derived one. The "/" boundary keeps sibling
// namespaces safe: clearing "gombit" never touches "gombit-ablation/xss", and
// clearing "gombit-ablation" never touches "gombit".
func MergeStack(existing, incoming []Row, namespace string) []Row {
	prefix := namespace + "/"
	out := make([]Row, 0, len(existing)+len(incoming))
	for _, r := range existing {
		if r.Stack == namespace || strings.HasPrefix(r.Stack, prefix) {
			continue
		}
		out = append(out, r)
	}
	out = append(out, incoming...)
	sortRows(out)
	return out
}

func sortRows(rows []Row) {
	scenIdx := map[string]int{}
	for i, s := range Scenarios {
		scenIdx[s] = i
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Stack != rows[j].Stack {
			return rows[i].Stack < rows[j].Stack
		}
		return scenIdx[rows[i].Scenario] < scenIdx[rows[j].Scenario]
	})
}

// WriteJSON / ReadJSON are the canonical microbench.json encoders.
func WriteJSON(w io.Writer, rows []Row) error {
	sorted := append([]Row(nil), rows...)
	sortRows(sorted)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(sorted)
}

func ReadJSON(r io.Reader) ([]Row, error) {
	var rows []Row
	if err := json.NewDecoder(r).Decode(&rows); err != nil {
		return nil, fmt.Errorf("decode microbench json: %w", err)
	}
	return rows, nil
}

// Relative renders x/base as a "N.Nx" string (base itself is "1.0x").
func Relative(x, base float64) string {
	if base <= 0 {
		return "—"
	}
	return strconv.FormatFloat(math.Round(x/base*10)/10, 'f', 1, 64) + "×"
}
