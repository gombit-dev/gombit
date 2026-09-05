# Application Lifecycle

The `framework` package owns the application lifecycle introduced in M1-2.
Applications construct an `App`, register optional hooks, and run it through
`framework.Run`.

```go
app, err := framework.New()
if err != nil {
    return err
}

app.OnStart(func(ctx context.Context) error {
    return nil
})

app.OnStop(func(ctx context.Context) error {
    return nil
})

return framework.Run(app)
```

Start hooks run in registration order. Stop hooks run in reverse registration
order so cleanup unwinds deterministically. Stop hooks receive a bounded
shutdown context; the default timeout is 10 seconds. If `http.Server.Shutdown`
hits that deadline while handlers are still in flight, Gombit calls
`server.Close()` so remaining connections are dropped and `Run`/`RunContext`
can return. Stop hooks also run after a start-hook failure, so they must
tolerate partial application startup and be safe to call when the resource
they clean up was never initialized.

`RunContext` binds the listener before start hooks run, which lets hooks read
`App.Addr()` even when the configured HTTP address uses port `0`. The HTTP
server starts serving only after all start hooks succeed.

`App.Router()` exposes the underlying `*gin.Engine` as the raw Gin escape hatch
required by ADR-011. Public API routes that belong in OpenAPI still need Huma
typed handlers. `framework.WithEmbeddedFrontend` installs Gin `NoRoute` only
when the FS has `index.html`; it does not wrap or replace `*gin.Engine`.

The default router installs `gin.Recovery()` so panics in runtime probes or raw
Gin escape-hatch handlers do not terminate the process. Production config sets
Gin release mode before the default router is constructed.

## Lifecycle Ownership

M1-2 introduces the framework-owned skeleton. Later issues fill in the lifecycle
steps that need their own runtime surfaces.

| Step | Status |
| --- | --- |
| 1. config loading | Owned in M1-1/M1-2 through `config.Config` and `framework.New`. |
| 2. logging initialization | Owned in M1-6 through Zap-backed `logging.New`; Mongo sinks are external modules. |
| 3. database connection | Owned in M1-4 through `database.Open`; applications open/close the handle and attach it with `framework.WithDatabase`. |
| 4. migration state checks | Deferred to M2. |
| 5. optional Redis/cache connection | Owned in M1-5 through `cache.Open`, the default memory cache, and `app.Redis()` for the Redis escape hatch. |
| 6. auth infrastructure | Owned in M5-2 (Bearer) and M5-3 (cookie/session + CSRF) through `auth` + `framework.New` when `GOMBIT_JWT_SECRET` is set. Cookie-mode `New` also mounts empty admin Huma routes (ADMIN-1) and the `/admin/` SPA (ADMIN-2); models are added with `admin.Register`. JWT-only apps do not get admin routes. |
| 7. tracing and metrics | Basic trace-ID propagation and HTTP request metrics are owned in M1-7; full OpenTelemetry exporter wiring is future runtime work. |
| 8. HTTP server construction | Owned in M1-2 through `framework.Run` and `RunContext`. |
| 9. middleware installation | Recovery is owned in M1-2. Route-registration composition (independently-registered route groups, each with optional group-scoped middleware) is owned in M1-3 — see [`docs/router.md`](router.md). Request ID, trace context, metrics, security headers, request timeout, and trusted-proxy configuration are owned in M1-7. XSS HTML-tag sanitization of request input is owned in M1-8 (fundamental first-party default on the runtime stack; not covered by security headers alone). CORS, rate limiting, and auth remain separate issues. |
| 10. module registration | Owned in M1-3 — applications register feature routes directly against `app.Router()`; see [`docs/router.md`](router.md). |
| 11. readiness/liveness endpoints | `GET /livez` (liveness) and `GET /readyz` (readiness) are raw Gin probes owned in M1-2. HOST-2 (ADR-015) makes `/readyz` datastore-aware — `503` while draining or when an attached database is unreachable, `200` otherwise — as the stable host health contract. See [`docs/health.md`](health.md). |
| 12. frontend static asset mounting when embedded | Owned in M5-5 through `framework.WithEmbeddedFrontend` (application SPA) and ADMIN-2 through explicit `/admin` Gin routes over `internal/adminui` embed. Application SPA fallback is installed only when the FS has `index.html`; the `gombit new` placeholder embed (`.keep` only) is a no-op so `go run ./cmd/server` works without a Vite `dist`. Admin `/admin/` is cookie-mode only. See [`docs/build.md`](build.md) and [`docs/admin.md`](admin.md). |
| 13. signal handling | Owned in M1-2 through `framework.Run`. |
| 14. graceful shutdown | Owned in M1-2 through bounded `http.Server.Shutdown`; on timeout, `server.Close()` drops remaining connections so `Run`/`RunContext` can return with `Serve` stopped. |
| 15. dependency cleanup | Owned in M1-2 through `OnStop`; concrete DB/cache/log sink cleanup lands with those features. |

The probes are raw Gin routes with D10-style bodies and remain outside
generated OpenAPI when Huma is mounted. `/readyz` is datastore-aware
(HOST-2 / ADR-015); the full contract — bodies, status codes, and how a host
consumes them — is in [`docs/health.md`](health.md).
