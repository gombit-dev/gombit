package nethttp

import (
	"testing"

	"github.com/gombit-dev/gombit/benchmarks/micro/scenario"
)

func stack() scenario.Stack {
	return scenario.Stack{Name: "net-http", Handler: NewHandler(), Envelope: false}
}

// TestScenarios checks the net/http row implements the framework-tax
// scenarios correctly before it's used for benchmarking. See
// scenario.Assert for what "correctly" means.
func TestScenarios(t *testing.T) {
	scenario.Assert(t, stack())
}

// BenchmarkFrameworkTax reports the net/http row of the framework-tax
// matrix. Run alongside the gin, huma, and gombit rows with:
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
