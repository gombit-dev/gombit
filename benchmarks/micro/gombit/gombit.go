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
//
// The per-layer ablation of this stack lives in package framework
// (framework/ablation_bench_test.go), not here: it builds the runtime stack
// one unexported middleware at a time, which is only reachable from inside the
// package (issue #265).
package gombit

import (
	"github.com/gombit-dev/gombit/benchmarks/micro/scenario"
	"github.com/gombit-dev/gombit/config"
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
