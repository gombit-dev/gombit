package gombit

import (
	"testing"

	"github.com/gombit-dev/gombit/benchmarks/micro/scenario"
)

// Ablation layers for per-layer middleware benchmarking. These are the layers
// from runtimeMiddlewareStack in framework/app.go, each representing a category
// of middleware cost.
var ablationLayers = []string{
	"recovery",
	"request_context",
	"metrics",
	"security_headers",
	"xss",
	"csrf",
	"request_timeout",
}

// BenchmarkAblation runs the framework-tax scenario with progressive middleware
// stacking: first recovery only, then recovery+request_context, etc. Each
// sub-benchmark isolates the cost of adding one layer on top of what came
// before. Run with:
//
//	go test ./benchmarks/micro/gombit -bench=BenchmarkAblation -benchmem -count=10
func BenchmarkAblation(b *testing.B) {
	for _, layer := range ablationLayers {
		b.Run("gombit-ablation/"+layer, func(b *testing.B) {
			app := NewAppWithAblation(ablationLayers[:indexOf(ablationLayers, layer)+1])
			s := scenario.Stack{Name: "gombit-ablation/" + layer, Handler: app.Router(), Envelope: true}
			scenario.RunBenchmark(b, s)
		})
	}
}

// BenchmarkAblationSolo is the parallel-scaling variant of BenchmarkAblation
// (issue #243). Run with:
//
//	go test ./benchmarks/micro/gombit -bench=BenchmarkAblationSolo -benchmem -cpu=1,2,4,8,16
func BenchmarkAblationSolo(b *testing.B) {
	for _, layer := range ablationLayers {
		b.Run("gombit-ablation/"+layer, func(b *testing.B) {
			app := NewAppWithAblation(ablationLayers[:indexOf(ablationLayers, layer)+1])
			s := scenario.Stack{Name: "gombit-ablation/" + layer, Handler: app.Router(), Envelope: true}
			scenario.RunParallelBenchmark(b, s)
		})
	}
}

func indexOf(layers []string, layer string) int {
	for i, l := range layers {
		if l == layer {
			return i
		}
	}
	return -1
}
