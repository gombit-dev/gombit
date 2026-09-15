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
  -> request context (request ID + W3C trace context, and — when
     GOMBIT_HTTP_REQUEST_TIMEOUT > 0 — the per-handler deadline; #268)
  -> request metrics
  -> security headers
  -> request body size limit (JSON POST/PUT/PATCH; 413 over 8MiB, always on)
  -> XSS HTML-tag sanitization (request input; only when GOMBIT_SECURITY_SANITIZE_INPUT=true)
  -> Bearer JWT middleware on protected Huma operations (`GET /me`)
  -> feature group middleware (if any)
    -> feature handler
```

The **request timeout is opt-in** (issue #270 / PERF-12). The framework default
is `0`, which disables the per-handler deadline. The deadline lives inside the
`request_context` middleware (#268 folded it in — there is no separate timeout
layer), and that middleware does no timeout work when the value is `0`: no
timer, nothing added on the request path. Set
`GOMBIT_HTTP_REQUEST_TIMEOUT` (scaffolded apps set `60s`) to install it; the
deadline then propagates into the request context and any DB/cache call that
honors it. The `http.Server` `ReadHeaderTimeout`/`ReadTimeout`/`WriteTimeout`/
`IdleTimeout` remain the connection-level safety net either way. Trade-off: with
the per-handler deadline off, a slow handler keeps running after `WriteTimeout`
closes the connection, and a long-running DB query is not cancelled unless the
app enables the timeout or sets its own deadline. See
`docs/adr/017-request-timeout-opt-in.md`.

**Input sanitization is opt-in** (issue
[#271](https://github.com/gombit-dev/gombit/issues/271) / PERF-13). By default a
Gombit app **does not** rewrite request input, so the values in
`{"description":"x < y"}` or a literal `<b>bold</b>` reach handlers intact. XSS
is an output-encoding concern, and the controls Gombit ships are on output — the
React frontend escapes text (JSX), the framework admin is a React SPA that
renders values as text (there is no server-rendered `html/template` admin path),
a JSON response is not an HTML sink, and the response CSP is a backstop.
Stripping markup on ingress instead corrupts those values, besides costing
allocations on every write request. See
[security.md § Input sanitization](security.md#input-sanitization-opt-in) for
the posture and [ADR-018](adr/018-input-sanitization-opt-in.md).

Set `Security.SanitizeInput` (`GOMBIT_SECURITY_SANITIZE_INPUT=true`) to install
the sanitizer. It strips HTML tags from JSON string fields (POST/PUT/PATCH) and
GET query values using a first-party sanitizer built on `golang.org/x/net/html`.
For a single field, call `framework.SanitizeHTML(s)` from a handler instead of
turning the whole pipeline back on. The rest of this section describes the
behavior of that opt-in layer.

**Why first-party (not the template wrapper):** the template's
`pkg/middleware/xss.go` is a thin wrapper around `gin-gonic-xss-middleware`
(Bluemonday). The framework keeps the *behavior* (strip HTML tags from request
input) but does not take that Gin wrapper or Bluemonday as a dependency — both
were rejected for hygiene (stale/unmaintained surface). The framework owns a
small sanitizer on `golang.org/x/net/html`, which was already in the module
graph. This is an intentional, documented divergence from extract-preserve for
that one package.

Other behavior notes (they describe the opt-in layer):

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
- JSON request bodies are capped at 8MiB by the always-on **request body size
  limit** layer (`request_body_limit`), independent of whether sanitization is
  enabled. A larger JSON `POST`/`PUT`/`PATCH` body aborts with HTTP 413 and a
  D10 error envelope (`payload_too_large`) before any handler runs — including a
  raw `app.Router()` handler that calls `ShouldBindJSON` — and a streamed body
  is bounded mid-read by `http.MaxBytesReader`. `WithRawBodyPaths` are exempt
  (a signature-verifying webhook keeps its exact bytes). The `http.Server`
  read/write/idle timeouts are a separate, time-based safety net (they take
  `GOMBIT_HTTP_REQUEST_TIMEOUT` when set and fall back to 60s otherwise);
  a context deadline does not abort `Body.Read`, so the size cap and the
  connection read deadline are the brakes on a slow or never-ending JSON
  body (#137).

Canonical design order (draft §13.3) also includes CORS, rate limiting, and
auth context; those remain deferred. The request body size limit above is the
fixed 8MiB default cap — a first-class, per-route configurable body-size
middleware is still future work, and would slot in at the same position.

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
