import type { FieldMeta, Row } from "./api/types";

export function isHasMany(field: FieldMeta): boolean {
  return field.type === "relation" && field.related?.kind === "has_many";
}

export function isManyToMany(field: FieldMeta): boolean {
  return field.type === "relation" && field.related?.kind === "many_to_many";
}

export function isBelongsTo(field: FieldMeta): boolean {
  const kind = field.related?.kind;
  return field.type === "relation" && (kind === "belongs_to" || kind === "one_to_one");
}

/** RelId is a related primary key: a number (integer PK) or a string (uuid /
 * string PK). */
export type RelId = number | string;

/** toRelId coerces a single related primary key, preserving its type (a numeric
 * id stays a number; a uuid / string id stays a string). */
export function toRelId(raw: unknown): RelId {
  if (typeof raw === "number") {
    return raw;
  }
  const s = String(raw).trim();
  const n = Number(s);
  return s !== "" && !Number.isNaN(n) ? n : s;
}

/**
 * toIdList coerces a form value into the id list a many_to_many field submits.
 * It preserves the related primary key type: a numeric id stays a number, a
 * non-numeric id (uuid / string PK) stays a string. Only null / undefined /
 * empty entries are dropped — a string id is never silently coerced away (which
 * would wipe the join table for string-keyed related models).
 */
export function toIdList(raw: unknown): RelId[] {
  if (!Array.isArray(raw)) {
    return [];
  }
  const out: RelId[] = [];
  for (const v of raw) {
    if (v === null || v === undefined) {
      continue;
    }
    if (typeof v === "number") {
      if (!Number.isNaN(v)) {
        out.push(v);
      }
      continue;
    }
    const s = String(v).trim();
    if (s === "") {
      continue;
    }
    const n = Number(s);
    out.push(Number.isNaN(n) ? s : n);
  }
  return out;
}

/** RelationOption is a picker choice: the primary key (as a string value) and a
 * human label. */
export type RelationOption = { value: string; label: string };

/**
 * relationOptions maps related list rows to picker options. The label comes from
 * the `label_field` key of the row JSON — which is the related model's field
 * name (json tag / registered name), not its SQL column — falling back to the
 * primary key when the label field is absent.
 */
export function relationOptions(rows: Row[], pkField: string, labelField: string): RelationOption[] {
  return rows
    .filter((r) => r[pkField] != null)
    .map((r) => {
      const value = String(r[pkField]);
      return {
        value,
        label: labelField && r[labelField] != null ? String(r[labelField]) : value,
      };
    });
}

/**
 * relationListQuery builds the list query for a relation picker page. The
 * `search` term is only sent when the related model actually supports search
 * (has a configured Search); otherwise the picker filters the loaded page
 * client-side and a stray `search` would be a no-op that hides valid rows.
 */
export function relationListQuery(
  searchable: boolean,
  search: string,
  pageSize: number,
): Record<string, string | number> {
  const query: Record<string, string | number> = { per_page: pageSize };
  if (searchable && search) {
    query.search = search;
  }
  return query;
}

export function isWritable(field: FieldMeta): boolean {
  return !field.readonly && !isHasMany(field);
}

export function writableFields(fields: FieldMeta[]): FieldMeta[] {
  return fields.filter(isWritable);
}

export function formatCell(value: unknown, field?: FieldMeta): string {
  if (value === null || value === undefined) {
    return "";
  }
  if (typeof value === "boolean") {
    return value ? "yes" : "no";
  }
  if (typeof value === "object") {
    try {
      return JSON.stringify(value);
    } catch {
      return String(value);
    }
  }
  const text = String(value);
  const label = field?.choices?.find((choice) => choice.value === text)?.label;
  return label ?? text;
}

export function emptyFormValue(field: FieldMeta): unknown {
  if (field.default !== undefined && field.default !== "") {
    if (field.type === "boolean") {
      return field.default === "true";
    }
    if (field.type === "integer" || field.type === "float") {
      return Number(field.default);
    }
    return field.default;
  }
  if (field.type === "boolean") {
    return false;
  }
  if (isManyToMany(field) || isHasMany(field)) {
    return [];
  }
  if (field.type === "json") {
    return "";
  }
  return "";
}

export function rowToFormValues(row: Row, fields: FieldMeta[]): Row {
  const values: Row = {};
  for (const field of fields) {
    const raw = row[field.name];
    values[field.name] = valueToForm(field, raw);
  }
  return values;
}

