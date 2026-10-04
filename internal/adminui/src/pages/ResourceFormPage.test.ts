import { describe, expect, it } from "vitest";

import formSrc from "./ResourceFormPage.tsx?raw";

describe("ResourceFormPage", () => {
  it("does not GET or PATCH an edit form unless the row can be hydrated", () => {
    expect(formSrc).toMatch(/canPopulateEditForm\(model\)/);
    expect(formSrc).toMatch(/if \(mode === "edit" && !rowLoaded\) \{/);
    expect(formSrc).toMatch(/const values = rowToFormValues\(envelope\.data, model\.fields\);\s*loaded\.current = values;\s*reset\(values\);\s*setRowLoaded\(true\);/s);
    expect(formSrc).toMatch(/unmountedContractMessage\(err, mounted\)/);
    expect(formSrc).toMatch(/if \(orphan\) \{\s*setStatus\(orphan\);/s);
  });
  it("drops the uploads of any failed save, field errors included", () => {
    const catchBlock = formSrc.slice(formSrc.indexOf("} catch (err: unknown) {"));
    expect(catchBlock).toMatch(/for \(const drop of uploadsToDrop\(model\.fields, getValues\(\), loaded\.current, errored\)\)/);
    expect(catchBlock).not.toMatch(/if \(!fieldErrors\)/);
  });
});
