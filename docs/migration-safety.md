# Migration safety manifest

A **migration safety manifest** classifies whether a migration destroys data,
and a **verifier** proves that classification matches the migration's actual
SQL. A deployment host (a container platform, CI, or
[Gombit Cloud](adr/015-host-deployment-contracts.md)) uses it to **gate
destructive migrations** without blindly trusting a declared safety field. It is
the HOST-3 contract from [ADR-015](adr/015-host-deployment-contracts.md).

```bash
gombit db verify            # classify each migration; verify existing manifests
gombit db verify --write    # write a manifest beside each migration
gombit db verify --json     # print each migration's classification (for a host)
```

## What it classifies

`gombit db verify` reads each `database/migrations/*.sql` file, strips comments,
and classifies every statement into a closed set of operations, flagging those
that lose data:

| Operation | Safety |
| --- | --- |
| `create_table`, `add_column`, `create_index`, `drop_index`, `rename_table`, `rename_column` | `non_destructive` |
| `drop_column`, `drop_table`, `alter_column`, `DELETE` / `TRUNCATE` (as `other`) | **`data_loss`** |

A migration with any `data_loss` operation sets `requires_confirmation: true`.
`alter_column` is treated as data loss conservatively — a narrowing type change
can truncate values, and the classifier cannot prove otherwise. It is a
statement-level DDL classifier, not a full SQL parser, and it errs toward
`data_loss` rather than declaring a statement safe. It handles the Postgres
(`"`), MySQL (`` ` ``) and SQLite quoting Atlas emits; where SQLite rewrites a
drop-column as a table rebuild, the old-table `DROP` still flags data loss.

## The manifest

`--write` emits `<version>_<name>.manifest.json` beside each migration:

```json
{
  "manifest_version": 1,
  "migration": { "version": "20260901120000", "name": "drop_legacy_code" },
  "sql_sha256": "…",
  "requires_confirmation": true,
  "operations": [
    { "kind": "drop_column", "resource": "customers", "column": "legacy_code", "safety": "data_loss" }
  ]
}
```

`sql_sha256` binds the manifest to the exact reviewed SQL. It is **anti-tamper**
binding, not migration drift detection (build plan D4 keeps
`framework_migrations` checksum-free); the manifest never touches that table.

## Verify, don't trust

Without `--write`, `gombit db verify` checks each migration that has a manifest
against its SQL and **fails** (non-zero exit) when:

- **the SQL changed after review** — `sql_sha256` no longer matches; or
- **the manifest lies** — it declares `non_destructive` while the SQL drops a
  column, or otherwise disagrees with what the SQL actually does.

So a host never trusts a manifest's own `safety` field — it re-derives the
classification from the executable SQL and compares (DESIGN.md §31).

## Gombit classifies; the host gates

Gombit **classifies and verifies**. It does **not** implement the approval
workflow: blocking a `data_loss` migration until a human confirms it is the
host's policy (DESIGN.md §32), and automation must never silently auto-confirm
data loss (§33). `gombit db verify` gives a host the signal — a non-zero exit
and `requires_confirmation` — to enforce that gate. See
[ADR-015](adr/015-host-deployment-contracts.md).
