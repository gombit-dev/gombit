package manifest

import "testing"

func TestClassifySingleStatements(t *testing.T) {
	tests := map[string]struct {
		sql  string
		want Operation
	}{
		"postgres create table": {
			`CREATE TABLE "users" ("id" bigserial NOT NULL, PRIMARY KEY ("id"));`,
			Operation{Kind: OpCreateTable, Resource: "users", Safety: SafetyNonDestructive},
		},
		"mysql create table backticks": {
			"CREATE TABLE `users` (`id` bigint NOT NULL);",
			Operation{Kind: OpCreateTable, Resource: "users", Safety: SafetyNonDestructive},
		},
		"create unique index": {
			`CREATE UNIQUE INDEX "idx_users_email" ON "users" ("email");`,
			Operation{Kind: OpCreateIndex, Resource: "idx_users_email", Safety: SafetyNonDestructive},
		},
		"drop table is data loss": {
			`DROP TABLE "customers";`,
			Operation{Kind: OpDropTable, Resource: "customers", Safety: SafetyDataLoss},
		},
		"drop index is not data loss": {
			`DROP INDEX "idx_users_email";`,
			Operation{Kind: OpDropIndex, Resource: "idx_users_email", Safety: SafetyNonDestructive},
		},
		"postgres drop column is data loss": {
			`ALTER TABLE "customers" DROP COLUMN "legacy_code";`,
			Operation{Kind: OpDropColumn, Resource: "customers", Column: "legacy_code", Safety: SafetyDataLoss},
		},
		"mysql drop column backticks": {
			"ALTER TABLE `customers` DROP COLUMN `legacy_code`;",
			Operation{Kind: OpDropColumn, Resource: "customers", Column: "legacy_code", Safety: SafetyDataLoss},
		},
		"mysql bare drop column": {
			"ALTER TABLE `customers` DROP `legacy_code`;",
			Operation{Kind: OpDropColumn, Resource: "customers", Column: "legacy_code", Safety: SafetyDataLoss},
		},
		"add column is safe": {
			`ALTER TABLE "customers" ADD COLUMN "note" text NOT NULL DEFAULT '';`,
			Operation{Kind: OpAddColumn, Resource: "customers", Column: "note", Safety: SafetyNonDestructive},
		},
		"alter column is data loss (conservative)": {
			`ALTER TABLE "customers" ALTER COLUMN "code" TYPE integer;`,
			Operation{Kind: OpAlterColumn, Resource: "customers", Column: "code", Safety: SafetyDataLoss},
		},
		"mysql modify column is data loss": {
			"ALTER TABLE `customers` MODIFY COLUMN `code` int;",
			Operation{Kind: OpAlterColumn, Resource: "customers", Column: "code", Safety: SafetyDataLoss},
		},
		"rename column is safe": {
			`ALTER TABLE "customers" RENAME COLUMN "code" TO "ref";`,
			Operation{Kind: OpRenameColumn, Resource: "customers", Column: "code", Safety: SafetyNonDestructive},
		},
		"drop constraint is not row data loss": {
			`ALTER TABLE "customers" DROP CONSTRAINT "fk_customers_owner";`,
			Operation{Kind: OpOther, Resource: "customers", Safety: SafetyNonDestructive},
		},
		"delete from is data loss": {
			`DELETE FROM "customers" WHERE "legacy" = true;`,
			Operation{Kind: OpOther, Safety: SafetyDataLoss},
		},
		"mysql multi-table delete is data loss": {
			"DELETE c FROM `customers` c JOIN `orders` o ON c.id = o.cid;",
			Operation{Kind: OpOther, Safety: SafetyDataLoss},
		},
		"update is data loss (conservative)": {
			`UPDATE "customers" SET "status" = 'x';`,
			Operation{Kind: OpOther, Safety: SafetyDataLoss},
		},
		"truncate is data loss": {
			`TRUNCATE "customers";`,
			Operation{Kind: OpOther, Safety: SafetyDataLoss},
		},
		"drop schema cascade is data loss": {
			`DROP SCHEMA "legacy" CASCADE;`,
			Operation{Kind: OpOther, Safety: SafetyDataLoss},
		},
		"drop database is data loss": {
			`DROP DATABASE "app";`,
			Operation{Kind: OpOther, Safety: SafetyDataLoss},
		},
		"insert is safe other": {
			`INSERT INTO "customers" ("name") VALUES ('a');`,
			Operation{Kind: OpOther, Safety: SafetyNonDestructive},
		},
		"create view is safe other": {
			`CREATE VIEW "active" AS SELECT * FROM "customers";`,
			Operation{Kind: OpOther, Safety: SafetyNonDestructive},
		},
		"unrecognized statement is data loss (fail-safe)": {
			`SOMETHING WEIRD HERE;`,
			Operation{Kind: OpOther, Safety: SafetyDataLoss},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ops := Classify(tc.sql)
			if len(ops) != 1 {
				t.Fatalf("Classify() = %d ops, want 1: %+v", len(ops), ops)
			}
			if ops[0] != tc.want {
				t.Fatalf("Classify() = %+v, want %+v", ops[0], tc.want)
			}
		})
	}
}

