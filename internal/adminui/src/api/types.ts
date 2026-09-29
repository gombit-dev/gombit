export type FieldType =
  | "string"
  | "text"
  | "integer"
  | "float"
  | "decimal"
  | "boolean"
  | "datetime"
  | "date"
  | "time"
  | "duration"
  | "uuid"
  | "json"
  | "file"
  | "image"
  | "relation";

export type RelationKind = "belongs_to" | "one_to_one" | "has_many" | "many_to_many";

export type RelationMeta = {
  slug: string;
  kind: RelationKind;
  label_field: string;
};

export type FieldMeta = {
  name: string;
  type: FieldType;
  required: boolean;
  readonly: boolean;
  writeonly?: boolean;
  related?: RelationMeta;
  minimum?: string;
  maximum?: string;
  max_length?: number;
  pattern?: string;
  default?: string;
  format?: string;
  choices?: { value: string; label: string }[];
  // A file or image field's upload policy, as hints (the server enforces it).
  accept?: string[];
  max_bytes?: number;
};

// FileValue is a file field's value: what a row carries (key, filename,
// size, type, download URL), or what an upload just produced (no URL yet).
export type FileValue = {
  key: string;
  filename?: string;
  size?: number;
  content_type?: string;
  url?: string;
  missing?: boolean;
};

// UploadGrant is the admin's answer to an upload request: the key to send
// as the field's value, and the request that uploads the bytes.
export type UploadGrant = {
  key: string;
  upload: { method: string; url: string; headers?: Record<string, string>; expires?: string };
};

export type Actions = {
  list: boolean;
  detail: boolean;
  create: boolean;
  update: boolean;
  delete: boolean;
};

export type Permissions = {
  view: string;
  create: string;
  update: string;
  delete: string;
};

export type Capabilities = {
  view: boolean;
  create: boolean;
  update: boolean;
  delete: boolean;
};

export type ModelMeta = {
  slug: string;
  singular: string;
  plural: string;
  pk: string;
  fields: FieldMeta[];
  list: string[];
  search: string[];
  filter: string[];
  ordering: string[];
  actions: Actions;
  permissions: Permissions;
  can: Capabilities;
};

export type Catalog = {
  models: ModelMeta[];
};

export type CatalogAux = {
  auth?: {
    mode: string;
    bootstrap: string;
  };
};

export type PageMeta = {
  page: number;
  per_page: number;
  total: number;
};

export type Row = Record<string, unknown>;
