table "items" {
  schema = schema.main
  column "id" {
    null = true
    type = integer
  }
  column "name" {
    null    = false
    type    = text
    default = "keep"
  }
  column "price" {
    null    = false
    type    = integer
    default = 1
  }
  column "doubled" {
    null = true
    type = integer
    as {
      expr = "(price * 2)"
      type = VIRTUAL
    }
  }
  primary_key {
    columns = [column.id]
  }
}
schema "main" {
}
