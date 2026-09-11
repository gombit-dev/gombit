package framework

import (
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2/adapters/humagin"
	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/benchmarks/micro/scenario"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
)

// The per-layer ablation lives here, in package framework, rather than in
// benchmarks/micro/gombit, because it builds the runtime stack one middleware
// at a time and those constructors (runtimeMiddlewareStack, newHTTPMetrics) are
// unexported — measuring the real stack from outside the package would mean
// either a hand-maintained copy of it or a permanent public API existing only
// for the benchmark (issue #265). It reuses the shared five-scenario set from
// benchmarks/micro/scenario so the ablation rows are directly comparable to the
// framework-tax matrix rows. See benchmarks/docs/methodology.md.

// ablationConfig is the config the ablation runs under: the same production
// defaults the Gombit framework-tax row (benchmarks/micro/gombit.NewApp) uses,
// so the full-app row measures the real production stack. Under production,
// auth defaults to JWT, so runtimeMiddlewareStack installs no CSRF layer (that
// is cookie-mode only) — the ladder ablates exactly the layers a production app
// runs.
func ablationConfig() config.Config {
	return config.DefaultFor(config.EnvironmentProduction)
}

type ablationRow struct {
	name    string
	handler http.Handler
}

// ablationRows builds the full ablation ladder:
//
//   - "baseline": bare Huma+Gin with contract.HumaConfigFor and no Gombit
//     middleware — the like-for-like floor the first layer's delta is read
//     against. Issue #265 requires HumaConfigFor here (not huma.DefaultConfig,
//     which the benchmarks/micro/huma row uses and which keeps a schema-link
//     transformer Gombit drops), so the comparison is apples-to-apples.
//   - one cumulative row per runtimeMiddlewareStack layer, in install order.
//   - "full-app": a real framework.App, so the ladder ends at the genuine
//     application rather than only a reconstruction of its stack.
//
// The middle rows are derived straight from runtimeMiddlewareStack, so the
// ladder cannot omit a layer the runtime installs;
// TestAblationFullAppMatchesRuntimeStack additionally proves the reconstruction
// matches the real App.
func ablationRows() []ablationRow {
	cfg := ablationConfig()
	layers := runtimeMiddlewareStack(cfg, newHTTPMetrics(), nil, nil)

	rows := make([]ablationRow, 0, len(layers)+2)
	rows = append(rows, ablationRow{name: "baseline", handler: ablationHandler(cfg, 0)})
	for i := range layers {
		rows = append(rows, ablationRow{name: layers[i].name, handler: ablationHandler(cfg, i+1)})
	}
	rows = append(rows, ablationRow{name: "full-app", handler: fullAppHandler(cfg)})
	return rows
}

// ablationHandler builds a Huma+Gin handler carrying the first n layers of the
// runtime middleware stack (n == 0 is the bare baseline), registering the same
// five scenarios every matrix row uses. It reconstructs the router shell rather
// than calling newRouter so it can stop after n layers; TestAblationFullApp-
// MatchesRuntimeStack guards that the n == len(stack) reconstruction allocates
// identically to a real framework.App.
func ablationHandler(cfg config.Config, n int) http.Handler {
	// Idempotent: installs the D10 huma.NewError replacement once per process,
	// so every row — baseline included — maps errors identically and the deltas
	// stay pure middleware cost.
	contract.Install(contract.InstallOptions{RequestID: GetRequestIDFromContext})

	router := gin.New()
	layers := runtimeMiddlewareStack(cfg, newHTTPMetrics(), nil, nil)
	for _, mw := range layers[:n] {
		router.Use(mw.handler)
	}
	api := humagin.New(router, contract.HumaConfigFor(cfg.AppName, "0.0.0", cfg.API.DocsEnabled))
	scenario.RegisterEnvelopedRoutes(api)
	return router
}

func fullAppHandler(cfg config.Config) http.Handler {
	app, err := New(WithConfig(cfg))
	if err != nil {
		panic("framework: build ablation full-app: " + err.Error())
	}
	scenario.RegisterEnvelopedRoutes(app.API())
	return app.Router()
}

