import { describe, expect, it } from "vitest";

import loginSrc from "./LoginPage.tsx?raw";

// The login answers the multi-factor challenge: a 422 on mfa_code (a field
// the first form does not render) must show a code field and send the code,
// never be swallowed as an error on an unmounted field.
describe("LoginPage multi-factor challenge", () => {
  it("shows a code field on a 422 on mfa_code", () => {
    expect(loginSrc).toMatch(/fieldError\(err, "mfa_code"\)/);
    expect(loginSrc).toMatch(/setMfaRequired\(true\)/);
    expect(loginSrc).toMatch(/name="mfa_code"/);
  });
  it("sends the code with the login once asked", () => {
    expect(loginSrc).toMatch(/client\.login\(values\.email, values\.password, mfaRequired \? values\.mfa_code/);
  });
  it("surfaces field errors it does not render", () => {
    expect(loginSrc).toMatch(/unmountedContractMessage\(err, mounted\)/);
    expect(loginSrc).toMatch(/if \(orphan\) \{\s*setStatus\(orphan\);/s);
  });
});
