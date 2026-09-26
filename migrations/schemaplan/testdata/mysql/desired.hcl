table "products" {
  schema = schema.dev
  column "id" {
    null           = false
    type           = bigint
    auto_increment = true
  }
  column "title" {
    null = true
    type = varchar(255)
  }
  column "price" {
    null = true
    type = int
  }
  column "note" {
    null = false
    type = varchar(100)
  }
  column "sku" {
    null = true
    type = varchar(255)
  }
  column "stock" {
    null    = false
    type    = bigint
    default = 0
  }
  column "qty" {
    null = false
    type = bigint
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
    on_delete   = CASCADE
  }
  index "fk_owner" {
    columns = [column.owner_id]
  }
  index "idx_sku" {
    unique  = true
    columns = [column.sku]
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
