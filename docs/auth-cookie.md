# Cookie/session auth + CSRF (`--auth cookie`)

[M5-3] adds HttpOnly cookie session auth as a first-class alternative to the
[Bearer JWT default](auth.md) (C3 / D3). Select it with `gombit new --auth
cookie`; Bearer JWT (`--auth jwt`) stays the default. The two modes are
mutually exclusive per app — `framework.New` mounts one route shape or the
other from `Config.Auth.EffectiveMode()`, never both.

Behavior lives in the `auth` runtime package (`cookie.go`, `csrf.go`,
`cookie_handlers.go`) alongside the Bearer implementation. Generated apps
wire this through `config.Load` + `framework.WithDatabase`, exactly like
Bearer mode; they do not copy handler code.

## Why cookies at all, given Bearer is the default

HttpOnly cookies remove the access/refresh tokens from page JavaScript
entirely, which closes the token-exfiltration-via-XSS class of attack that
Bearer's in-memory tokens do not (an XSS payload that runs in the page can
still call `fetch` and read whatever the compromised page's JS has access
to, including the in-memory Bearer token). The trade-off is that cookies
reopen CSRF, which Bearer's custom `Authorization` header naturally avoids
(cross-origin forms and `<img>`/`<script>` tags cannot set arbitrary
headers). This mode closes that gap with a signed double-submit token
instead of asking every mutating handler to re-implement it.

## Threat model

