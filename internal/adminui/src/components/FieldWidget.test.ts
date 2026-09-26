import { describe, expect, it } from "vitest";

import widgetSrc from "./FieldWidget.tsx?raw";

describe("FieldWidget", () => {
  it("shows a server error on the write-only boolean control", () => {
    const branch = widgetSrc.slice(widgetSrc.indexOf("field.writeonly"));
    expect(branch).toMatch(/fieldState\.error/);
    expect(branch).toMatch(/FormHelperText/);
  });
});
