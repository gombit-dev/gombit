# Application-Owned Route Registration

M1-3 de-domains the router introduced in M1-2: `framework.New` builds a
`*gin.Engine` with zero knowledge of any application domain and mounts only
its own endpoints. Today that means `/livez`, `/readyz`, `/metrics`, the Huma
OpenAPI routes (`/openapi.json` and siblings), `/docs` when
`API.DocsEnabled` is true, and — when `GOMBIT_JWT_SECRET` is set and a
database is attached — the Bearer auth routes (`/api/v1/auth/*`, `/api/v1/me`).
Public API handlers register on `app.API()` (see [`docs/contract.md`](contract.md));
raw Gin routes continue to use `app.Router()`.

Applications register their own routes against the appropriate surface:

```go
app, err := framework.New()
if err != nil {
    return err
}

registerProductRoutes(app.API(), app.Config().API.Prefix) // contract / OpenAPI
registerWebhookRoutes(app.Router())                         // escape hatch

return framework.Run(app)
```

Each `registerXRoutes` function is the runtime equivalent of a feature
package's `routes.go` (build plan §3.2 /
`.cursor/skills/create-feature/references/layout.md`): registration is
**explicit**, called from `main`, and never discovered through reflection
(principle 6.2). `examples/router` demonstrates raw Gin groups; `examples/contract`
demonstrates Huma-typed registration with D10 validation errors.

## No module/registry abstraction

Gombit does not provide a `Module` type, a route registry, or any
composition layer beyond `*gin.Engine` itself. `app.Router()` already is the
idiomatic mechanism: `router.Group(prefix)` gives a feature its own route
group and its own group-scoped middleware, and two features registered this
way cannot interfere with each other. Adding a bespoke abstraction on top
would duplicate what Gin already does well and cut against the "no reflection
discovery" principle — see `framework/router_test.go`'s
`TestApplicationOwnedRouteRegistrationComposesIndependently`, which proves
two independently-registered groups compose without cross-module leakage.

## Middleware ordering

Framework middleware installed before `New` returns wraps every route
registered afterward, including application route groups and their own
group-scoped middleware. Order is:

```text
Recovery
  -> request ID
  -> trace context
  -> request metrics
  -> security headers
  -> XSS HTML-tag sanitization (request input)
  -> request timeout
  -> Bearer JWT middleware on protected Huma operations (`GET /me`)
  -> feature group middleware (if any)
    -> feature handler
```

XSS sanitization is a fundamental security default (M1-8): response headers
alone are not enough. The runtime strips HTML tags from JSON string fields
(POST/PUT/PATCH) and GET query values using a first-party sanitizer built on
`golang.org/x/net/html`.

**Why first-party (not the template wrapper):** the template's
`pkg/middleware/xss.go` is a thin wrapper around
`gin-gonic-xss-middleware` (Bluemonday). M1-8 keeps the *behavior*
(strip HTML tags from request input before handlers) but does not take that
Gin wrapper or Bluemonday as a dependency — both were rejected for hygiene
(stale/unmaintained surface). The framework owns a small sanitizer on
`golang.org/x/net/html`, which was already in the module graph. This is an
intentional, documented divergence from extract-preserve for that one package.

Other behavior notes:

- The `password` exemption is an **exact, case-sensitive** JSON/query key
  match (`password` only). `Password` and other casings are still sanitized.
- Invalid JSON is passed through so Gin/Huma can return normal validation
  errors.
- Non-JSON bodies (form/multipart) are not sanitized in v0.1; the public API
  path is JSON/Huma. Form/multipart coverage can land before browser/admin
  session work if needed.
