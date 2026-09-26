table "items" {
  schema = schema.main
  column "id" {
    null = true
    type = integer
  }
  column "name" {
    null = false
    type = text
  }
  column "stamped" {
    null    = false
    type    = text
    default = "never"
  }
  primary_key {
    columns = [column.id]
  }
}
schema "main" {
}
