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
// the form loaded (initial): uploads (or removals) made for this save.
export function freshFileFields(fields: FieldMeta[], current: Record<string, unknown>, initial: Record<string, unknown>): string[] {
  return fields
    .filter((field) => isFileField(field))
    .filter((field) => asFileValue(current[field.name])?.key !== asFileValue(initial[field.name])?.key)
    .map((field) => field.name);
}

// DroppedUpload is a file field to put back after a failed save: its loaded
// value, and the message to show (none when the field was only cleared, or
// already carries the server's own error).
export type DroppedUpload = { name: string; value: unknown; message: string | null };

// uploadsToDrop is what the form does to its file fields after a save
// fails, whatever the failure: the server may have discarded the uploads it
// named (a failed write abandons them, field errors raised inside the write
// included), so every fresh one is put back to the loaded value, and the
// operator is asked for the file again, rather than resubmit a dead key.
// errored names the fields that already show a server error.
export function uploadsToDrop(
  fields: FieldMeta[],
  current: Record<string, unknown>,
  initial: Record<string, unknown>,
  errored: Set<string>,
): DroppedUpload[] {
  return freshFileFields(fields, current, initial).map((name) => ({
    name,
    value: initial[name] ?? null,
    message:
      asFileValue(current[name]) && !errored.has(name)
        ? "The upload was discarded by the failed save; choose the file again."
        : null,
  }));
}
