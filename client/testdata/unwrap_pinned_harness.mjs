/**
 * #505 against the real dependency: runs the emitted client.ts and its unwrap()
 * with openapi-fetch@0.17.0 — the version `gombit client generate` pins — and a
 * custom fetch, so unwrap is fed the result the library itself returns rather
 * than a hand-written one. That is what makes the fix's premise checkable: a
 * successful response with no body is reported as `data: undefined`, and no
 * status-only rule can tell it apart from a body that was promised.
 *
 * Usage: node unwrap_pinned_harness.mjs <client.ts> <openapi-fetch entry>
 */
import { pathToFileURL } from "node:url";

const [clientPath, entryPath] = process.argv.slice(2);
if (!clientPath || !entryPath) {
  throw new Error("usage: unwrap_pinned_harness.mjs <client.ts> <openapi-fetch entry>");
}

const { unwrap, ContractError } = await import(pathToFileURL(clientPath).href);
const { default: createClient } = await import(pathToFileURL(entryPath).href);

const assert = (condition, message) => {
  if (!condition) throw new Error(message);
};

// A client whose transport answers with the response under test. openapi-fetch
// parses it the way it would parse one off a socket, including its own
// no-content branch.
const call = async (response, method = "GET") => {
  const client = createClient({
    baseUrl: "http://api.test",
    fetch: async () => response,
  });
  const result = await client[method]("/api/v1/products/{id}", {
    params: { path: { id: "7" } },
  });
  return result;
};

// Bodyless success. 204 is the case the issue reported: a Huma output struct
// with no Body answers 204. The library's own report is asserted first, so a
// future openapi-fetch that stops returning `data: undefined` fails here with
// the library's behaviour named instead of breaking unwrap somewhere else.
const noContent = await call(new Response(null, { status: 204 }), "DELETE");
assert(noContent.data === undefined, "openapi-fetch does not report 204 as data: undefined");
assert((await unwrap(noContent)) === undefined, "a 204 success must resolve undefined");

// A HEAD never has a body, whatever the status.
const head = await call(new Response(null, { status: 200 }), "HEAD");
assert(head.data === undefined, "openapi-fetch does not report a HEAD as data: undefined");
assert((await unwrap(head)) === undefined, "a successful HEAD must resolve undefined");

// A 200 that carries a Content-Length of zero.
const zeroLength = await call(
  new Response("", { status: 200, headers: { "content-length": "0" } }),
);
assert(zeroLength.data === undefined, "openapi-fetch does not report a zero-length 200 as undefined");
assert((await unwrap(zeroLength)) === undefined, "a zero-length success must resolve undefined");

// Body-bearing success resolves exactly the parsed body.
const product = { id: 7, sku: "gadget" };
const ok = await call(
  new Response(JSON.stringify(product), {
    status: 200,
    headers: { "content-type": "application/json" },
  }),
);
assert(JSON.stringify(ok.data) === JSON.stringify(product), "openapi-fetch lost the body");
assert(
  JSON.stringify(await unwrap(ok)) === JSON.stringify(product),
  "a success with data must resolve that data",
);

// A non-2xx with a D10 envelope throws the typed ContractError, with the code,
// status, fields and request id off the envelope.
const errorBody = {
  error: {
    code: "not_found",
    message: "No such product",
    fields: { id: "missing" },
    request_id: "req-1",
  },
};
try {
  await unwrap(
    await call(
      new Response(JSON.stringify(errorBody), {
        status: 404,
        headers: { "content-type": "application/json" },
      }),
    ),
  );
  throw new Error("a 404 must throw");
} catch (err) {
  assert(err instanceof ContractError, `not a ContractError: ${err}`);
  assert(err.code === "not_found", `code = ${err.code}, want not_found`);
  assert(err.status === 404, `status = ${err.status}, want 404`);
  assert(err.requestId === "req-1", `requestId = ${err.requestId}, want req-1`);
  assert(JSON.stringify(err.fields) === JSON.stringify({ id: "missing" }), `fields = ${err.fields}`);
}

// A failure the server answers without a D10 body still throws: the status,
// not the parsed result, is what makes it a failure.
try {
  await unwrap(await call(new Response("gateway blew up", { status: 502 })));
  throw new Error("a 502 without a D10 body must throw");
} catch (err) {
  assert(err instanceof ContractError, `not a ContractError: ${err}`);
  assert(err.status === 502, `status = ${err.status}, want 502`);
}

// …and the same for a non-2xx with no body at all, which openapi-fetch reports
// as a success-shaped `{ data: undefined }` only when the response is ok.
try {
  await unwrap(await call(new Response(null, { status: 500 }), "DELETE"));
  throw new Error("a bodyless 500 must throw");
} catch (err) {
  assert(err instanceof ContractError, `not a ContractError: ${err}`);
  assert(err.status === 500, `status = ${err.status}, want 500`);
}

console.log("ok");
