table "users" {
  schema = schema.main
  column "id" {
    null           = true
    type           = integer
    auto_increment = true
  }
  column "email" {
    null = false
    type = text
  }
  primary_key {
    columns = [column.id]
  }
}
table "products" {
  schema = schema.main
  column "id" {
    null           = true
    type           = integer
    auto_increment = true
  }
  column "title" {
    null = true
    type = text
  }
  column "price" {
    null = true
    type = text
  }
  column "note" {
    null = false
    type = text
  }
  column "sku" {
    null = true
    type = text
  }
  column "stock" {
    null = false
    type = integer
  }
  column "owner_id" {
    null = true
    type = integer
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
schema "main" {
}
