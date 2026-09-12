package microbench

import (
	"bytes"
	"strings"
	"testing"
)

// fullOutput returns `go test -bench` text covering all five scenarios once.
func fullOutput() string {
	var b strings.Builder
	b.WriteString("goos: linux\npkg: .../gin\n")
	for _, s := range Scenarios {
		b.WriteString("BenchmarkFrameworkTax/" + s + "-16   100   2740 ns/op   512 B/op   8 allocs/op\n")
	}
	b.WriteString("PASS\nok\t.../gin\t3.2s\n")
	return b.String()
}

func TestParseBenchOutputAllScenarios(t *testing.T) {
	rows, err := ParseBenchOutput("gin", strings.NewReader(fullOutput()))
	if err != nil {
		t.Fatalf("ParseBenchOutput: %v", err)
	}
	if len(rows) != len(Scenarios) {
		t.Fatalf("rows = %d, want %d", len(rows), len(Scenarios))
	}
	for _, r := range rows {
		if r.Stack != "gin" || r.SchemaVersion != SchemaVersion {
			t.Errorf("bad row identity: %+v", r)
		}
	}
}

func TestParseBenchOutputKeepsEverySample(t *testing.T) {
	// One scenario with three ns/op samples + the other four once each.
	var b strings.Builder
	for _, ns := range []string{"3000", "2600", "2800"} {
		b.WriteString("BenchmarkFrameworkTax/valid-post-16   100   " + ns + " ns/op   500 B/op   8 allocs/op\n")
	}
	for _, s := range []string{"plaintext", "json", "path-param", "invalid-post"} {
		b.WriteString("BenchmarkFrameworkTax/" + s + "-16   100   1000 ns/op   100 B/op   2 allocs/op\n")
	}
	rows, err := ParseBenchOutput("huma", strings.NewReader(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	var vp Row
	for _, r := range rows {
		if r.Scenario == "valid-post" {
			vp = r
		}
	}
	if len(vp.NsPerOp) != 3 {
		t.Fatalf("valid-post kept %d samples, want 3 (nothing thrown away)", len(vp.NsPerOp))
	}
	if vp.MedianNsPerOp() != 2800 { // median of 2600,2800,3000
		t.Errorf("median = %v, want 2800", vp.MedianNsPerOp())
	}
}

// At GOMAXPROCS=1 (go test -cpu=1 / a 1-CPU host) Go omits the -<N> suffix, so
// the hyphenated scenario names arrive bare. LastIndex("-") would truncate
// valid-post -> valid; stripProcs must keep them.
func TestParseBenchOutputUnsuffixedHyphenatedNames(t *testing.T) {
	var b strings.Builder
	for _, s := range Scenarios {
		b.WriteString("BenchmarkFrameworkTax/" + s + "   100   1500 ns/op   256 B/op   4 allocs/op\n")
	}
	rows, err := ParseBenchOutput("gombit", strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("unsuffixed names must parse: %v", err)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Scenario] = true
	}
	for _, want := range []string{"path-param", "valid-post", "invalid-post"} {
		if !seen[want] {
			t.Errorf("hyphenated scenario %q was mangled by the suffix strip; got %v", want, seen)
		}
	}
}

// ablationOutput returns `go test -bench` text for the in-package ablation
// benchmark (framework/ablation_bench_test.go): BenchmarkAblation/<stack>/<scenario>,
// one line per (stack, scenario), with the -<GOMAXPROCS> suffix Go appends.
func ablationOutput(stacks []string) string {
	var b strings.Builder
	b.WriteString("goos: linux\npkg: github.com/gombit-dev/gombit/framework\n")
	for _, stack := range stacks {
		for _, s := range Scenarios {
			b.WriteString("BenchmarkAblation/" + stack + "/" + s + "-16   100   1500 ns/op   256 B/op   4 allocs/op\n")
		}
	}
	b.WriteString("PASS\nok\tgithub.com/gombit-dev/gombit/framework\t3.2s\n")
	return b.String()
}

// The ablation benchmark reports many stacks from one process; each row's stack
// is parsed out of the benchmark name (BenchmarkAblation/<stack>/<scenario>),
// not taken from the -stack argument the way the framework-tax rows are.
func TestParseBenchOutputAblationMultipleStacks(t *testing.T) {
	stacks := []string{"gombit-ablation/baseline", "gombit-ablation/recovery", "gombit-ablation/full-app"}
	rows, err := ParseBenchOutput("gombit-ablation", strings.NewReader(ablationOutput(stacks)))
	if err != nil {
		t.Fatalf("ParseBenchOutput: %v", err)
	}
	if len(rows) != len(stacks)*len(Scenarios) {
		t.Fatalf("rows = %d, want %d (%d stacks × %d scenarios)", len(rows), len(stacks)*len(Scenarios), len(stacks), len(Scenarios))
	}
	byStack := map[string]map[string]bool{}
	for _, r := range rows {
		if r.SchemaVersion != SchemaVersion {
			t.Errorf("row %+v: schema version = %d, want %d", r, r.SchemaVersion, SchemaVersion)
		}
		if byStack[r.Stack] == nil {
			byStack[r.Stack] = map[string]bool{}
		}
		byStack[r.Stack][r.Scenario] = true
	}
	for _, want := range stacks {
		if got := byStack[want]; len(got) != len(Scenarios) {
			t.Errorf("stack %q covered %d scenarios, want %d (%v)", want, len(got), len(Scenarios), got)
		}
	}
	// The -stack argument ("gombit-ablation") must not leak in as a row stack;
	// every stack is the parsed "gombit-ablation/<row>".
	if _, leaked := byStack["gombit-ablation"]; leaked {
		t.Error(`row stack "gombit-ablation" leaked from the -stack argument; must be parsed from the name`)
	}
}

// A per-stack completeness check: if any ablation stack is missing a scenario
// (e.g. a panic mid-row), the whole run is rejected rather than published
// partial — the same guarantee the framework-tax path has.
func TestParseBenchOutputAblationRejectsIncompleteStack(t *testing.T) {
	full := ablationOutput([]string{"gombit-ablation/baseline"})
	// Drop the last scenario line from an otherwise-complete second stack.
	partial := full + "BenchmarkAblation/gombit-ablation/recovery/plaintext-16   100   1500 ns/op   256 B/op   4 allocs/op\n"
	if _, err := ParseBenchOutput("gombit-ablation", strings.NewReader(partial)); err == nil {
		t.Error("an ablation stack missing scenarios must be rejected, not published partial")
	}
}

// Lines that are neither BenchmarkFrameworkTax/… nor BenchmarkAblation/<stack>/<scenario>
// are ignored: the parallel variant (BenchmarkAblationSolo/…, which does not
// match the "BenchmarkAblation/" prefix), unrelated benchmarks, and the
// top-level BenchmarkAblation line Go emits no metrics for.
func TestParseBenchOutputAblationIgnoresUnrelatedLines(t *testing.T) {
	out := ablationOutput([]string{"gombit-ablation/baseline"}) +
		"BenchmarkAblationSolo/gombit-ablation/baseline/plaintext-16   100   1500 ns/op   256 B/op   4 allocs/op\n" +
		"BenchmarkSomethingElse/foo-16   100   10 ns/op   0 B/op   0 allocs/op\n" +
		"BenchmarkAblation-16   1   0 ns/op   0 B/op   0 allocs/op\n"
	rows, err := ParseBenchOutput("gombit-ablation", strings.NewReader(out))
	if err != nil {
		t.Fatalf("ParseBenchOutput: %v", err)
	}
	if len(rows) != len(Scenarios) {
		t.Fatalf("rows = %d, want %d — only the one real ablation stack should be parsed", len(rows), len(Scenarios))
	}
	for _, r := range rows {
		if r.Stack != "gombit-ablation/baseline" {
			t.Errorf("unexpected stack %q parsed from unrelated lines", r.Stack)
		}
	}
}

func TestStripProcs(t *testing.T) {
	// A trailing -<digits> is dropped (the GOMAXPROCS suffix); a hyphen followed
	// by non-digits is part of the scenario name and kept.
	cases := map[string]string{
		"valid-post-16": "valid-post",
		"valid-post":    "valid-post",
		"path-param-8":  "path-param",
		"json-16":       "json",
		"json":          "json",
		"plaintext":     "plaintext",
	}
	for in, want := range cases {
		if got := stripProcs(in); got != want {
			t.Errorf("stripProcs(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseBenchOutputRejectsIncompleteRun(t *testing.T) {
	// A panic after the GET scenarios leaves valid-post/invalid-post missing.
	partial := "BenchmarkFrameworkTax/plaintext-16   1   1000 ns/op   1 B/op   1 allocs/op\n" +
		"BenchmarkFrameworkTax/json-16   1   2000 ns/op   2 B/op   2 allocs/op\npanic: boom\n"
	if _, err := ParseBenchOutput("gin", strings.NewReader(partial)); err == nil {
		t.Error("an incomplete run (missing scenarios) must be rejected, not published partial")
	}
}

func TestParseBenchOutputRejectsEmptyStack(t *testing.T) {
	if _, err := ParseBenchOutput("", strings.NewReader(fullOutput())); err == nil {
		t.Error("empty stack should error")
	}
}

func TestMergeReplacesWholeStack(t *testing.T) {
	existing := []Row{
		{Stack: "gin", Scenario: "valid-post", NsPerOp: []float64{2740}},
		{Stack: "gin", Scenario: "json", NsPerOp: []float64{900}}, // stale scenario from an old run
		{Stack: "huma", Scenario: "valid-post", NsPerOp: []float64{5000}},
	}
	// A fresh gin run reports only valid-post this time; the stale gin/json must go.
	incoming := []Row{{Stack: "gin", Scenario: "valid-post", NsPerOp: []float64{9999}}}
	merged := Merge(existing, incoming)

	stacks := map[string]int{}
	for _, r := range merged {
		stacks[r.Stack]++
	}
	if stacks["gin"] != 1 {
		t.Errorf("gin should be replaced wholesale (1 row), got %d", stacks["gin"])
	}
	if stacks["huma"] != 1 {
		t.Errorf("huma should be kept, got %d", stacks["huma"])
	}
}

// The ablation ladder is dynamic: it is derived from runtimeMiddlewareStack, so
// a layer can be removed or renamed between runs. MergeStack must replace the
// whole gombit-ablation/ namespace, or an obsolete layer's row survives forever
// in the authoritative JSON beside the fresh ladder.
func TestMergeStackReplacesAblationNamespace(t *testing.T) {
	existing := []Row{
		{Stack: "gombit-ablation/baseline", Scenario: "plaintext", NsPerOp: []float64{100}},
		{Stack: "gombit-ablation/xss", Scenario: "plaintext", NsPerOp: []float64{200}}, // a layer since removed/renamed
		{Stack: "gombit", Scenario: "plaintext", NsPerOp: []float64{300}},              // unrelated framework-tax stack
	}
	// A rerun of the ladder no longer contains an xss layer.
	incoming := []Row{
		{Stack: "gombit-ablation/baseline", Scenario: "plaintext", NsPerOp: []float64{111}},
		{Stack: "gombit-ablation/request_context", Scenario: "plaintext", NsPerOp: []float64{222}},
	}
	merged := MergeStack(existing, incoming, "gombit-ablation")

	stacks := map[string]bool{}
	for _, r := range merged {
		stacks[r.Stack] = true
	}
	if stacks["gombit-ablation/xss"] {
		t.Error("obsolete gombit-ablation/xss survived a rerun; the namespace must be replaced as a whole")
	}
	if !stacks["gombit-ablation/request_context"] {
		t.Error("fresh gombit-ablation/request_context row missing after merge")
	}
	if !stacks["gombit"] {
		t.Error("unrelated framework-tax stack gombit must be preserved")
	}
}

// A sibling namespace that shares a prefix must not be swept: clearing "gombit"
// must leave "gombit-ablation/..." intact, and vice versa (the "/" boundary).
func TestMergeStackDoesNotCrossNamespaceBoundary(t *testing.T) {
	existing := []Row{
		{Stack: "gombit", Scenario: "plaintext", NsPerOp: []float64{1}},
		{Stack: "gombit-ablation/baseline", Scenario: "plaintext", NsPerOp: []float64{2}},
	}
	// Rerun only the framework-tax "gombit" leaf.
	merged := MergeStack(existing, []Row{{Stack: "gombit", Scenario: "plaintext", NsPerOp: []float64{9}}}, "gombit")

	var kept bool
	for _, r := range merged {
		if r.Stack == "gombit-ablation/baseline" {
			kept = true
		}
	}
	if !kept {
		t.Error(`clearing namespace "gombit" wrongly swept "gombit-ablation/baseline" (prefix, not a namespace member)`)
	}
}

func TestCoVNsPerOp(t *testing.T) {
	// A tight series is low-CoV; a wide (non-stationary) one is high.
	tight := Row{NsPerOp: []float64{1000, 1010, 990, 1005}}
	if cv := tight.CoVNsPerOp(); cv > 0.05 {
		t.Errorf("tight series CoV = %.3f, want < 0.05", cv)
	}
	wide := Row{NsPerOp: []float64{5538, 2589, 4479, 3210}}
	if cv := wide.CoVNsPerOp(); cv < 0.05 {
		t.Errorf("wide series CoV = %.3f, want > 0.05 (noise must be detectable)", cv)
	}
	if cv := (Row{NsPerOp: []float64{1000}}).CoVNsPerOp(); cv != 0 {
		t.Errorf("single-sample CoV = %v, want 0", cv)
	}
}

func TestRelative(t *testing.T) {
	if got := Relative(5217, 804); got != "6.5×" {
		t.Errorf("Relative(5217,804) = %q, want 6.5×", got)
	}
	if got := Relative(804, 804); got != "1.0×" {
		t.Errorf("baseline should be 1.0×, got %q", got)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	rows := []Row{
		{Stack: "huma", Scenario: "valid-post", NsPerOp: []float64{5000, 5100}},
		{Stack: "gin", Scenario: "plaintext", NsPerOp: []float64{1050}},
		{Stack: "gin", Scenario: "valid-post", NsPerOp: []float64{2740}},
	}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, rows); err != nil {
		t.Fatal(err)
	}
	got, err := ReadJSON(&buf)
	if err != nil {
		t.Fatal(err)
	}
	// Sorted by (stack, scenario-in-Scenarios-order): gin/plaintext, gin/valid-post, huma/valid-post.
	if len(got) != 3 || got[0].Stack != "gin" || got[0].Scenario != "plaintext" {
		t.Fatalf("unexpected order: %+v", got)
	}
	if len(got[2].NsPerOp) != 2 {
		t.Errorf("samples not preserved through round-trip: %+v", got[2])
	}
}
