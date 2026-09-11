package gombit

import (
	"net/http"
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

// coverageScenario is one of the five framework-tax request shapes, named so
// the coverage test can report which one drifted.
type coverageScenario struct {
	name   string
	method string
	path   string
	body   string
}

var coverageScenarios = []coverageScenario{
	{"plaintext", http.MethodGet, "/plaintext", ""},
	{"json", http.MethodGet, "/json", ""},
	{"path-param", http.MethodGet, "/users/user-42", ""},
	{"valid-post", http.MethodPost, "/users", scenario.ValidCreateUserBody},
	{"invalid-post", http.MethodPost, "/users", scenario.InvalidCreateUserBody},
}

// allocTolerance is the per-scenario allocs/op difference the coverage test
// permits between the full app and the max-ablation app. It is well below one
// middleware layer's worth of allocations (each runtime layer that touches the
// request adds several) but absorbs incidental noise from the two routers
// being constructed by slightly different code paths (framework.newRouter vs
// buildCustomRouter).
const allocTolerance = 2.0

// TestAblationStackCoverageMatchesFullApp asserts that the max-ablation app
// (all real runtime layers) is allocation-equivalent to the full framework.App
// on every scenario, and therefore that the ablation is measuring the genuine
// runtime stack rather than a subset of it.
//
// Both apps draw their middleware from runtimeMiddlewareStack — the full app
// via framework.New, the ablation app via framework.RuntimeMiddlewareLayers —
// so their per-request allocs/op must match. If a layer is added to the
// runtime stack but the ablation seam or the router reconstruction stops
// mirroring it (or a layer's cost silently disappears), one side allocates
// measurably more and this test fails. That is the drift scenario.Assert alone
// cannot catch: it checks only status codes and JSON shape, which adding or
// removing a middleware layer does not change for these payloads.
func TestAblationStackCoverageMatchesFullApp(t *testing.T) {
	fullApp := NewApp()
	maxAblationApp := NewAppWithAblation(len(ablationMiddlewareLayers()))

	// Correctness first: both stacks must still implement every scenario, or an
	// allocs/op comparison between them is meaningless.
	scenario.Assert(t, scenario.Stack{Name: "gombit-full", Handler: fullApp.Router(), Envelope: true})
	scenario.Assert(t, scenario.Stack{Name: "gombit-ablation-full", Handler: maxAblationApp.Router(), Envelope: true})

	for _, sc := range coverageScenarios {
		full := allocsPerRequest(fullApp.Router(), sc)
		ablation := allocsPerRequest(maxAblationApp.Router(), sc)
		if diff := full - ablation; diff < -allocTolerance || diff > allocTolerance {
			t.Errorf("%s: full app allocs/op=%.1f, max-ablation allocs/op=%.1f (diff %.1f > tolerance %.1f) — "+
				"the max-ablation stack no longer mirrors runtimeMiddlewareStack",
				sc.name, full, ablation, diff, allocTolerance)
		}
	}
}

// allocsPerRequest measures the average allocations a single scenario request
// makes through handler. The per-request request/recorder construction inside
// scenario.Do allocates identically for both handlers, so it cancels out of
// the full-vs-ablation comparison, leaving the middleware-attributable delta.
func allocsPerRequest(handler http.Handler, sc coverageScenario) float64 {
	return testing.AllocsPerRun(200, func() {
		scenario.Do(handler, sc.method, sc.path, sc.body)
	})
}
