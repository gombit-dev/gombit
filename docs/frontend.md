# Frontend (minimal React skeleton)

`gombit new` writes a Vite + React + TypeScript app under `frontend/`.
The default UI is **minimal/headless** (C4). `--ui mui` scaffolds the
opt-in MUI CRUD preset documented in [frontend-mui.md](frontend-mui.md).
Bearer login, refresh rotation, and protected routes are documented in
[auth.md](auth.md); this page describes that (default) `--auth jwt`
wiring. `--auth cookie` swaps `auth/session.ts`, `api/client.ts`,
`auth/RequireAuth.tsx`, and `pages/LoginPage.tsx` for the cookie/CSRF
variants documented in [auth-cookie.md](auth-cookie.md). Auth behavior is
independent of the UI preset: `--auth cookie --ui mui` still has CSRF
double-submit **and** MUI screens.

See also [cli.md](cli.md) (`gombit new`, `gombit dev`, `gombit build --embed`),
[build.md](build.md) (collectstatic + SPA fallback), and
[client.md](client.md) (TypeScript client generation).

## Stack

- Vite + `@vitejs/plugin-react`
- React + TypeScript
- React Router (`BrowserRouter`)
- generated `openapi-typescript` + `openapi-fetch` client
- React Hook Form with D10 `error.fields` mapping

Package manager D6: the scaffolded `package.json` works with npm (CI uses
Node 22). `gombit dev` prefers pnpm when it is available.

## Layout

```text
frontend/src/
├── main.tsx
├── app/
│   ├── providers.tsx   # API client context (ThemeProvider when --ui mui)
│   └── router.tsx      # /login, RequireAuth, /, /products/new
├── api/
│   ├── client.ts       # createAppClient + 401 refresh + useApiClient
│   ├── apiPrefix.ts    # runtime GOMBIT_API_PREFIX (not baked at gombit new)
│   ├── retry.ts        # buffer POST/PATCH body; rebuild 401 retry init
│   ├── formErrors.ts   # D10 fields → RHF setError
│   └── generated/      # schema.ts, client.ts, error.ts
├── auth/
│   ├── session.ts      # in-memory access + refresh tokens
│   └── RequireAuth.tsx # redirect anonymous users to /login
├── layouts/
│   └── AppLayout.tsx
├── pages/
│   ├── LoginPage.tsx
│   ├── ProductListPage.tsx
│   └── ProductFormPage.tsx
├── theme.ts            # only with --ui mui
└── resources.tsx       # rewritten by gombit make resource
```

## Talking to the API

The home page calls `unwrap(client.GET("/api/v1/products"))`. Create uses
`client.POST("/api/v1/products", { body })`. Those strings are **OpenAPI
path keys** from the generated placeholder client (D8 default `/api/v1`)
— do not hand-write DTOs.

`createAppClient` rewrites that typed `/api/v1` prefix to the live
`config.API.Prefix` / `GOMBIT_API_PREFIX` on the way out
(`rewriteAPIRequest` in `src/api/apiPrefix.ts`). Changing `.env` and
restarting does **not** require regenerating frontend **page** source.
`gombit client generate` / `gombit dev` rewrite live Huma paths in a
temporary spec copy to `/api/v1` before `openapi-typescript`, so
`schema.ts` keys stay `/api/v1/...` and `tsc` still accepts the
scaffolded calls. The committed `openapi.json` keeps the live prefix
(contract-drift compares that document). Cookie-mode CSRF/refresh
`fetch()` URLs go through `apiPath("/auth/csrf")` the same way.

The prefix is injected at serve time, matching the admin SPA, **only**
when Gombit serves `index.html`:

- `gombit build --embed`: Gin replaces `__GOMBIT_API_PREFIX__` in
  `index.html` when it serves the SPA.
- `gombit dev`: Vite's `transformIndexHtml` plugin does the same from
  `GOMBIT_API_PREFIX` (passed in the child environment). Production Vite
  builds **leave the placeholder** so embed can inject the live value.

**Split deploy (C5, the default) does not inject automatically.** A CDN
or static host is not Gin. After `vite build`, replace
`__GOMBIT_API_PREFIX__` in `dist/index.html` with the live prefix before
uploading, or set `window.__GOMBIT_API_PREFIX__` to the same value.
Leaving the placeholder makes `apiPrefix()` fall back to `/api/v1`. Do
not put the prefix in `VITE_*`.

