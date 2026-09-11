# OpenAPI emission and docs

Gombit emits OpenAPI 3.1 from Huma-typed handlers. The live document is served
at `/openapi.json`. A FastAPI-style interactive docs UI is served at `/docs`
in local and test environments. `gombit openapi generate` writes that document
to disk.

## Live surfaces

| Surface | URL | Role |
| --- | --- | --- |
| OpenAPI document | `/openapi.json` | Machine-readable contract (CLI, CI, codegen) |
| Interactive docs | `/docs` | Swagger UI backed by `/openapi.json`, with try-it-out against the same server |

`examples/contract` registers Huma widget routes. After `go run ./examples/contract`:

```sh
curl -sS http://127.0.0.1:8080/openapi.json
# open http://127.0.0.1:8080/docs
```

Try-it-out requests hit the running app, so D10 validation and category errors
appear as they would for any client. The docs page uses Huma's Swagger UI
renderer and loads the UI assets from `unpkg.com`; Huma sets a page-specific
CSP so try-it-out can call the same origin, and the framework adds the browser
hardening headers (`Referrer-Policy`, `X-Frame-Options`) on top. JSON API
routes keep the strict API policy `default-src 'none'; frame-ancestors 'none'`
(see [security.md](security.md)).

Raw `app.Router()` routes (webhooks, SSE, probes, metrics) stay out of the
OpenAPI document and therefore out of `/docs`.

## Production

Docs are on by default in `development` and `test`. `config.Load` turns them
off in `production` unless `GOMBIT_DOCS_ENABLED=true`. `/openapi.json` stays
available so codegen and drift checks still work.

| Variable | Field | Default |
| --- | --- | --- |
| `GOMBIT_DOCS_ENABLED` | `Config.API.DocsEnabled` | `true` outside production; `false` in production when unset |

`config.Default()` is the development preset (`DocsEnabled` true). Changing
only `Environment` on that value does not turn docs off. Use
`config.DefaultFor(env)` or set `API.DocsEnabled` when passing
`framework.WithConfig`. `config.Load` / `LoadFromEnv` apply
`DefaultDocsEnabled` when `GOMBIT_DOCS_ENABLED` is unset.
`contract.HumaConfigFor(title, version, docsEnabled)` is the Huma helper
`framework.New` uses.

## Write the spec to disk

With the app running:

```sh
go run ./cmd/gombit openapi generate \
  --url http://127.0.0.1:8080/openapi.json \
  --out openapi.json
```

`--url` defaults to `http://127.0.0.1:8080/openapi.json`. `--out` defaults to
`openapi.json`. The CLI validates the document as OpenAPI 3.1 before writing.

In-process generation (tests, `go:generate`) uses the same helpers:

```go
spec, err := contract.OpenAPIJSON(app.API())
if err != nil {
    return err
}
return contract.WriteOpenAPI("openapi.json", app.API())
```

`contract.OpenAPIJSON` is semantically identical to `GET /openapi.json` (same
document; `OpenAPIJSON` pretty-prints, Huma's route does not). The CLI writes
the fetched live bytes. Whitespace-only differences are not contract drift:
`gombit client check` compares JSON semantically, and CI always rewrites the
sample fixture with `contract.WriteOpenAPI` so formatting stays stable.

## Contract drift check

CI regenerates the sample widget spec and TypeScript client in-process from
`client.SampleApp()` (no running server, no `gombit openapi generate --url`)
and fails if the committed files would change. An intentional Huma handler
change without regenerating those fixtures fails CI.

From the repository root:

```sh
# Report drift without writing (whitespace-only JSON is not drift)
go run ./cmd/gombit client check

# Rewrite examples/client/openapi.json and the generated TypeScript client
go run ./cmd/gombit client check --write

# Same rewrite via go generate
go generate ./client
```

`CheckDrift` is the reusable Go entry point. See [`docs/client.md`](client.md).
