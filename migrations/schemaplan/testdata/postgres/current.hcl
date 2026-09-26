table "products" {
  schema = schema.public
  column "id" {
    null = false
    type = bigserial
  }
  column "name" {
    null = true
    type = character_varying(255)
  }
  column "price" {
    null = true
    type = bigint
  }
  column "note" {
    null = true
    type = text
  }
  column "sku" {
    null = true
    type = text
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
