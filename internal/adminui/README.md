# Framework-owned admin SPA (ADMIN-2 + ADMIN-3)

Vite + React + TypeScript app served from `go:embed` under `/admin/`.
Feature packages still only call `admin.Register` — there are **no**
per-model React files.

## Rebuild the committed `dist/`

`dist/` is committed so `go test` and `go run ./examples/admin` work
without npm at compile time (`go:embed` cannot use `..` and cannot embed
an empty directory).

```sh
cd internal/adminui
npm ci
npm test
npm run typecheck
npm run build
```

Commit the resulting `dist/` with the source change. CI (and the release
workflow) rebuilds it and fails on any difference (the "Fail on admin UI
dist drift" step), so a source change that was not rebuilt cannot reach a
binary (issue #451). Do not copy this tree into generated `frontend/`.
Do not add `--admin` to `gombit make resource`.

`base` is `/admin/` so hashed assets are `/admin/assets/…`. Auth is
cookie + CSRF only. Tokens and CSRF stay out of `localStorage` /
`sessionStorage`. `VITE_API_URL` is origin-only (empty = same-origin).
The API path prefix is injected at serve time from `config.API.Prefix`
(`__GOMBIT_API_PREFIX__` in `index.html`); do not set it via `VITE_*`.
