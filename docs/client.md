# TypeScript client generation

`gombit client generate` turns an OpenAPI 3.1 document into TypeScript types
and a thin `openapi-fetch` wrapper. Typed errors map to the D10 envelope.
This is D5: `openapi-typescript` + `openapi-fetch`, generation-only.

## Generate

```sh
go run ./cmd/gombit client generate \
  --spec openapi.json \
  --out frontend/src/api/generated
```

`--spec` defaults to `openapi.json`. `--out` defaults to
`frontend/src/api/generated` (design §23.3). The command prints created or
modified files and reminds you to add `openapi-fetch@0.17.0`. Schema types
come from `openapi-typescript@7.13.0`.

Path keys in `schema.ts` are normalized to D8 `/api/v1/...` even when the
spec (and the live Huma API) uses another `GOMBIT_API_PREFIX`. The
generator rewrites a temporary copy; it does **not** change the OpenAPI
document on disk. Scaffolded pages call `client.GET("/api/v1/...")`;
`createAppClient` maps that typed prefix to the live one on the wire.

`--dry-run` prints the file list without writing and applies the same
overwrite rule as a real run: a user-owned file (no generated banner) is
refused unless `--force` is also set. `--force` overwrites a file that was
not produced by Gombit. Re-running replaces only files that carry the
generated banner.

Write the spec first with [`gombit openapi generate`](openapi.md) (or copy
`examples/client/openapi.json`).

## Output

| File | Role |
| --- | --- |
| `schema.ts` | Types from `openapi-typescript` |
| `error.ts` | D10 `error.{code,message,fields,request_id}` helpers |
| `client.ts` | `createGombitClient` + `unwrap` over `openapi-fetch` |

Do not hand-edit these files. Access tokens are supplied through
`getAccessToken` and stay in memory. Generated source never reads
`localStorage` or `sessionStorage`.

```ts
import {
  ContractError,
  createGombitClient,
  isD10ErrorBody,
  unwrap,
} from "./api/generated/client";

const client = createGombitClient({
  baseUrl: import.meta.env.VITE_API_URL ?? "",
  getAccessToken: () => accessToken,
});

const listed = await unwrap(await client.GET("/api/v1/widgets"));

try {
  await unwrap(await client.POST("/api/v1/widgets", { body: { name: "" } }));
} catch (err) {
  if (err instanceof ContractError) {
    // err.code, err.fields, err.requestId, err.status
  }
}

if (isD10ErrorBody(body)) {
  mapFieldsIntoForm(body.error.fields);
}
```

`VITE_API_URL` is public. Do not put secrets in `VITE_*` values.

## Example

`examples/client` ships a sample spec (the contract widget API) and the
generated client. See that README to typecheck it.

## Contract drift check

`gombit client check` regenerates the OpenAPI document and TypeScript client
and fails if committed artifacts would change. The document it compares
against comes from, in order: `--url` (fetched over HTTP), an in-process
`huma.API` (only available to Go callers inside this repository, such as
`client.SampleApp()`), or `client.SampleApp()` itself. `--spec` and `--out`
default to `openapi.json` and `frontend/src/api/generated` — the same
defaults as `client generate` — so a bare `gombit client check --url ...` is
meant to run inside a **generated app**, not this repository.

**In a generated app**, with the API running (`gombit dev` or a deployed
instance):

```sh
gombit client check --url http://127.0.0.1:8080/openapi.json
```

`gombit` is a separately compiled binary there, so it has no Go-level
`huma.API` to compare against — `--url` is required.

**In this repository**, the drift check instead guards
`examples/client/openapi.json` and `examples/client/frontend/src/api/generated`
against `client.SampleApp()`, so it needs explicit paths:

```sh
go run ./cmd/gombit client check --spec examples/client/openapi.json --out examples/client/frontend/src/api/generated
go run ./cmd/gombit client check --write --spec examples/client/openapi.json --out examples/client/frontend/src/api/generated
```

`--write` rewrites those two paths. CI runs the `--write` form and then
`git diff --exit-code` on them, so a Huma handler change to `SampleApp` that
is not regenerated fails the job. Spec comparison in `CheckDrift` is semantic
(`encoding/json`); whitespace-only JSON is not drift.

`go generate ./client` runs the same rewrite as the explicit-path `--write`
form above.

## React skeleton

`gombit new` wires this client into a Vite + React app (router, providers,
React Hook Form). See [frontend.md](frontend.md). `--ui mui` is documented
in [frontend-mui.md](frontend-mui.md). Bearer login is M5-2.
