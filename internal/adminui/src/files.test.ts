import { describe, expect, it, vi } from "vitest";

import type { FieldMeta, UploadGrant } from "./api/types";
import { acceptAttribute, accepts, asFileValue, fileBodyValue, fileLabel, uploadFile } from "./files";
import { emptyFormValue, formValuesToBody, formatCell, rowToFormValues } from "./fields";

function field(partial: Pick<FieldMeta, "name" | "type"> & Partial<FieldMeta>): FieldMeta {
  return { required: false, readonly: false, ...partial };
}

const cover = field({ name: "cover", type: "image", accept: ["image/png", "image/jpeg"], max_bytes: 100 });
const doc = field({ name: "doc", type: "file", required: true, accept: ["*/*"] });

describe("file values", () => {
  it("reads a row's file object, a bare key, or nothing", () => {
    expect(asFileValue({ key: "k", filename: "a.png" })).toEqual({ key: "k", filename: "a.png" });
    expect(asFileValue("k")).toEqual({ key: "k" });
    expect(asFileValue("")).toBeNull();
    expect(asFileValue(null)).toBeNull();
    expect(fileLabel({ key: "k", filename: "a.png" })).toBe("a.png");
    expect(fileLabel({ key: "k" })).toBe("k");
    expect(fileBodyValue({ key: "k", url: "/x" })).toBe("k");
    expect(fileBodyValue(null)).toBeNull();
  });

  it("sends the key, or null to remove the file", () => {
    const { body } = formValuesToBody({ cover: { key: "c1", filename: "c.png" }, doc: null }, [cover, doc]);
    expect(body).toEqual({ cover: "c1", doc: null });
    expect(emptyFormValue(cover)).toBeNull();
    expect(rowToFormValues({ cover: { key: "c1" } }, [cover]).cover).toEqual({ key: "c1" });
    expect(formatCell({ key: "c1", filename: "c.png" }, cover)).toBe("c.png");
  });

  it("uses the policy as a hint", () => {
    expect(accepts(cover, "image/png")).toBe(true);
    expect(accepts(cover, "text/html")).toBe(false);
    expect(accepts(doc, "application/pdf")).toBe(true);
    expect(accepts(field({ name: "x", type: "image", accept: ["image/*"] }), "image/webp")).toBe(true);
    expect(acceptAttribute(cover)).toBe("image/png,image/jpeg");
    expect(acceptAttribute(doc)).toBeUndefined();
    expect(acceptAttribute(field({ name: "p", type: "image", accept: ["*/*"] }))).toBe("image/*");
  });
});

describe("uploadFile", () => {
  const grant: UploadGrant = {
    key: "papers/cover/abc",
    upload: { method: "PUT", url: "/_storage/papers/cover/abc?signature=s", headers: { "Content-Type": "image/png" } },
  };

  it("asks for a grant, sends the bytes with it, and returns the file", async () => {
    const client = { uploadGrant: vi.fn().mockResolvedValue({ data: grant }) };
    const send = vi.fn().mockResolvedValue(new Response(null, { status: 200 }));
    const file = new File([new Uint8Array(10)], "c.png", { type: "image/png" });
    const got = await uploadFile(client, "papers", cover, file, send);
    expect(client.uploadGrant).toHaveBeenCalledWith("papers", "cover", { size: 10, content_type: "image/png", filename: "c.png" });
    expect(send).toHaveBeenCalledWith(grant.upload.url, { method: "PUT", headers: { "Content-Type": "image/png" }, body: file });
    expect(got).toEqual({ key: "papers/cover/abc", filename: "c.png", size: 10, content_type: "image/png" });
  });

  it("refuses what the policy will refuse, before asking", async () => {
    const client = { uploadGrant: vi.fn() };
    await expect(uploadFile(client, "papers", cover, new File([new Uint8Array(101)], "big.png", { type: "image/png" }))).rejects.toThrow(/larger than 100/);
    await expect(uploadFile(client, "papers", cover, new File(["<html>"], "a.html", { type: "text/html" }))).rejects.toThrow(/not accepted/);
    expect(client.uploadGrant).not.toHaveBeenCalled();
  });

  it("fails when the storage refuses the bytes", async () => {
    const client = { uploadGrant: vi.fn().mockResolvedValue({ data: grant }) };
    const send = vi.fn().mockResolvedValue(new Response(null, { status: 403 }));
    await expect(uploadFile(client, "papers", cover, new File(["x"], "c.png", { type: "image/png" }), send)).rejects.toThrow(/403/);
  });
});
