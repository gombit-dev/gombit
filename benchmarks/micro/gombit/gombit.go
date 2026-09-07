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
	"net/http"

	"github.com/danielgtaylor/huma/v2/adapters/humagin"
	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/auth"
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

// NewAppWithAblation returns a framework.App configured with a progressively
// stacked subset of middleware layers, allowing per-layer ablation benchmarking.
// Layers must be a subset of the full middleware stack (recovery, request_context,
// metrics, security_headers, xss, csrf, request_timeout) in that order.
// This function replicates framework.New's initialization but allows selective
// middleware enabling/disabling by building only the specified layers.
func NewAppWithAblation(layers []string) *framework.App {
	cfg := config.DefaultFor(config.EnvironmentProduction)

	// Build a custom router with only the requested middleware layers, without
	// creating a full framework.App first. This avoids initializing the full
	// middleware stack only to discard it.
	router := buildCustomRouter(cfg, layers)

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

func buildCustomRouter(cfg config.Config, layers []string) *gin.Engine {
	router := gin.New()

	// Build middleware stack based on requested layers
	stack := buildAblationMiddlewareStack(cfg, layers)

	// Apply middleware
	for _, mw := range stack {
		router.Use(mw.handler)
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

// namedMiddleware mirrors the one in framework/app.go for ablation building
type namedMiddleware struct {
	name    string
	handler gin.HandlerFunc
}

// buildAblationMiddlewareStack builds a middleware stack containing only the
// requested layers, progressively stacked. This allows benchmarking the cost
// of each layer by running with fewer layers and measuring the delta.
// Layers must be valid names from the full middleware stack; unknown layers
// are silently skipped.
func buildAblationMiddlewareStack(cfg config.Config, layers []string) []namedMiddleware {
	layerSet := make(map[string]bool)
	for _, layer := range layers {
		layerSet[layer] = true
	}

	var stack []namedMiddleware

	// Build middleware layers in the same order as runtimeMiddlewareStack
	// in framework/app.go, but only include requested layers.

	if layerSet["recovery"] {
		stack = append(stack, namedMiddleware{name: "recovery", handler: gin.Recovery()})
	}

	if layerSet["request_context"] {
		stack = append(stack, namedMiddleware{name: "request_context", handler: framework.RequestContextMiddleware()})
	}

	if layerSet["metrics"] {
		// Metrics middleware needs httpMetrics; create a simple one for this router
		// Note: we can't use the global metrics from framework since we're building
		// a standalone router. Each ablation benchmark gets its own metrics instance.
		metrics := framework.NewHTTPMetrics()
		stack = append(stack, namedMiddleware{name: "metrics", handler: framework.MetricsMiddleware(metrics)})
	}

	if layerSet["security_headers"] {
		isProd := cfg.Environment == config.EnvironmentProduction
		stack = append(stack, namedMiddleware{
			name:    "security_headers",
			handler: framework.SecurityHeadersMiddleware(isProd),
		})
	}

	if layerSet["xss"] {
		stack = append(stack, namedMiddleware{name: "xss", handler: framework.XSSMiddleware()})
	}

	if layerSet["csrf"] {
		// CSRF is only enabled when auth is enabled and in cookie mode
		if cfg.Auth.Enabled() && cfg.Auth.EffectiveMode() == config.AuthModeCookie {
			// For ablation, we skip exempt paths since this is a benchmark.
			// auth.CSRFMiddleware is already exported for this use case.
			stack = append(stack, namedMiddleware{name: "csrf", handler: auth.CSRFMiddleware(cfg)})
		}
	}

	if layerSet["request_timeout"] {
		stack = append(stack, namedMiddleware{name: "request_timeout", handler: framework.RequestTimeoutMiddleware(cfg.HTTP.RequestTimeout)})
	}

	return stack
}
