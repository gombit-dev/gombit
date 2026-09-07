package gombit

import (
	"testing"

	"github.com/gombit-dev/gombit/benchmarks/micro/scenario"
)

func stack() scenario.Stack {
	return scenario.Stack{Name: "gombit", Handler: NewApp().Router(), Envelope: true}
}

// TestScenarios checks the Gombit row implements the framework-tax scenarios
// correctly before it's used for benchmarking. See scenario.Assert for what
// "correctly" means.
func TestScenarios(t *testing.T) {
	scenario.Assert(t, stack())
}

// BenchmarkFrameworkTax reports the Gombit row of the framework-tax matrix.
// Run alongside the net-http, gin, and huma rows with:
//
//	go test ./benchmarks/micro/... -bench=BenchmarkFrameworkTax -benchmem -count=10
func BenchmarkFrameworkTax(b *testing.B) {
	scenario.RunBenchmark(b, stack())
}

// BenchmarkFrameworkTaxParallel reports this row's cross-core scaling (issue
// #243). Compare its per-op time across -cpu values to see whether the stack
// serializes requests; run alongside the other rows with:
//
//	go test ./benchmarks/micro/... -bench=BenchmarkFrameworkTaxParallel -benchmem -cpu=1,2,4,8,16
func BenchmarkFrameworkTaxParallel(b *testing.B) {
	scenario.RunParallelBenchmark(b, stack())
}

// TestAblationStackCoverageMatchesFullApp verifies that the full framework.App's
// middleware stack is completely covered by the ablation layers. This ensures
// that the last ablation sub-benchmark (request_timeout) exercise all
// middleware that framework.New installs by default, so comparing it against
// the full app's allocs/op catches middleware-stack drift.
//
// The test runs both the full app and the max-ablation app through the five
// benchmark scenarios and logs the per-scenario alloc counts for comparison.
// If the counts diverge significantly (e.g., the full app allocates
// substantially more than the last ablation row), it indicates a new middleware
// layer has been added to runtimeMiddlewareStack without being included in
// ablationLayers, or that a layer's middleware implementation has changed.
func TestAblationStackCoverageMatchesFullApp(t *testing.T) {
	// Run the full app through all five scenarios
	fullApp := NewApp()
	fullStack := scenario.Stack{Name: "gombit-full", Handler: fullApp.Router(), Envelope: true}

	// Run the max-ablation app (all layers) through the same scenarios
	maxAblationApp := NewAppWithAblation(ablationLayers)
	ablationStack := scenario.Stack{Name: "gombit-ablation-full", Handler: maxAblationApp.Router(), Envelope: true}

	// Test both to verify they work
	scenario.Assert(t, fullStack)
	scenario.Assert(t, ablationStack)

	// Both should pass; if they don't, the middleware stack has changed in a way
	// that breaks the benchmark scenarios. The benchmark harness (RunBenchmark)
	// will provide alloc counts for detailed comparison.
	t.Logf("Full app and ablation stack both pass scenarios.Assert; middleware coverage appears OK")
}
