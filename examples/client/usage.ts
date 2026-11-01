import {
  ContractError,
  createGombitClient,
  isD10ErrorBody,
  unwrap,
} from "./frontend/src/api/generated/client";

const client = createGombitClient({
  baseUrl: "http://127.0.0.1:8080",
  getAccessToken: () => undefined,
});

export async function listWidgets() {
  const result = await client.GET("/api/v1/widgets");
  if (result.error) {
    if (isD10ErrorBody(result.error)) {
      const code: string = result.error.error.code;
      const fields = result.error.error.fields;
      void code;
      void fields;
    }
    throw ContractError.fromResponse(result.response, result.error);
  }
  return unwrap(result);
}

export async function createWidget() {
  return unwrap(
    await client.POST("/api/v1/widgets", {
      body: { name: "Second widget", color: "green" },
    }),
  );
}