```sh
# same token Gin substitutes for gombit build --embed
sed "s|__GOMBIT_API_PREFIX__|${GOMBIT_API_PREFIX:-/api/v1}|g" \
  dist/index.html > dist/index.html.tmp && mv dist/index.html.tmp dist/index.html
```

`createGombitClient` `baseUrl` is `import.meta.env.VITE_API_URL` (public).
Empty means same-origin so the Vite `/api` proxy used by `gombit dev`
works. For a split deploy, set the API **origin only** (for example
`http://127.0.0.1:8080`); OpenAPI path keys stay `/api/v1/...` and
`rewriteAPIRequest` maps them to the prefix injected (or substituted) in
`index.html`. Prefixes that still start with `/api` (such as `/api/v2`)
hit the existing `/api` proxy during `gombit dev`; a prefix that does not
(`/svc/v2`) gets an extra Vite proxy entry. Vite also proxies `/docs` and
`/admin` so interactive docs and the cookie-mode admin SPA are reachable on
the frontend origin.

`VITE_*` values are baked into the browser bundle. Never put JWT secrets,
database passwords, or other server credentials there. Do not put
`GOMBIT_API_PREFIX` in `VITE_*` — that would freeze it at Vite build
time.

## Access token (in memory)

`src/auth/session.ts` holds the access and refresh tokens in module
variables. `getAccessToken` is passed into `createGombitClient`.
`createAppClient` attaches `Authorization: Bearer` and, on 401, calls
`POST /auth/refresh` once using the in-memory refresh token (typed
OpenAPI path `/api/v1/auth/refresh`, rewritten to the live prefix).
Concurrent 401s wait on that refresh and retry instead of returning the
stale failure. The retry rebuilds the request from buffered body bytes
(gated on method, not the Request.body getter — unimplemented in Firefox)
rather than cloning the consumed `Request`, so POST/PATCH JSON survives
silent refresh. `RequireAuth` sends anonymous users to `/login`. Logout
clears memory and revokes the refresh token. Generated source never reads
`localStorage` or `sessionStorage`. See [auth.md](auth.md).

## D10 field errors

`src/api/formErrors.ts` maps `ContractError.fields` or a D10 error body
onto React Hook Form:

```ts
try {
  await unwrap(await client.POST("/api/v1/products", { body }));
} catch (err: unknown) {
  if (!applyContractErrors(setError, err)) {
    // non-field error (message / request_id)
  }
}
```

A body like `{"error":{"code":"validation_error","message":"...","fields":{"name":["required"]}}}`
calls `setError("name", { type: "server", message: "required" })`. Do not
invent another error shape.

Minimal `type="number"` inputs use React Hook Form `setValueAs` so a
cleared field becomes `0`, not `NaN` (`JSON.stringify` would emit
`null` and Huma would 422 a non-pointer Go int). An optional pointer
number (such as a nullable foreign key) submits `null` instead. The MUI preset does
the same with `raw === "" ? 0 : Number(raw)`.

## Generated client placeholder

`frontend/src/api/generated` ships a placeholder product contract so
`npm run typecheck` / `npm run build` succeed right after `gombit new`.
Those files carry the generated banner. `gombit client generate` and
`gombit dev` overwrite them from the live OpenAPI 3.1 document.

After `gombit make resource`, regenerate the client so new paths exist:

```sh
gombit client generate --spec openapi.json
# or
gombit dev
```

`gombit make resource` honors `ui:` in `gombit.yaml`. Default (`minimal`
or missing) stays headless; `ui: mui` emits MUI Table/TextField pages.

## Split vs embed (C5)

Split deploy is the default. The Vite app is a separate origin; set
`VITE_API_URL` to the API origin for production. Optional single-binary
deploy is `gombit build --embed`: Vite production build, collectstatic into
`internal/web/static`, `go:embed`, `go build ./cmd/server`. The binary
serves API + static assets + `index.html` SPA fallback. After `gombit new`,
`go run ./cmd/server` still works without a Vite `dist` — the placeholder
embed has no `index.html`, so unknown paths stay 404.

See [build.md](build.md) and [cli.md](cli.md#gombit-build---embed).
