export type FieldType =
  | "string"
  | "text"
  | "integer"
  | "float"
  | "decimal"
  | "boolean"
  | "datetime"
  | "date"
  | "uuid"
  | "json"
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