| Threat | Mitigation |
| --- | --- |
| XSS reads the session token | Access/refresh cookies are `HttpOnly`; page JavaScript cannot read them via `document.cookie`. |
| Cross-site request forges a mutating request | Every `POST`/`PUT`/`PATCH`/`DELETE` must echo the `gombit_csrf` cookie value as the `X-CSRF-Token` header (double-submit), **except** paths opted out via `framework.WithCSRFExemptPaths` (see [Exempting non-browser endpoints](#exempting-non-browser-endpoints-webhooks)), which must authenticate the caller themselves and must not trust the session cookie. A cross-origin page can trigger a cookie-bearing request but cannot read the CSRF cookie's value (browsers do not expose `Set-Cookie`/`document.cookie` across origins) or set a custom header on a simple cross-origin form submission, so it cannot produce a matching header. |
| Cookie value is guessed or brute-forced | The CSRF token is 32 random bytes (`crypto/rand`) plus an HMAC-SHA256 signature over the token using `Config.Auth.JWTSecret`. `validCSRFTokenValue` checks the signature, not just cookie/header equality, so an attacker who can set *some* cookie on a subdomain ("cookie tossing") still cannot forge a token that verifies without the server secret. |
| Session cookie is stolen off the wire | `Secure` is required in production (enforced by `config.Validate` / `gombit doctor`, see [Config](#config)) so cookies are never sent over plain HTTP once deployed. |
| Session cookie is replayed after logout | Logout revokes the refresh token server-side ([`auth.Service.RevokeRefresh`](../auth/service.go)) and clears both cookies; a captured access JWT still fails once its short TTL (`GOMBIT_JWT_ACCESS_TTL`, default `15m`) expires, same as Bearer mode. |
| Cross-site cookie leakage on navigation | `SameSite=Lax` (default) blocks the cookie on cross-site sub-requests (XHR/fetch/form POST from another site) while still attaching it on top-level navigation (so a bookmarked link still works); `SameSite=Strict` blocks it even on top-level navigation from another site, at the cost of breaking session continuity across an external link into the app. Choose `Strict` for apps with no legitimate cross-site entry points. |
| Reused/rotated refresh token | Identical to Bearer mode: `RotateRefresh` revokes the old token and presenting that revoked token again revokes the whole family (`errRefreshReuse`). Concurrent refresh of the current still-valid token shares one rotation and does not family-revoke the winner. |

This is the **double-submit cookie** pattern, not the synchronizer-token
pattern (no server-side per-session token store beyond the refresh token
table that already exists for rotation). It was chosen because it needs no
additional storage and composes with the existing stateless access-JWT
verification path.

## Cookies

| Cookie | Contents | HttpOnly | Notes |
| --- | --- | --- | --- |
| `gombit_access` | Access JWT (same claims as Bearer mode) | yes | `Path=/`, `MaxAge` = `GOMBIT_JWT_ACCESS_TTL`. |
| `gombit_refresh` | Opaque rotating refresh token | yes | `Path=/`, `MaxAge` = `GOMBIT_JWT_REFRESH_TTL`. |
| `gombit_csrf` | `<random-token>.<hmac-signature>` | **no** | Must be JS-readable so the SPA can echo it as `X-CSRF-Token`; `MaxAge` 24h, reissued whenever a safe request arrives without one. |

All three set `Secure` from `Config.Auth.CookieSecure` and `SameSite` from
`Config.Auth.CookieSameSite` (see [`auth/cookie.go`](../auth/cookie.go)).
None of them are ever written by frontend code — only `Set-Cookie` response
headers from the endpoints below.

## CSRF enforcement

`auth.CSRFMiddleware` is wired as global Gin middleware (not scoped to the
auth routes) whenever `Config.Auth.Enabled()` and `EffectiveMode() ==
AuthModeCookie`, so it covers every state-changing request in the
app — including feature routes added later via `app.Router()` — not just
`/auth/*`, except the exact paths passed to `framework.WithCSRFExemptPaths`
(see [below](#exempting-non-browser-endpoints-webhooks)). See
[`framework/app.go`](../framework/app.go)'s `runtimeMiddlewareStack`.

- **Safe methods** (`GET`, `HEAD`, `OPTIONS`): the middleware ensures a
  signed `gombit_csrf` cookie exists, minting one if missing. It never
  rejects a safe request.
- **Unsafe methods** (`POST`, `PUT`, `PATCH`, `DELETE`): the request must
  carry a `gombit_csrf` cookie **and** an identical `X-CSRF-Token` header,
  and the cookie's value must verify against `Config.Auth.JWTSecret`.
  Otherwise the middleware aborts with a D10 403:

  ```json
  {"error": {"code": "authorization", "message": "csrf token missing or invalid", "request_id": "..."}}
  ```

`GET /auth/csrf` is the bootstrap endpoint: it **reuses a still-valid signed
`gombit_csrf` cookie** and only mints a fresh token when the request carries no
cookie or a forged/invalid one. Re-setting the same value also refreshes the
cookie's `Max-Age`. This is deliberate (#250): double-submit needs a *shared*
signed value, not a per-call-unique one. Minting on every call would rotate the
cookie shared by all tabs, so a second tab's bootstrap would invalidate the
token the first tab holds and permanently 403 its writes. The token is mirrored
in both the `Set-Cookie` header and the JSON body (`{"data": {"csrf_token":
"..."}}`); the SPA reads it from the JS-readable cookie at request time (see
[Generated frontend](#generated-frontend)).

### Exempting non-browser endpoints (webhooks)

A **webhook** or other server-to-server `POST` cannot participate in the
double-submit defense — the caller has no `gombit_csrf` cookie to echo — so in
cookie mode it would always 403. It also usually verifies a **signature over
the raw request body** (e.g. GitHub's `X-Hub-Signature-256` HMAC), and the XSS
input sanitizer re-encodes JSON bodies, which would break that check. Use
`framework.WithRawBodyPaths` for these endpoints — it exempts them from **both**
CSRF and body sanitization:

```go
app, err := framework.New(
    framework.WithConfig(cfg),
    framework.WithDatabase(db),
    framework.WithRawBodyPaths("/api/v1/webhooks/github"),
)
```

Paths match the request path **exactly**, including the API prefix. On such a
path CSRF enforcement is skipped for unsafe methods (safe methods still mint
the cookie) and the request body reaches the handler byte-for-byte unmodified.

`WithCSRFExemptPaths` is the narrower option: it skips CSRF only, leaving the
body sanitizer in place. Use it for a non-browser endpoint that does **not**
hash its raw body; use `WithRawBodyPaths` for signature-verifying webhooks.

**Exempting a path removes a protection; it does not make the route
credential-free.** The session cookies (`gombit_access` / `gombit_refresh`)
are scoped `Path=/`, so a same-site request — including one a cross-site page
triggers — still delivers them to the exempt path. CSRF is exactly what stops a
cross-origin page from riding those *ambient* session cookies on an unsafe
method; drop it and any handler that trusts the session is exposed to forgery
again.

So an exempt handler **must not authenticate via the session cookie**. It must
verify the caller by other means — e.g. an HMAC signature over the raw body
(`X-Hub-Signature-256` for GitHub) — and ignore session identity. CSRF was
never authentication, and exemption is safe **only** for a handler that does
not rely on ambient session auth. Keep the exempt list to the specific
non-browser paths, and never exempt a route that authenticates users by cookie.

## Endpoints

Same paths and D10 envelopes as Bearer mode, but the request/response shape
differs: tokens travel in cookies, not JSON.

| Method | Path | Auth | Notes |
| --- | --- | --- | --- |
| `GET` | `/auth/csrf` | Public, safe (CSRF-exempt) | Reuses a valid `gombit_csrf` cookie (refreshing its `Max-Age`); mints a new one only if missing/invalid. Body mirrors the value as `csrf_token`. |
| `POST` | `/auth/register` | Public, CSRF-protected | Same as Bearer mode: creates a user, never sets `IsSuperuser`. |
| `POST` | `/auth/login` | Public, CSRF-protected | Body `{email, password}`. On success, sets `gombit_access` + `gombit_refresh` cookies; body is the public user, not tokens. |
| `POST` | `/auth/refresh` | Cookie (`gombit_refresh`), CSRF-protected | No request body; reads the refresh cookie. Rotates both session cookies. |
| `POST` | `/auth/logout` | Cookie (`gombit_refresh`), CSRF-protected | No request body; revokes the current refresh token and clears both session cookies. Idempotent. |
| `GET` | `/me` | Cookie (`gombit_access`), safe | Same response shape as Bearer mode's `/me`. Missing or invalid session cookies are 401 D10 `authentication` and **omit** `WWW-Authenticate: Bearer` (RFC 7235: the challenge must name the scheme the resource uses). |

## Config

| Variable | Field | Default | Notes |
| --- | --- | --- | --- |
| `GOMBIT_AUTH_MODE` | `Config.Auth.Mode` | `jwt` | `jwt` or `cookie`. Empty resolves to `jwt` via `EffectiveMode()`. |
| `GOMBIT_COOKIE_SECURE` | `Config.Auth.CookieSecure` | `true` | Must be `true` in `GOMBIT_ENV=production` when `GOMBIT_AUTH_MODE=cookie`; `config.Validate` / `gombit doctor` fail loud otherwise (Appendix C). |
| `GOMBIT_COOKIE_SAMESITE` | `Config.Auth.CookieSameSite` | `lax` | `lax` or `strict`. Empty resolves to `lax` via `EffectiveCookieSameSite()`. |

`GOMBIT_JWT_SECRET` / `GOMBIT_JWT_ACCESS_TTL` / `GOMBIT_JWT_REFRESH_TTL` are
shared with Bearer mode (same access-JWT format; the secret also signs the
CSRF token). See [`docs/auth.md`](auth.md#config) for those and the
production JWT-secret-strength check.

`gombit new --auth cookie` records the choice in `gombit.yaml` and writes
`GOMBIT_AUTH_MODE=cookie` / `GOMBIT_COOKIE_SECURE=true` /
`GOMBIT_COOKIE_SAMESITE=lax` into `.env.example` / the generated `.env`.

## Generated frontend

`frontend/src/auth/session.ts` (cookie variant) tracks only a boolean
"authenticated" flag and an in-memory CSRF mirror — never the session
tokens themselves, which the browser holds as HttpOnly cookies this code
cannot read. `frontend/src/api/client.ts` attaches `X-CSRF-Token` on unsafe
requests by reading the JS-readable `gombit_csrf` cookie **at request time**
(the in-memory value is only a fallback), so a cookie rotated or re-minted by
another tab or after expiry is always matched. It retries once after a silent
`/auth/refresh` on 401, and **once after re-bootstrapping CSRF on a 403** —
clearing the stale mirror, forcing a fresh `GET /auth/csrf`, and replaying the
request. CSRF and refresh `fetch()` URLs go through `apiPath()` so they follow
`GOMBIT_API_PREFIX`. Typed openapi-fetch calls keep `/api/v1/...` path keys;
`rewriteAPIRequest` maps them to the live prefix. Both retries rebuild the
request from buffered body bytes so POST/PATCH JSON survives (buffering is gated
on method; Firefox does not implement the Request.body getter). `RequireAuth`
confirms a session by calling `GET /me`. `LoginPage` warms the cookie with
`bootstrapCSRF()` on mount and **awaits** it before `POST /auth/login` and
`POST /auth/register`. `AppProviders` also fires `bootstrapCSRF()` so a hard
reload on a gated route has a `gombit_csrf` cookie before POST/PATCH/DELETE.
Concurrent callers share one in-flight promise (`csrfInFlight`); `bootstrapCSRF`
is a no-op when the cookie is already present (so concurrent tabs converge on
the one shared cookie), and `force=true` re-fetches for 403 recovery. See the
templates under
[`scaffold/templates/frontend/src`](../scaffold/templates/frontend/src) for
the exact `{{if eq .Auth "cookie"}}` branches.

## Choosing a mode

| | Bearer JWT (`--auth jwt`, default) | Cookie (`--auth cookie`) |
| --- | --- | --- |
| Token exposed to page JS | Yes (in memory) | No (HttpOnly) |
| Needs CSRF defense | No (custom header is not cross-origin-settable) | Yes (built in) |
| Works for non-browser clients (mobile, CLI, service-to-service) | Yes | No (cookie jar + CSRF token dance assumes a browser) |
| Split frontend/backend origins | Works with any `VITE_API_URL` | Needs `credentials: "include"` and a CORS policy that allows credentials from that exact origin (never `*`) |

Bearer stays the v0.1 API default; pick cookie mode for a same-origin (or
credentialed-CORS) browser app where minimizing token exposure to XSS
matters more than the added CSRF plumbing.
