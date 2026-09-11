package gombit

import (
	"testing"

	"github.com/gombit-dev/gombit/benchmarks/micro/scenario"
)

// BenchmarkAblation runs the framework-tax scenarios with progressive
// middleware stacking: first the first runtime layer only, then the first
// two, and so on. Each sub-benchmark isolates the incremental cost of adding
// one more layer on top of what came before. The layer set, order, and
// conditionals come straight from framework.RuntimeMiddlewareLayers (see
// ablationMiddlewareLayers), so the ablation tracks the real
// runtimeMiddlewareStack rather than a private copy of it. Run with:
//
//	go test ./benchmarks/micro/gombit -bench='^BenchmarkAblation$' -benchmem -count=10
func BenchmarkAblation(b *testing.B) {
	layers := ablationMiddlewareLayers()
	for i, layer := range layers {
		b.Run("gombit-ablation/"+layer.Name, func(b *testing.B) {
			app := NewAppWithAblation(i + 1)
			s := scenario.Stack{Name: "gombit-ablation/" + layer.Name, Handler: app.Router(), Envelope: true}
			scenario.RunBenchmark(b, s)
		})
	}
}

// BenchmarkAblationSolo is the parallel-scaling variant of BenchmarkAblation
// (issue #243). Run with:
//
//	go test ./benchmarks/micro/gombit -bench='^BenchmarkAblationSolo$' -benchmem -cpu=1,2,4,8,16
func BenchmarkAblationSolo(b *testing.B) {
	layers := ablationMiddlewareLayers()
	for i, layer := range layers {
		b.Run("gombit-ablation/"+layer.Name, func(b *testing.B) {
			app := NewAppWithAblation(i + 1)
			s := scenario.Stack{Name: "gombit-ablation/" + layer.Name, Handler: app.Router(), Envelope: true}
			scenario.RunParallelBenchmark(b, s)
		})
	}
}
