table "items" {
  schema = schema.public
  column "id" {
    null = false
    type = bigserial
  }
  column "created_at" {
    null = true
    type = timestamptz
  }
  column "name" {
    null = false
    type = character_varying(120)
  }
  column "sku" {
    null = false
    type = character_varying(64)
  }
  primary_key {
    columns = [column.id]
  }
  index "idx_items_created_at" {
    columns = [column.created_at]
  }
  index "idx_items_sku" {
    unique  = true
    columns = [column.sku]
  }
}
schema "public" {
  comment = "standard public schema"
}