export function formValuesToBody(values: Row, fields: FieldMeta[]): { body: Row; jsonErrors: Record<string, string> } {
  const body: Row = {};
  const jsonErrors: Record<string, string> = {};
  for (const field of writableFields(fields)) {
    const raw = values[field.name];
    if (isManyToMany(field)) {
      // Send the id list so the data plane syncs the join table. An empty
      // list clears the relation; omission is not possible from the form.
      body[field.name] = toIdList(raw);
      continue;
    }
    if (isBelongsTo(field)) {
      // Send the selected foreign key (preserving its type), or null to clear.
      body[field.name] = isEmptyFormValue(raw) ? null : toRelId(raw);
      continue;
    }
    if (field.type === "json") {
      const text = raw === undefined || raw === null ? "" : String(raw).trim();
      if (text === "") {
        // Include null so PATCH can clear an optional JSON column. Omit
        // would leave the previous value (applyWrite is key-present).
        body[field.name] = null;
        continue;
      }
      try {
        body[field.name] = JSON.parse(text) as unknown;
      } catch {
        jsonErrors[field.name] = "must be valid JSON";
      }
      continue;
    }
    if (field.type === "boolean") {
      body[field.name] = Boolean(raw);
      continue;
    }
    const constraint = constraintError(field, raw);
    if (constraint) {
      jsonErrors[field.name] = constraint;
      continue;
    }
    if (field.type === "decimal") {
      // Decimals are submitted as an exact string, never a JSON number: a JSON
      // body decodes numbers to float64 on the server, which cannot represent a
      // decimal exactly (the data plane rejects a float for a decimal column).
      if (isEmptyFormValue(raw)) {
        body[field.name] = null;
        continue;
      }
      const text = String(raw).trim();
      if (!/^-?\d+(\.\d+)?$/.test(text)) {
        jsonErrors[field.name] = "must be a decimal";
        continue;
      }
      body[field.name] = text;
      continue;
    }
    if (field.type === "integer" || field.type === "float") {
      if (isEmptyFormValue(raw)) {
        body[field.name] = null;
        continue;
      }
      const n = Number(raw);
      if (Number.isNaN(n)) {
        jsonErrors[field.name] = "must be a number";
        continue;
      }
      body[field.name] = field.type === "integer" ? Math.trunc(n) : n;
      continue;
    }
    if (isEmptyFormValue(raw)) {
      body[field.name] = null;
      continue;
    }
    if (field.type === "datetime") {
      body[field.name] = datetimeLocalToRFC3339(String(raw));
      continue;
    }
    if (field.type === "date") {
      // ADMIN-1 asDate expects YYYY-MM-DD; datetime-local is out of band here.
      body[field.name] = String(raw).trim().slice(0, 10);
      continue;
    }
    if (field.type === "time") {
      body[field.name] = padClock(String(raw).trim());
      continue;
    }
    body[field.name] = raw;
  }
  return { body, jsonErrors };
}

// padClock turns an HTML time value HH:MM into HH:MM:SS. The request format
// is the full clock.
function padClock(value: string): string {
  return value.length === 5 ? `${value}:00` : value;
}

// formPattern is the pattern compiled with the u flag. `.` outside a class is
// `[^\n]`, matching RE2: one code point, every character except newline.
export function formPattern(pattern: string): string {
  let out = "";
  let inClass = false;
  for (let i = 0; i < pattern.length; i++) {
    const c = pattern[i];
    if (c === "\\") {
      out += c;
      if (i + 1 < pattern.length) {
        i++;
        out += pattern[i];
      }
      continue;
    }
    if (c === "[") {
      inClass = true;
    } else if (inClass && c === "]") {
      inClass = false;
    }
    if (c === "." && !inClass) {
      out += "[^\\n]";
      continue;
    }
    out += c;
  }
  return out;
}

function constraintError(field: FieldMeta, raw: unknown): string {
  if (isEmptyFormValue(raw)) {
    return "";
  }
  if (field.max_length && [...String(raw)].length > field.max_length) {
    return "is too long";
  }
  if (field.pattern && !new RegExp(formPattern(field.pattern), "u").test(String(raw))) {
    return "does not match pattern";
  }
  if ((field.minimum || field.maximum) && (field.type === "integer" || field.type === "float")) {
    const n = Number(raw);
    if (Number.isNaN(n)) {
      return "must be a number";
    }
    if (field.minimum !== undefined && field.minimum !== "" && n < Number(field.minimum)) {
      return `must be at least ${field.minimum}`;
    }
    if (field.maximum !== undefined && field.maximum !== "" && n > Number(field.maximum)) {
      return `must be at most ${field.maximum}`;
    }
  }
  if ((field.minimum || field.maximum) && field.type === "decimal") {
    const text = String(raw).trim();
    if (!/^-?\d+(\.\d+)?$/.test(text)) {
      return "must be a decimal";
    }
    if (field.minimum && compareDecimal(text, field.minimum) < 0) {
      return `must be at least ${field.minimum}`;
    }
    if (field.maximum && compareDecimal(text, field.maximum) > 0) {
      return `must be at most ${field.maximum}`;
    }
  }
  return "";
}

