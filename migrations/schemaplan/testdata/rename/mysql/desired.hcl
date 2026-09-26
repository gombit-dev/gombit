table "items" {
  schema = schema.dev
  column "id" {
    null           = false
    type           = bigint
    unsigned       = true
    auto_increment = true
  }
  column "created_at" {
    null = true
    type = datetime(3)
  }
  column "name" {
    null = false
    type = varchar(120)
  }
  column "sku" {
    null = false
    type = varchar(64)
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
schema "dev" {
  charset = "utf8mb4"
  collate = "utf8mb4_0900_ai_ci"
}
