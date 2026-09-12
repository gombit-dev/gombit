# Security headers

Every Gombit response carries a baseline set of security headers, applied by
the runtime `security_headers` middleware. The set is **scoped by response
kind**: a JSON API response and an HTML document have different threat models,
so they get different policies (issue
[#267](https://github.com/gombit-dev/gombit/issues/267) / PERF-9).

## Per-response-kind headers

| Header | API / JSON (default) | HTML: SPA & admin | HTML: `/docs`² |
| --- | --- | --- | --- |
| `Content-Security-Policy` | `default-src 'none'; frame-ancestors 'none'` | SPA policy (see below) | Huma's Swagger UI policy |
| `X-Content-Type-Options` | `nosniff` | `nosniff` | `nosniff` |
| `Strict-Transport-Security` | production only¹ | production only¹ | production only¹ |
| `Referrer-Policy` | — | `strict-origin-when-cross-origin` | `strict-origin-when-cross-origin` |
| `X-Frame-Options` | — | `DENY` | `DENY` |

¹ `max-age=315360000; includeSubDomains`, set only when
`Environment == production`.

² Only the **exact** `/docs` route, and only when docs are enabled
(`API.DocsEnabled`, off by default in production). Huma registers the docs UI at
that one path, not the `/docs/` subtree, so an unknown descendant like
`/docs/not-a-route` — a 404 served by no handler — is an ordinary **API**
response and gets the API policy, as does any `/docs` request when docs are
disabled. Classification follows the route that is actually served, never a URL
prefix.

The **SPA policy** (embedded frontend `index.html` and the admin SPA) is:

```
default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self'
```

It is looser than the API default so `--ui mui` + `--embed` can load Roboto and
Emotion-injected `<style>` tags. `script-src` stays `'self'` — Vite production
JS is hashed same-origin modules, never `'unsafe-inline'`.

## Why the API default is so strict — and so small

A JSON API response renders nothing and loads no sub-resources, so
`default-src 'none'` is the correct policy for it: it forbids everything. The
modern `frame-ancestors 'none'` directive is the clickjacking defense, and it
**subsumes** `X-Frame-Options: DENY`, so the API response needs neither
`X-Frame-Options` nor `Referrer-Policy`. Those are browser-document concerns and
live only on the HTML response kinds above.

Keeping the API set this small is also a measured performance win. The header
value slices are shared, pre-allocated, and read-only (assigned straight into
the response header map), so the values themselves allocate nothing. The cost
was **header count**: correlation IDs (`X-Request-Id`, `X-Trace-Id`) +
`Content-Type` + the security headers. A ninth entry tips the response past
Go's 8-slot swiss-map group, at which point both the handler header map and the
`Header.Clone` net/http performs on `WriteHeader` grow — about 5 allocs/op on
**every** API response. The pre-#267 set (CSP + `Referrer-Policy` + HSTS +
`X-Content-Type-Options` + `X-Frame-Options`, plus a dead
`X-Download-Options`) crossed that boundary. The scoped API set stays at ≤ 6
headers, so the layer allocates nothing. `TestSecurityHeadersLayerAllocatesNothing`
and `TestDefaultJSONResponseStaysUnderHeaderBudget` guard both invariants.

### Header budget

Hold the default API response at **≤ 8 total headers**, including correlation
IDs and `Content-Type`. A route that also sets `Set-Cookie`, `Cache-Control`,
`Link`, etc. can cross the threshold again and reintroduce the map-growth
allocations. If you add headers to a hot API path, keep the total at or under
eight.

## `X-Download-Options` is not set

`X-Download-Options: noopen` was IE8-only guidance for the long-retired IE8
downloads behavior. No supported browser honors it, so it is set on **no**
response — carrying it forward was dead weight (and, being the ninth header,
part of what pushed the API response over the swiss-map threshold).

## Overriding

HTML documents the framework serves through its own handlers (the embedded SPA
and the admin SPA) start from the API baseline and are promoted to the browser
policy via `applyBrowserSecurityHeaders`. Overriding a security header from your
own handler is supported through the standard `http.Header` mutation APIs — use
`c.Header(key, value)` (which replaces the map entry). **Never** write in place
through the slice `Header.Values(key)` returns: those backing slices are shared
process-globals, and an in-place write corrupts the value for every other
in-flight request. `TestSecurityHeaderSharedValueContract` locks this contract.
