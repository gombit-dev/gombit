import type { FieldMeta, FileValue, UploadGrant } from "./api/types";

// A file or image field (storage-backed): its value is a FileValue, sent to
// the API as its key.
export function isFileField(field?: FieldMeta): boolean {
  return field?.type === "file" || field?.type === "image";
}

// asFileValue reads a row's or the form's value of a file field.
export function asFileValue(value: unknown): FileValue | null {
  if (value && typeof value === "object" && typeof (value as FileValue).key === "string") {
    return value as FileValue;
  }
  if (typeof value === "string" && value !== "") {
    return { key: value };
  }
  return null;
}

// fileLabel is how a file shows: its filename, or its key.
export function fileLabel(value: unknown): string {
  const file = asFileValue(value);
  if (!file) {
    return "";
  }
  return file.filename || file.key;
}

// fileBodyValue is a file field's value in a request body: the key, or
// null to clear it.
export function fileBodyValue(value: unknown): string | null {
  return asFileValue(value)?.key ?? null;
}

// accepts reports whether the field's policy (as a hint) takes a file of
// this type; the server decides from the bytes.
export function accepts(field: FieldMeta, type: string): boolean {
  const patterns = field.accept ?? [];
  if (patterns.length === 0) {
    return true;
  }
  const [major] = type.split("/");
  return patterns.some((p) => p === "*/*" || p === type || p === `${major}/*`);
}

// acceptAttribute is the file input's accept attribute for the field.
export function acceptAttribute(field: FieldMeta): string | undefined {
  const patterns = field.accept ?? [];
  if (patterns.length === 0 || patterns.includes("*/*")) {
    return field.type === "image" ? "image/*" : undefined;
  }
  return patterns.join(",");
}

type Granter = {
  uploadGrant: (slug: string, field: string, body: { size: number; content_type: string; filename: string }) => Promise<{ data: UploadGrant }>;
};

// uploadFile uploads file for a field directly to storage: the admin
// grants the upload (checking the declared size and type), the bytes go
// with the grant's request, and the result is the value to send.
export async function uploadFile(
  client: Granter,
  slug: string,
  field: FieldMeta,
  file: File,
  send: typeof fetch = fetch,
): Promise<FileValue> {
  if (field.max_bytes !== undefined && file.size > field.max_bytes) {
    throw new Error(`The file is larger than ${field.max_bytes} bytes.`);
  }
  const contentType = file.type || "application/octet-stream";
  if (!accepts(field, contentType)) {
    throw new Error(`A ${contentType} file is not accepted here.`);
  }
  const granted = await client.uploadGrant(slug, field.name, {
    size: file.size,
    content_type: contentType,
    filename: file.name,
  });
  const grant = granted.data;
  const sent = await send(grant.upload.url, {
    method: grant.upload.method,
    headers: grant.upload.headers ?? {},
    body: file,
  });
  if (!sent.ok) {
    throw new Error(`The upload failed (${sent.status}).`);
  }
  return { key: grant.key, filename: file.name, size: file.size, content_type: contentType };
}

// freshFileFields are the file fields whose value in current is not the one
// the form loaded (initial): uploads made for this save. A save that fails
// for any reason but a field error may have discarded them on the server
// (a failed write abandons the uploads it named), so the form drops them
// and asks for the files again rather than resubmit dead keys.
export function freshFileFields(fields: FieldMeta[], current: Record<string, unknown>, initial: Record<string, unknown>): string[] {
  return fields
    .filter((field) => isFileField(field))
    .filter((field) => asFileValue(current[field.name])?.key !== asFileValue(initial[field.name])?.key)
    .map((field) => field.name);
}
