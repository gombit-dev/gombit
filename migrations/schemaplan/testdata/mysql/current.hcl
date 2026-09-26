table "products" {
  schema = schema.dev
  column "id" {
    null           = false
    type           = bigint
    auto_increment = true
  }
  column "name" {
    null = true
    type = varchar(255)
  }
  column "price" {
    null = true
    type = bigint
  }
  column "note" {
    null = true
    type = varchar(255)
  }
  column "sku" {
    null = true
    type = varchar(255)
  }
  column "owner_id" {
    null = true
    type = bigint
  }
  primary_key {
    columns = [column.id]
  }
  foreign_key "fk_owner" {
    columns     = [column.owner_id]
    ref_columns = [table.users.column.id]
    on_update   = NO_ACTION
    on_delete   = RESTRICT
  }
  index "fk_owner" {
    columns = [column.owner_id]
  }
}
table "users" {
  schema = schema.dev
  column "id" {
    null           = false
    type           = bigint
    auto_increment = true
  }
  column "email" {
    null = false
    type = varchar(255)
  }
  primary_key {
    columns = [column.id]
  }
}
schema "dev" {
  charset = "utf8mb4"
  collate = "utf8mb4_0900_ai_ci"
}
