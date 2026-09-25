import { describe, expect, it } from "vitest";

import type { FieldMeta } from "./api/types";
import { emptyFormValue, formValuesToBody, relationListQuery, relationOptions } from "./fields";

function field(partial: Pick<FieldMeta, "name" | "type"> & Partial<FieldMeta>): FieldMeta {
  return {
    required: false,
    readonly: false,
    ...partial,
  };
}

describe("formValuesToBody", () => {
  it("includes emptied optional string/text/date/datetime/json as null", () => {
    const fields: FieldMeta[] = [
      field({ name: "note", type: "string" }),
      field({ name: "body", type: "text" }),
      field({ name: "due", type: "date" }),
      field({ name: "published_at", type: "datetime" }),
      field({ name: "payload", type: "json" }),
    ];
    const { body, jsonErrors } = formValuesToBody(
      { note: "", body: null, due: undefined, published_at: "", payload: "  " },
      fields,
    );
    expect(jsonErrors).toEqual({});
    expect(body).toEqual({
      note: null,
      body: null,
      due: null,
      published_at: null,
      payload: null,
    });
  });

  it("includes required empties as null so the server can 422", () => {
    const fields: FieldMeta[] = [field({ name: "name", type: "string", required: true })];
    const { body, jsonErrors } = formValuesToBody({ name: "" }, fields);
    expect(jsonErrors).toEqual({});
    expect(body).toEqual({ name: null });
  });

  it("sends null for emptied optional numbers and keeps 0", () => {
    const fields: FieldMeta[] = [
      field({ name: "price", type: "integer" }),
      field({ name: "weight", type: "float" }),
      field({ name: "qty", type: "decimal" }),
    ];
    const cleared = formValuesToBody({ price: "", weight: null, qty: undefined }, fields);
    expect(cleared.jsonErrors).toEqual({});
    expect(cleared.body).toEqual({ price: null, weight: null, qty: null });

    // integer/float stay JSON numbers; decimal is an exact string (never a
    // float, which the data plane rejects for a decimal column).
    const zeros = formValuesToBody({ price: 0, weight: "0", qty: 0 }, fields);
    expect(zeros.jsonErrors).toEqual({});
    expect(zeros.body).toEqual({ price: 0, weight: 0, qty: "0" });
  });

  it("sends decimal as an exact string and rejects non-numeric text", () => {
    const fields: FieldMeta[] = [field({ name: "amount", type: "decimal" })];
    const ok = formValuesToBody({ amount: "19.9999" }, fields);
    expect(ok.jsonErrors).toEqual({});
    expect(ok.body).toEqual({ amount: "19.9999" });

    const bad = formValuesToBody({ amount: "1.2.3" }, fields);
    expect(bad.jsonErrors).toHaveProperty("amount");
  });

  it("always sends booleans, including false", () => {
    const fields: FieldMeta[] = [field({ name: "active", type: "boolean" })];
    const { body, jsonErrors } = formValuesToBody({ active: false }, fields);
    expect(jsonErrors).toEqual({});
    expect(body).toEqual({ active: false });
  });

  it("keeps invalid JSON as jsonErrors and omits that key", () => {
    const fields: FieldMeta[] = [
      field({ name: "payload", type: "json" }),
      field({ name: "note", type: "string" }),
    ];
    const { body, jsonErrors } = formValuesToBody({ payload: "{", note: "" }, fields);
    expect(jsonErrors).toEqual({ payload: "must be valid JSON" });
    expect(body).toEqual({ note: null });
    expect(body).not.toHaveProperty("payload");
  });

  it("starts from a declared default and still submits an explicit zero", () => {
    const fields: FieldMeta[] = [
      field({ name: "count", type: "integer", default: "1", minimum: "0" }),
      field({ name: "active", type: "boolean", default: "true" }),
      field({ name: "status", type: "string", default: "draft" }),
      field({ name: "note", type: "text", pattern: "^[a-z]+$" }),
    ];
    expect(emptyFormValue(fields[0])).toBe(1);
    expect(emptyFormValue(fields[1])).toBe(true);
    expect(emptyFormValue(fields[2])).toBe("draft");

    const explicit = formValuesToBody({ count: 0, active: false, status: "published", note: "hello" }, fields);
    expect(explicit.jsonErrors).toEqual({});
    expect(explicit.body).toEqual({ count: 0, active: false, status: "published", note: "hello" });

    const bad = formValuesToBody({ count: 0, active: false, status: "published", note: "Hello" }, fields);
    expect(bad.jsonErrors).toEqual({ note: "does not match pattern" });
  });

  it("does not put readonly fields in the body", () => {
    const fields: FieldMeta[] = [
      field({ name: "id", type: "integer", readonly: true }),
      field({ name: "note", type: "text" }),
    ];
    const { body } = formValuesToBody({ id: 3, note: "" }, fields);
    expect(body).toEqual({ note: null });
  });
});

