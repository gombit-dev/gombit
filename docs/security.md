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

## Input sanitization (opt-in)

Gombit does **not** sanitize request input by default (issue
[#271](https://github.com/gombit-dev/gombit/issues/271) / PERF-13; ADR-018). The
default middleware no longer mutates JSON string or query **values**, so
`{"description":"x < y"}` and `{"note":"<b>bold</b>"}` reach handlers with their
values intact. (This is value fidelity, not byte-for-byte fidelity: the response
envelope re-encodes JSON, so whitespace, key order, and number spelling may
differ. A handler that needs the exact original bytes — a webhook verifying a
signature — reads the raw body via
[`framework.WithRawBodyPaths`](auth-cookie.md#exempting-non-browser-endpoints-webhooks).)

This is deliberate. **XSS is an output-encoding problem, not an input problem.**
The defense is escaping data at the point it is rendered — and the controls
Gombit actually ships are all on output:

- The generated React frontend escapes text by default (JSX interpolation), and
  the framework admin is a React SPA that renders values as React text, never as
  HTML — there is no server-rendered `html/template` admin path.
- A JSON API response is not an HTML sink: a browser does not execute markup in
  an `application/json` body.
- The response `Content-Security-Policy` (see above) is a backstop that blocks
  inline script execution even on the framework's own HTML pages.

Stripping HTML tags on ingress is the wrong layer: it silently mutates data the
application may need to store faithfully (a code snippet, a math expression like
`a < b`, legitimate markup a downstream consumer renders in a safe context), it
gives a false sense of safety (it does not encode for the *output* context that
actually matters), and it costs allocations on every write request. The default
posture leaves your data intact and puts the XSS defense where it belongs.

**When to enable ingress stripping.** If your app renders stored values into an
HTML context you do not control — or you want defense-in-depth for a specific
untrusted field — you have two opt-ins:

- **Per app:** set `Security.SanitizeInput` (`GOMBIT_SECURITY_SANITIZE_INPUT=true`).
  This installs the legacy sanitizer middleware: JSON string values
  (POST/PUT/PATCH) and GET query values are stripped to plain text before
  handlers run, the exact key `password` is exempt, and
  [`framework.WithRawBodyPaths`](auth-cookie.md#exempting-non-browser-endpoints-webhooks)
  paths (webhooks that verify a signature over the raw body) are skipped
  entirely. See [router.md](router.md#middleware-ordering) for the layer's exact
  behavior.
- **Per field:** call `framework.SanitizeHTML(s string) string` from a handler.
  It applies the same stripping to one value without turning the whole request
  pipeline back on — the right tool when only one field is untrusted.

Enabling `Security.SanitizeInput` changes behavior: request input is rewritten,
and JSON bodies are re-encoded (key order and whitespace may change), which is
why signature-verifying webhook paths must be marked raw. It does not change
anything on the **output** side — that was never sanitization's job.
