// Package gombit is the Gombit-runtime row of the BENCH-1 framework-tax
// microbenchmark matrix (issue #141): the same four scenarios as
// benchmarks/micro/huma, but registered through a real framework.App instead
// of bare Huma+Gin (scenario.RegisterEnvelopedRoutes instead of
// scenario.RegisterRoutes — Gombit wraps responses in the D10 envelope,
// which bare Huma+Gin does not do by default), so the delta between the two
// rows isolates the cost of the Gombit runtime itself — request-id,
// security headers, XSS sanitization, D10 error mapping and envelope — on
// top of Huma.
//
// This package must stay in its own directory/package, separate from the
// other three rows: constructing a framework.App calls contract.Install,
// which replaces Huma's process-global huma.NewError with Gombit's D10
// error mapping, once, for the lifetime of the process (see
// contract.Install's doc comment). Since `go test` runs one process per
// package, keeping this row in its own package is what makes that safe
// rather than order-dependent.
package gombit

import (
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2/adapters/humagin"
	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/benchmarks/micro/scenario"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/framework"
)

// NewApp returns a full framework.App carrying the same four scenarios as
// the other rows, registered through the runtime's Huma API (app.API()).
func NewApp() *framework.App {
	cfg := config.DefaultFor(config.EnvironmentProduction)

	app, err := framework.New(framework.WithConfig(cfg))
	if err != nil {
		panic("benchmarks/micro/gombit: build bench app: " + err.Error())
	}

	scenario.RegisterEnvelopedRoutes(app.API())
	return app
}

// ablationConfig is the config the ablation benchmark runs under: the same
// production defaults NewApp uses, so the max-ablation stack is measuring the
// production runtime stack, not a differently-configured one. The set of
// layers a config actually installs — see framework.RuntimeMiddlewareLayers —
// depends on the config (CSRF, for instance, is installed only under cookie
// auth), so the ablation must ablate whatever THIS config resolves to rather
// than a hard-coded wish list.
func ablationConfig() config.Config {
	return config.DefaultFor(config.EnvironmentProduction)
}

// ablationMiddlewareLayers returns the real runtime middleware layers for the
// ablation config, in install order, straight from the framework. The
// benchmark ablates prefixes of THIS slice, so the layers, their order, and
// their conditionals always track framework.runtimeMiddlewareStack: add,
// remove, or reorder a layer there and the ablation follows with no edit
// here. Because these are the layers actually installed for the config, every
// row the benchmark emits measures a layer that genuinely runs — there is no
// phantom row for a layer the config leaves out.
func ablationMiddlewareLayers() []framework.MiddlewareLayer {
	return framework.RuntimeMiddlewareLayers(ablationConfig(), nil, nil)
}

// NewAppWithAblation returns a framework.App whose router carries only the
// first n layers of the real runtime middleware stack (see
// ablationMiddlewareLayers), progressively stacked, for per-layer ablation
// benchmarking. n ranges over 0..len(ablationMiddlewareLayers()); n equal to
// the full length reconstructs the production stack.
func NewAppWithAblation(n int) *framework.App {
	cfg := ablationConfig()
	layers := ablationMiddlewareLayers()
	if n < 0 || n > len(layers) {
		panic(fmt.Sprintf("benchmarks/micro/gombit: ablation prefix %d out of range [0,%d]", n, len(layers)))
	}

	// Build a custom router carrying only the requested prefix of the real
	// stack, without creating a full framework.App first — that would install
	// the whole stack only to discard it.
	router := buildCustomRouter(layers[:n])

	// Install the contract (sets up the global huma.NewError replacement) if
	// not already done. This is idempotent: contract.Install only installs once
	// per process, so calling it multiple times is safe.
	contract.Install(contract.InstallOptions{
		RequestID: framework.GetRequestIDFromContext,
	})

	// Create the Huma API for the custom router exactly once, then pass both
	// the router and the API to framework.New via options. framework.New only
	// builds its own router/API when app.router/app.api are still nil, so
	// pre-populating both here avoids a second, duplicate route registration
	// (humagin.New registers routes like /openapi.json on the underlying Gin
	// engine; calling it twice against the same router panics).
	humaAPI := humagin.New(router, contract.HumaConfigFor(cfg.AppName, "0.0.0", cfg.API.DocsEnabled))

	app, err := framework.New(
		framework.WithConfig(cfg),
		framework.WithRouter(router),
		framework.WithAPI(humaAPI),
	)
	if err != nil {
		panic("benchmarks/micro/gombit: build ablation app: " + err.Error())
	}

	scenario.RegisterEnvelopedRoutes(app.API())
	return app
}

func buildCustomRouter(layers []framework.MiddlewareLayer) *gin.Engine {
	router := gin.New()

	for _, layer := range layers {
		router.Use(layer.Handler)
	}

	// Add operational probes (same as newRouter in framework/app.go)
	router.GET("/livez", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"data": gin.H{
				"status": "ok",
			},
		})
	})
	router.GET("/readyz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"data": gin.H{
				"status": "ready",
			},
		})
	})

	return router
}
