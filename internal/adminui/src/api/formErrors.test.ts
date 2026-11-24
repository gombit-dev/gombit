import { describe, expect, it } from "vitest";

import { ContractError } from "./error";
import { fieldError, unmountedContractMessage } from "./formErrors";

describe("unmountedContractMessage", () => {
  it("names field errors that have no mounted input", () => {
    const err = new ContractError("validation", "The request contains invalid fields.", 422, {
      title: ["is required"],
      owner_id: ["is set by the server and cannot be stored empty"],
      audit_id: ["is set by the server and cannot be stored empty"],
      tenant_id: ["is set by the server and cannot be stored empty"],
    });
    const message = unmountedContractMessage(err, new Set(["title"]));
    expect(message).toContain("owner_id: is set by the server and cannot be stored empty");
    expect(message).toContain("audit_id: is set by the server and cannot be stored empty");
    expect(message).toContain("tenant_id: is set by the server and cannot be stored empty");
    expect(message).not.toContain("title");
  });
});

describe("fieldError", () => {
  it("is the field's messages, or undefined when the error has none", () => {
    const err = { error: { code: "validation_error", message: "x", fields: { mfa_code: ["a multi-factor code is required"] } } };
    expect(fieldError(err, "mfa_code")).toBe("a multi-factor code is required");
    expect(fieldError(err, "email")).toBeUndefined();
    expect(fieldError(new Error("boom"), "mfa_code")).toBeUndefined();
  });
});