- Sanitization re-marshals JSON, so key order and whitespace may change.
  Callers that hash the raw body must hash the bytes handlers actually see —
  which a webhook can't, since it verifies a signature over the *original*
  bytes. Mark such paths with
  [`framework.WithRawBodyPaths`](auth-cookie.md#exempting-non-browser-endpoints-webhooks):
  they skip sanitization entirely (and the 8MiB cap below), so the body reaches
  the handler byte-for-byte, and they are CSRF-exempt too.
- Unclosed dangerous elements (for example a truncated `<script>…`) strip the
  tag itself but keep the text that follows; only the content of a *properly
  closed* dangerous element is discarded. That recovered text is re-parsed, so
  tags smuggled inside the unclosed element are stripped too and never reach
  handlers.
- Incomplete angle brackets that are not a complete HTML tag (no closing `>`,
  e.g. a product name `a<b`) are left unchanged — the HTML tokenizer would
  otherwise treat `"<"+letter` as a start tag and silently shorten the string.
  This applies to the submitted value itself, not to text recovered from
  inside an unclosed dangerous element. That text is unparsed markup rather
  than something the user typed, so it is re-parsed in full: every `"<"` +
  letter through the next `>` is treated as a tag and removed, which can drop
  more than a stray bracket — `<script>if (a<b && c>d) return` yields
  `if (ad) return`. Complete tags (`<b>hi</b>`, `<script>…</script>`) are
  always stripped.
- JSON sanitizer buffering is capped at 8MiB. Larger JSON bodies abort with
  HTTP 413 and a D10 error envelope (`payload_too_large`) and never reach
  handlers. `http.Server.ReadTimeout` matches `GOMBIT_HTTP_REQUEST_TIMEOUT`
  (`0` disables it). The request-timeout middleware is a context deadline; it
  does not abort `Body.Read`. The connection read deadline and the sanitizer
  cap are the brakes on a slow or never-ending JSON body (#137).

Canonical design order (draft §13.3) also includes CORS, body-size limit, rate
limiting, and auth context. Those remain deferred; when a first-class body-size
middleware lands it still inserts immediately before XSS. The 8MiB XSS cap is
only a bound on sanitizer buffering, not that middleware.

Request IDs use the `X-Request-Id` header. If the caller provides one, the
runtime preserves it; otherwise it generates one and stores it on both Gin's
context and `c.Request.Context()` for downstream code:

```go
requestID := framework.GetRequestID(c)
requestIDFromContext := framework.GetRequestIDFromContext(c.Request.Context())
```

Trace context currently preserves the W3C `Traceparent` trace ID when present
and exposes the active trace ID through `X-Trace-Id`,
`framework.GetTraceID(c)`, and `framework.GetTraceIDFromContext(ctx)`. Full
OpenTelemetry exporter wiring remains future runtime work; M1-7 preserves the
runtime seam and parity tests.

`/metrics` exposes Prometheus text-format request counters, active request
gauge, and request-duration sums for the runtime router, labelled by `method`,
`route`, and `status`. All three labels are bounded so a remote caller cannot
inflate series cardinality: `route` is the matched route pattern (or
`unmatched`), and `method` is limited to the standard HTTP methods with any
other token bucketed as `other` (the raw request method is otherwise an
unbounded client-supplied value). The text renderer is intentionally minimal
for M1 runtime parity; a later observability issue can swap in
`prometheus/client_golang` or full OpenTelemetry exporter wiring without
changing the route contract. Trusted proxies are configured through
`config.Config.HTTP.TrustedProxies` or
`GOMBIT_HTTP_TRUSTED_PROXIES`; when unset, Gin ignores forwarded-client IP
headers and uses the direct TCP peer.

`framework.WithRouter` is the custom-router escape hatch. When an application
passes its own `*gin.Engine`, Gombit applies trusted-proxy configuration only;
the application owns recovery, request ID, trace context, metrics, security
headers, XSS HTML sanitization, and timeout middleware for that router.

`framework/router_test.go`'s `TestDefaultRouterMountsOnlyFrameworkEndpoints`
and `TestApplicationOwnedRouteRegistrationComposesIndependently` cover this;
`framework/app_test.go`'s `TestDefaultRouterRecoversFromPanics` covers
Recovery still applying to an application-registered route.

## What is not here yet

Contract DTOs, validation → D10 field errors, and `app.API()` are documented in
[`docs/contract.md`](contract.md). This router surface still does not include
CORS, rate limiting, or authentication middleware:

- auth middleware: M5

OpenAPI emission, `/docs`, and `gombit openapi generate` are documented in
[`docs/openapi.md`](openapi.md).

Until then, an application that needs middleware can add it directly via
`app.Router().Use(...)` or on its own route groups; the framework will not
silently reorder or override it.

`examples/router` posts JSON to `/echo` and returns the comment the handler
saw after XSS sanitization.
