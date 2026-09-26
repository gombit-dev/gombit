# Migration Example

Run these commands from the repository root. Install Atlas Community Edition
first (`curl -sSf https://atlasgo.sh | sh -s -- --community`).

## Generate

```sh
gombit db makemigrations create_products \
  --driver sqlite \
  --model github.com/gombit-dev/gombit/examples/migrations/internal/product.Product
```

This demonstrates the feature-package model path used by the temporary Atlas
Program Mode loader. The generated migration files are written to
`database/migrations` unless `--dir` is provided.

## Plan a change

After the first migration, change the model (drop a field, add a required one)
and preview what the next migration would do before writing it:

```sh
gombit db plan --driver sqlite
```

A destructive or unsafe step (a dropped column, a NOT NULL column with no
default) makes `plan` exit non-zero, and `makemigrations` refuses to write it
until you pass `--allow <id>` for each step. See
[docs/migrations.md](../../docs/migrations.md#planning-a-change).

`gombit db lint` then checks the directory in CI: integrity, layout, and that
every destructive change carries a `-- gombit:allow` line (makemigrations writes
them for you). After a hand edit, `gombit db repair` rehashes and re-checks it.

If you rename the example's `Product` model to `Item` (in a new
`internal/item` package), `--rename-table` keeps the table's rows:

```sh
gombit db makemigrations rename_products --rename-table products:items \
  --forget-model github.com/gombit-dev/gombit/examples/migrations/internal/product.Product \
  --model github.com/gombit-dev/gombit/examples/migrations/internal/item.Item
```

See [Renaming a table](../../docs/migrations.md#renaming-a-table) for the full
workflow.

## Apply, status, and rollback

After generating an up migration, add a companion down file with the same
version/name prefix, for example:

```text
database/migrations/20260101000000_create_products.sql
database/migrations/downs/20260101000000_create_products.down.sql
```

Configure the database (defaults are SQLite) and run:

```sh
export GOMBIT_DATABASE_DRIVER=sqlite
export GOMBIT_DATABASE_DSN='file:gombit.db?cache=shared&_fk=1'

gombit db status
gombit db migrate
gombit db status
gombit db rollback
```

`migrate` wraps `atlas migrate apply` and records the applied versions in
`framework_migrations` as one batch. `rollback` executes the latest batch's
`downs/*.down.sql` files and clears both Gombit and Atlas revision rows for those
versions.

## Seed and reset

Add SQL seed files under `database/seeds/` (lexical order). This example includes
[`database/seeds/01_demo.sql`](database/seeds/01_demo.sql):

```sh
# from the repository root, after configuring GOMBIT_DATABASE_* as above.
# --dir points at the app migration directory (default/repo-root
# database/migrations from makemigrations); --seeds can point at this example.
gombit db seed --seeds examples/migrations/database/seeds
gombit db reset --dir database/migrations --seeds examples/migrations/database/seeds
```

Or create seeds next to your app migrations:

```sh
mkdir -p database/seeds
cat > database/seeds/01_demo.sql <<'SQL'
-- Example seed. Adjust to match your migrated schema.
-- INSERT INTO products (name) VALUES ('demo');
SELECT 1;
SQL

gombit db seed
gombit db reset
```

`seed` runs every `*.sql` file in `--seeds` (default `database/seeds`).
`reset` drops the schema, migrates, then seeds. In production, pass `--force`
to allow reset.