func TestClassifyStripsCommentsAndOrders(t *testing.T) {
	sql := `-- Create "users" table
CREATE TABLE "users" ("id" bigserial NOT NULL, PRIMARY KEY ("id"));
-- Create index "idx_users_email" to table: "users"
CREATE UNIQUE INDEX "idx_users_email" ON "users" ("email");`

	ops := Classify(sql)
	if len(ops) != 2 {
		t.Fatalf("Classify() = %d ops, want 2: %+v", len(ops), ops)
	}
	if ops[0].Kind != OpCreateTable || ops[1].Kind != OpCreateIndex {
		t.Fatalf("Classify() kinds = %s, %s; want create_table, create_index", ops[0].Kind, ops[1].Kind)
	}
}

// SQLite often can't drop a column in place; Atlas rewrites it as a table
// rebuild (new table, copy, drop old, rename). The old-table DROP still
// classifies as data loss, so the classifier stays safe-by-default even when
// the DDL differs by driver.
func TestClassifySQLiteRebuildFlagsDataLoss(t *testing.T) {
	sql := `CREATE TABLE ` + "`new_customers`" + ` (` + "`id`" + ` integer NOT NULL);
INSERT INTO ` + "`new_customers`" + ` (` + "`id`" + `) SELECT ` + "`id`" + ` FROM ` + "`customers`" + `;
DROP TABLE ` + "`customers`" + `;
ALTER TABLE ` + "`new_customers`" + ` RENAME TO ` + "`customers`" + `;`

	if !requiresConfirmation(Classify(sql)) {
		t.Fatalf("SQLite rebuild not flagged as requiring confirmation: %+v", Classify(sql))
	}
}

func TestClassifyMultiActionAlterInOneStatementFlagsDrop(t *testing.T) {
	// A single ALTER that adds one column and drops another must surface the
	// drop, even though the ADD comes first.
	ops := Classify(`ALTER TABLE "customers" ADD COLUMN "note" text, DROP COLUMN "legacy_code";`)
	if len(ops) != 1 {
		t.Fatalf("Classify() = %d ops, want 1: %+v", len(ops), ops)
	}
	if ops[0].Kind != OpDropColumn || ops[0].Safety != SafetyDataLoss {
		t.Fatalf("Classify() = %+v, want drop_column/data_loss", ops[0])
	}
}

func TestClassifyMysqlMultiActionAlterBareDrop(t *testing.T) {
	ops := Classify("ALTER TABLE `customers` ADD `note` text, DROP `legacy_code`;")
	if len(ops) != 1 || ops[0].Kind != OpDropColumn || ops[0].Safety != SafetyDataLoss {
		t.Fatalf("Classify() = %+v, want drop_column/data_loss", ops)
	}
}

func TestClassifyAlterDropConstraintIsNotDataLoss(t *testing.T) {
	ops := Classify(`ALTER TABLE "customers" DROP CONSTRAINT "fk_owner";`)
	if len(ops) != 1 || ops[0].Safety != SafetyNonDestructive {
		t.Fatalf("Classify() = %+v, want non_destructive", ops)
	}
}
