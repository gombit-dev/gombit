/**
 * Runtime contract for #505: unwrap resolves whatever openapi-fetch reported
 * for a successful response — including a success with no body — and throws
 * the D10 ContractError for anything else.
 *
 * It runs the bytes the generator emits (the test writes client.ts and
 * error.ts from the templates into a temporary directory next to a stub
 * openapi-fetch package: unwrap never calls createClient, so the stub only
 * has to exist for the module graph to resolve). Every response below is a
 * real Response object; the { data, response } pairs are the shape
 * openapi-fetch@0.17.0 returns for the status noted on each case.
 *
 * Usage: node unwrap_harness.mjs /path/to/client.ts
 */
import { pathToFileURL } from "node:url";

const clientPath = process.argv[2];
if (!clientPath) {
  throw new Error("usage: unwrap_harness.mjs <client.ts>");
}

const { unwrap, ContractError } = await import(pathToFileURL(clientPath).href);
if (typeof unwrap !== "function") {
  throw new Error("client.ts must export unwrap");
}

const json = (body, init) =>
  new Response(JSON.stringify(body), {
    headers: { "content-type": "application/json" },
    ...init,
  });

const assert = (condition, message) => {
  if (!condition) throw new Error(message);
};

// The reported case: a Huma output struct with no Body answers 204.
assert(
  (await unwrap({ data: undefined, response: new Response(null, { status: 204 }) })) === undefined,
  "a 204 success must resolve undefined",
);

// The same runtime representation for every other bodyless success, which is
// what made the 204-only special case wrong: a HEAD (2xx with no body by
// definition), a zero-length 200, a 201 with no body.
for (const response of [
  new Response(null, { status: 200 }),
  new Response("", { status: 200, headers: { "content-length": "0" } }),
  new Response(null, { status: 201 }),
]) {
  assert(
    (await unwrap({ data: undefined, response })) === undefined,
    `a successful ${response.status} with no content must resolve undefined`,
  );
}

// A success with content resolves exactly that content.
const product = { id: 7, sku: "gadget" };
assert(
  (await unwrap({ data: product, response: json(product) })) === product,
  "a success with data must resolve that data",
);

// A non-2xx still throws the D10 ContractError with the code, status, fields
// and request id from the envelope.
const errorBody = {
  error: {
    code: "not_found",
    message: "No such product",
    fields: { id: "missing" },
    request_id: "req-1",
  },
};
try {
  await unwrap({ data: undefined, error: errorBody, response: json(errorBody, { status: 404 }) });
  throw new Error("a non-2xx must throw");
} catch (err) {
  assert(err instanceof ContractError, `not a ContractError: ${err}`);
  assert(err.code === "not_found" && err.status === 404, `got code=${err.code} status=${err.status}`);
  assert(err.requestId === "req-1", `requestId = ${err.requestId}, want req-1`);
}

// A non-2xx whose body is not a D10 envelope still throws, with the fallback
// code — it does not resolve undefined.
try {
  await unwrap({ data: undefined, response: new Response("gateway blew up", { status: 502 }) });
  throw new Error("a non-2xx without a D10 body must throw");
} catch (err) {
  assert(err instanceof ContractError, `not a ContractError: ${err}`);
  assert(err.status === 502, `status = ${err.status}, want 502`);
}

// A result that carries an error is not a success even when the status looks
// like one (openapi-fetch reports the parse failure this way).
try {
  await unwrap({
    data: undefined,
    error: { message: "network" },
    response: new Response(null, { status: 200 }),
  });
  throw new Error("a result carrying error must throw");
} catch (err) {
  assert(err instanceof ContractError, `not a ContractError: ${err}`);
}

console.log("ok");