describe("relationListQuery", () => {
  it("sends search only when the related model is searchable", () => {
    expect(relationListQuery(true, "north", 100)).toEqual({ per_page: 100, search: "north" });
    // not searchable → no search param (it would be a no-op that hides rows)
    expect(relationListQuery(false, "north", 100)).toEqual({ per_page: 100 });
    // empty term → no search param
    expect(relationListQuery(true, "", 100)).toEqual({ per_page: 100 });
  });
});

describe("relationOptions (picker options)", () => {
  const rows = [
    { id: 1, title: "North" },
    { id: 2, title: "South" },
  ];

  it("labels rows by the label_field JSON key", () => {
    expect(relationOptions(rows, "id", "title")).toEqual([
      { value: "1", label: "North" },
      { value: "2", label: "South" },
    ]);
  });

  it("falls back to the primary key when the label field is not a row key", () => {
    // label_field must be a field name (json key), not a SQL column: a mismatch
    // falls back to the pk instead of crashing.
    expect(relationOptions(rows, "id", "name")).toEqual([
      { value: "1", label: "1" },
      { value: "2", label: "2" },
    ]);
  });

  it("supports uuid / string primary keys", () => {
    const uuidRows = [{ uid: "6f9619ff-8b86", title: "North" }];
    expect(relationOptions(uuidRows, "uid", "title")).toEqual([
      { value: "6f9619ff-8b86", label: "North" },
    ]);
  });
});

describe("belongs_to fields", () => {
  const rel = (): FieldMeta => ({
    name: "warehouse_id",
    type: "relation",
    required: false,
    readonly: false,
    related: { slug: "warehouses", kind: "belongs_to", label_field: "name" },
  });

  it("sends the selected foreign key, preserving numeric type", () => {
    const { body } = formValuesToBody({ warehouse_id: "7" }, [rel()]);
    expect(body.warehouse_id).toBe(7);
  });

  it("keeps a uuid / string foreign key as a string", () => {
    const uuid = "6f9619ff-8b86-d011-b42d-00c04fc964ff";
    const { body } = formValuesToBody({ warehouse_id: uuid }, [rel()]);
    expect(body.warehouse_id).toBe(uuid);
  });

  it("sends null to clear an optional belongs_to", () => {
    const { body } = formValuesToBody({ warehouse_id: "" }, [rel()]);
    expect(body.warehouse_id).toBeNull();
  });
});

describe("many_to_many fields", () => {
  const rel = (): FieldMeta => ({
    name: "warehouses",
    type: "relation",
    required: false,
    readonly: false,
    related: { slug: "warehouses", kind: "many_to_many", label_field: "name" },
  });

  it("submits the selected ids as a numeric list", () => {
    const { body } = formValuesToBody({ warehouses: ["1", "2", "3"] }, [rel()]);
    expect(body.warehouses).toEqual([1, 2, 3]);
  });

  it("submits an empty list to clear the relation", () => {
    const { body } = formValuesToBody({ warehouses: [] }, [rel()]);
    expect(body.warehouses).toEqual([]);
  });

  it("preserves string / uuid ids instead of dropping them", () => {
    const uuid = "6f9619ff-8b86-d011-b42d-00c04fc964ff";
    const { body } = formValuesToBody({ warehouses: [uuid, "abc"] }, [rel()]);
    expect(body.warehouses).toEqual([uuid, "abc"]);
  });

  it("drops only null / undefined / empty entries", () => {
    const { body } = formValuesToBody({ warehouses: ["4", "", null, 5] }, [rel()]);
    expect(body.warehouses).toEqual([4, 5]);
  });
});