// BenchmarkAblation reports the cumulative per-layer ablation of the Gombit
// runtime stack (issue #265 / PERF-7): a bare Huma+Gin baseline, then one row
// per middleware layer stacked in install order, then the full framework.App.
// Each layer's incremental ns/op·B/op·allocs/op is the delta between two
// consecutive rows. See benchmarks/docs/methodology.md for how to read the
// ladder and the two harness notes (httptest.NewRecorder contributes 3 constant
// allocs/op that cancel in the deltas; Header.Clone is real server behavior,
// not harness noise). Emitted into microbench.json under the
// "gombit-ablation/<row>" stacks by `make benchmark-micro-ablation`. Run with:
//
//	go test ./framework -run '^$' -bench '^BenchmarkAblation$' -benchmem -count=10
func BenchmarkAblation(b *testing.B) {
	for _, row := range ablationRows() {
		name := "gombit-ablation/" + row.name
		b.Run(name, func(b *testing.B) {
			scenario.RunBenchmark(b, scenario.Stack{Name: name, Handler: row.handler, Envelope: true})
		})
	}
}

// BenchmarkAblationSolo is the parallel-scaling variant of the ladder (issue
// #243): the same rows run through the parallel harness. Compare per-op time
// across -cpu values to see whether a layer serializes requests. Its rows are
// not persisted (the parser keys on the "BenchmarkAblation/" prefix, which
// "BenchmarkAblationSolo/" does not match). Run with:
//
//	go test ./framework -run '^$' -bench '^BenchmarkAblationSolo$' -benchmem -cpu=1,2,4,8,16
func BenchmarkAblationSolo(b *testing.B) {
	for _, row := range ablationRows() {
		name := "gombit-ablation/" + row.name
		b.Run(name, func(b *testing.B) {
			scenario.RunParallelBenchmark(b, scenario.Stack{Name: name, Handler: row.handler, Envelope: true})
		})
	}
}

// TestAblationFullAppMatchesRuntimeStack is the drift guard issue #265 requires
// "in a test, not just the benchmark". The full-app row and the last cumulative
// layer both carry every runtimeMiddlewareStack layer, so a scenario request
// through each must allocate identically. Adding a layer to
// runtimeMiddlewareStack is picked up by the cumulative ladder automatically
// (it is derived from that stack); this test additionally proves the
// reconstruction the benchmark measures matches the genuine framework.App, so
// the full-app row is neither silently cheaper nor dearer than the application
// it stands in for. scenario.Do's per-request construction allocates
// identically for both handlers, so it cancels out of the comparison.
func TestAblationFullAppMatchesRuntimeStack(t *testing.T) {
	previous := gin.Mode()
	t.Cleanup(func() { gin.SetMode(previous) })

	cfg := ablationConfig()
	layers := runtimeMiddlewareStack(cfg, newHTTPMetrics(), nil, nil)
	lastLayer := ablationHandler(cfg, len(layers))
	full := fullAppHandler(cfg)

	reqs := []struct {
		name, method, path, body string
	}{
		{"plaintext", http.MethodGet, "/plaintext", ""},
		{"json", http.MethodGet, "/json", ""},
		{"path-param", http.MethodGet, "/users/user-42", ""},
		{"valid-post", http.MethodPost, "/users", scenario.ValidCreateUserBody},
		{"invalid-post", http.MethodPost, "/users", scenario.InvalidCreateUserBody},
	}
	// A whole middleware layer is several allocs; this absorbs incidental noise
	// from the two routers being built by slightly different code paths
	// (newRouter vs the reconstruction) while still catching a dropped layer.
	const tolerance = 2.0
	for _, r := range reqs {
		reconstructed := testing.AllocsPerRun(200, func() { scenario.Do(lastLayer, r.method, r.path, r.body) })
		application := testing.AllocsPerRun(200, func() { scenario.Do(full, r.method, r.path, r.body) })
		if diff := application - reconstructed; diff < -tolerance || diff > tolerance {
			t.Errorf("%s: full-app allocs/op=%.1f, last-layer allocs/op=%.1f (diff %.1f > tolerance %.1f) — "+
				"the ablation's reconstructed stack no longer matches framework.App",
				r.name, application, reconstructed, diff, tolerance)
		}
	}
}