/** compareDecimal reports the magnitude order of two decimal strings. */
export function compareDecimal(a: string, b: string): number {
  const norm = (raw: string) => {
    let t = raw.trim();
    let neg = false;
    if (t.startsWith("-")) {
      neg = true;
      t = t.slice(1);
    }
    const parts = t.split(".");
    const ip = (parts[0] || "0").replace(/^0+(?=\d)/, "");
    const fp = (parts[1] || "").replace(/0+$/, "");
    if (ip === "0" && fp === "") neg = false;
    return { neg, ip, fp };
  };
  const left = norm(a);
  const right = norm(b);
  if (left.neg !== right.neg) {
    return left.neg ? -1 : 1;
  }
  const sign = left.neg ? -1 : 1;
  if (left.ip.length !== right.ip.length) {
    return sign * (left.ip.length < right.ip.length ? -1 : 1);
  }
  if (left.ip !== right.ip) {
    return sign * (left.ip < right.ip ? -1 : 1);
  }
  if (left.fp !== right.fp) {
    return sign * (left.fp < right.fp ? -1 : 1);
  }
  return 0;
}

function isEmptyFormValue(raw: unknown): boolean {
  return raw === "" || raw === undefined || raw === null;
}

/**
 * Convert a `datetime-local` value to RFC3339 for ADMIN-1 `asDateTime`.
 *
 * `<input type="datetime-local">` submits `YYYY-MM-DDTHH:mm` (optional
 * seconds, no offset). The Go converter only accepts RFC3339 / RFC3339Nano.
 * Wall time is treated as the browser's local zone; the result is ISO-8601
 * with a `Z` offset (`Date.toISOString()`). Values that already carry `Z`
 * or a numeric offset are left unchanged. Date-only fields stay YYYY-MM-DD
 * in `formValuesToBody` and never go through this helper.
 *
 * Examples:
 *   `2026-08-18T12:30`     → `2026-08-18T...Z` (local → UTC)
 *   `2026-08-18T12:30:00Z` → unchanged
 */
export function datetimeLocalToRFC3339(raw: string): string {
  const trimmed = raw.trim();
  if (trimmed === "") {
    return trimmed;
  }
  if (/[Zz]$/.test(trimmed) || /[+-]\d{2}:\d{2}$/.test(trimmed)) {
    return trimmed;
  }
  const match = trimmed.match(
    /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2})(?:\.(\d+))?)?$/,
  );
  if (match) {
    const year = Number(match[1]);
    const month = Number(match[2]);
    const day = Number(match[3]);
    const hour = Number(match[4]);
    const minute = Number(match[5]);
    const second = match[6] ? Number(match[6]) : 0;
    const millis = match[7] ? Number(match[7].slice(0, 3).padEnd(3, "0")) : 0;
    const date = new Date(year, month - 1, day, hour, minute, second, millis);
    if (!Number.isNaN(date.getTime())) {
      return date.toISOString();
    }
    const ss = String(second).padStart(2, "0");
    return `${match[1]}-${match[2]}-${match[3]}T${match[4]}:${match[5]}:${ss}Z`;
  }
  const parsed = new Date(trimmed);
  if (!Number.isNaN(parsed.getTime())) {
    return parsed.toISOString();
  }
  const withSeconds = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(trimmed) ? `${trimmed}:00` : trimmed;
  return withSeconds.endsWith("Z") ? withSeconds : `${withSeconds}Z`;
}

function valueToForm(field: FieldMeta, raw: unknown): unknown {
  if (field.type === "boolean") {
    return Boolean(raw);
  }
  if (isManyToMany(field) || isHasMany(field)) {
    return toIdList(raw);
  }
  if (field.type === "json") {
    if (raw === undefined || raw === null || raw === "") {
      return "";
    }
    if (typeof raw === "string") {
      return raw;
    }
    try {
      return JSON.stringify(raw, null, 2);
    } catch {
      return String(raw);
    }
  }
  if (field.type === "datetime" && typeof raw === "string") {
    return datetimeLocalValue(raw);
  }
  if (field.type === "date" && typeof raw === "string") {
    return raw.slice(0, 10);
  }
  if (raw === undefined || raw === null) {
    return emptyFormValue(field);
  }
  return raw;
}

function datetimeLocalValue(raw: string): string {
  const date = new Date(raw);
  if (Number.isNaN(date.getTime())) {
    return raw.length >= 16 ? raw.slice(0, 16) : raw;
  }
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}
