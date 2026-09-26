table "products" {
  schema = schema.public
  column "id" {
    null = false
    type = bigserial
  }
  column "title" {
    null = true
    type = character_varying(255)
  }
  column "price" {
    null = true
    type = integer
  }
  column "note" {
    null = false
    type = character_varying(100)
  }
  column "sku" {
    null = true
    type = text
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
  index "idx_sku" {
    unique  = true
    columns = [column.sku]
  }
}
table "users" {
  schema = schema.public
  column "id" {
    null = false
    type = bigserial
  }
  column "email" {
    null = false
    type = text
  }
  primary_key {
    columns = [column.id]
  }
}
schema "public" {
  comment = "standard public schema"
}
