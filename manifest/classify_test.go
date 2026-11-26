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
		"drop not null is alter column, not drop column, and not data loss": {
			`ALTER TABLE "users" ALTER COLUMN "email" DROP NOT NULL;`,
			Operation{Kind: OpAlterColumn, Resource: "users", Column: "email", Safety: SafetyNonDestructive},
		},
		"drop default is alter column, not drop column, and not data loss": {
			`ALTER TABLE "users" ALTER COLUMN "email" DROP DEFAULT;`,
			Operation{Kind: OpAlterColumn, Resource: "users", Column: "email", Safety: SafetyNonDestructive},
		},
		"mysql drop default is alter column, not drop column, and not data loss": {
			"ALTER TABLE `users` ALTER COLUMN `email` DROP DEFAULT;",
			Operation{Kind: OpAlterColumn, Resource: "users", Column: "email", Safety: SafetyNonDestructive},
		},
		"quoted column literally named default is still a real drop": {
			"ALTER TABLE `customers` DROP `default`;",
			Operation{Kind: OpDropColumn, Resource: "customers", Column: "default", Safety: SafetyDataLoss},
		},
		"quoted column literally named key is still a real drop": {
			"ALTER TABLE `customers` DROP `key`;",
			Operation{Kind: OpDropColumn, Resource: "customers", Column: "key", Safety: SafetyDataLoss},
		},
		"quoted column literally named index is still a real drop": {
			`ALTER TABLE "customers" DROP "index";`,
			Operation{Kind: OpDropColumn, Resource: "customers", Column: "index", Safety: SafetyDataLoss},
		},
		"quoted column literally named check is still a real drop": {
			`ALTER TABLE "customers" DROP "check";`,
			Operation{Kind: OpDropColumn, Resource: "customers", Column: "check", Safety: SafetyDataLoss},
		},
		"column-less alter drop not null is alter column, not data loss": {
			`ALTER TABLE "users" ALTER "email" DROP NOT NULL;`,
			Operation{Kind: OpAlterColumn, Resource: "users", Column: "email", Safety: SafetyNonDestructive},
		},
		"multi-action drop default comma glued onto token is still a relaxation": {
			`ALTER TABLE "users" ALTER COLUMN "a" DROP DEFAULT, ALTER COLUMN "b" DROP NOT NULL;`,
			Operation{Kind: OpAlterColumn, Resource: "users", Column: "a", Safety: SafetyNonDestructive},
		},
		"mysql multi-action drop default comma glued onto token is still a relaxation": {
			"ALTER TABLE `users` ALTER COLUMN `a` DROP DEFAULT, ALTER COLUMN `b` DROP DEFAULT;",
			Operation{Kind: OpAlterColumn, Resource: "users", Column: "a", Safety: SafetyNonDestructive},
		},
		"relaxation combined with a type change is still data loss": {
			`ALTER TABLE "users" ALTER COLUMN "a" DROP NOT NULL, ALTER "b" TYPE smallint;`,
			Operation{Kind: OpAlterColumn, Resource: "users", Column: "a", Safety: SafetyDataLoss},
		},
		"relaxation combined with set not null on another column is still data loss": {
			`ALTER TABLE "users" ALTER COLUMN "a" DROP NOT NULL, ALTER "b" SET NOT NULL;`,
			Operation{Kind: OpAlterColumn, Resource: "users", Column: "a", Safety: SafetyDataLoss},
		},
		"relaxation combined with drop column is still data loss": {
			`ALTER TABLE "users" ALTER COLUMN "a" DROP NOT NULL, DROP COLUMN "b";`,
			Operation{Kind: OpDropColumn, Resource: "users", Column: "b", Safety: SafetyDataLoss},
		},
		"relaxation combined with a bare drop is still data loss": {
			`ALTER TABLE "users" ALTER COLUMN "a" DROP NOT NULL, DROP "b";`,
			Operation{Kind: OpDropColumn, Resource: "users", Column: "b", Safety: SafetyDataLoss},
		},
		"mysql convert character set combined with relaxation is still data loss": {
			"ALTER TABLE `users` CONVERT TO CHARACTER SET latin1, ALTER COLUMN `a` DROP DEFAULT;",
			Operation{Kind: OpAlterColumn, Resource: "users", Column: "a", Safety: SafetyDataLoss},
		},
		"two relaxations in one statement with a space before the comma are both non-destructive": {
			`ALTER TABLE "users" ALTER COLUMN "c" DROP DEFAULT , ALTER COLUMN "d" DROP NOT NULL;`,
			Operation{Kind: OpAlterColumn, Resource: "users", Column: "c", Safety: SafetyNonDestructive},
		},
		"a comma inside a type modifier's parentheses does not split the action": {
			`ALTER TABLE "users" ALTER COLUMN "a" TYPE numeric(10,2), ALTER COLUMN "b" DROP NOT NULL;`,
			Operation{Kind: OpAlterColumn, Resource: "users", Column: "a", Safety: SafetyDataLoss},
		},
		"an unterminated quote in the body (an unescaped apostrophe in a block comment) fails closed to data loss": {
			`ALTER TABLE "users" ADD COLUMN "nick" text /* don't backfill */, DROP CONSTRAINT "c", DROP "legacy";`,
			Operation{Kind: OpOther, Resource: "users", Safety: SafetyDataLoss},
		},
		"mysql a backslash-escaped quote in a default confuses the tracker and fails closed to data loss": {
			"ALTER TABLE `t` ADD COLUMN `c` varchar(10) NOT NULL DEFAULT 'it\\'s', DROP INDEX `i`, DROP `legacy`;",
			Operation{Kind: OpOther, Resource: "t", Safety: SafetyDataLoss},
		},
		"postgres an escape-string-escaped quote in a default confuses the tracker and fails closed to data loss": {
			`ALTER TABLE "t" ADD COLUMN "c" text DEFAULT E'it\'s', DROP CONSTRAINT "x", DROP "legacy";`,
			Operation{Kind: OpOther, Resource: "t", Safety: SafetyDataLoss},
		},
		"an unmatched closing parenthesis in a block comment fails closed to data loss": {
			`ALTER TABLE "t" ADD COLUMN "c" text /* :) */, DROP CONSTRAINT "x", DROP "legacy";`,
			Operation{Kind: OpOther, Resource: "t", Safety: SafetyDataLoss},
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

// A comma inside parentheses or a quoted token must not split an ALTER
// TABLE action. This goes through the public Classify() API, rather than the
// internal splitter directly, so it keeps compiling against any internal
// implementation of the classifier.
func TestClassifyCommaInsideParensOrQuotesDoesNotSplitTheAction(t *testing.T) {
	tests := map[string]struct {
		sql  string
		want Operation
	}{
		"comma inside a type's parentheses, combined with a relaxation, is still data loss": {
			`ALTER TABLE "users" ALTER COLUMN "b" TYPE numeric(10,2), ALTER COLUMN "a" DROP NOT NULL;`,
			Operation{Kind: OpAlterColumn, Resource: "users", Column: "b", Safety: SafetyDataLoss},
		},
		"comma inside a quoted identifier is not an action separator": {
			`ALTER TABLE "users" ALTER COLUMN "a,b" DROP NOT NULL;`,
			Operation{Kind: OpAlterColumn, Resource: "users", Column: "a,b", Safety: SafetyNonDestructive},
		},
		"comma inside a quoted default literal is not an action separator": {
			`ALTER TABLE "users" ALTER COLUMN "a" SET DEFAULT 'x, y';`,
			Operation{Kind: OpAlterColumn, Resource: "users", Column: "a", Safety: SafetyDataLoss},
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
