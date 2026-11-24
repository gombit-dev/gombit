# TypeScript client example

Sample OpenAPI 3.1 document for the contract widget API, plus the output of
`gombit client generate`. Typed errors use the D10 envelope.

## Generate

From the repository root, regenerate the committed spec and client from
`client.SampleApp()` (same path CI uses):

```sh
go run ./cmd/gombit client check --write --spec examples/client/openapi.json --out examples/client/frontend/src/api/generated
```

`go generate ./client` does the same rewrite. To generate only the TypeScript
client from the committed spec:

```sh
go run ./cmd/gombit client generate \
  --spec examples/client/openapi.json \
  --out examples/client/frontend/src/api/generated
```

`gombit client check` (without `--write`) reports drift without touching files.
Whitespace-only JSON is not drift; an extra or changed Huma route is.

## Typecheck

```sh
cd examples/client
npm install
npx tsc --noEmit
```

## Use

```ts
import {
  ContractError,
  createGombitClient,
  unwrap,
} from "./frontend/src/api/generated/client";

const client = createGombitClient({
  baseUrl: "http://127.0.0.1:8080",
  getAccessToken: () => undefined,
});

const widgets = await unwrap(await client.GET("/api/v1/widgets"));
```

The Vite + React skeleton that wires this client into forms is documented
in [`docs/frontend.md`](../../docs/frontend.md).
