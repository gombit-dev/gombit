package schemaplan

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/config"
)

var bookkeeping = []string{"framework_migrations", "atlas_schema_revisions"}

const driftSQLiteExpected = `
table "widgets" {
  schema = schema.main
  column "id" {
    null = false
    type = integer
  }
  column "name" {
    null = false
    type = text
  }
  primary_key {
    columns = [column.id]
  }
  index "idx_widgets_name" {
    columns = [column.name]
  }
}
schema "main" {}
`

func TestDatabaseDriftSQLite(t *testing.T) {
	cases := []struct {
		name string
		live string
		want []string
	}{
		{
			name: "same schema plus the bookkeeping tables",
			live: `
table "framework_migrations" {
  schema = schema.main
  column "version" {
    null = false
    type = text
  }
}
table "atlas_schema_revisions" {
  schema = schema.main
  column "version" {
    null = false
    type = text
  }
}
` + driftSQLiteExpected,
		},
		{
			name: "a column added and an index dropped by hand",
			live: `
table "widgets" {
  schema = schema.main
  column "id" {
    null = false
    type = integer
  }
  column "name" {
    null = false
    type = text
  }
  column "extra" {
    null = true
    type = text
  }
  primary_key {
    columns = [column.id]
  }
}
schema "main" {}
`,
			want: []string{"add_column:widgets.extra", "drop_index:widgets.idx_widgets_name"},
		},
		{
			name: "a table created by hand",
			live: driftSQLiteExpected + `
table "scratch" {
  schema = schema.main
  column "id" {
    null = false
    type = integer
  }
}
`,
			want: []string{"add_table:scratch"},
		},
		{
			name: "a table the migrations create is missing",
			live: `schema "main" {}`,
			want: []string{"drop_table:widgets"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			steps, err := DatabaseDrift(config.DatabaseDriverSQLite, []byte(driftSQLiteExpected), []byte(tc.live), bookkeeping)
			if err != nil {
				t.Fatalf("DatabaseDrift() error = %v", err)
			}
			if got := stepIDs(steps); (len(got) != 0 || len(tc.want) != 0) && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("steps = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDatabaseDriftMySQLComparesTheOneSchema: the dev database the migrations
// are replayed on is named "dev", the application database something else,
// with its own server charset. Neither is drift.
func TestDatabaseDriftMySQLComparesTheOneSchema(t *testing.T) {
	table := func(schema string, extra string) string {
		return `
table "widgets" {
  schema = schema.` + schema + `
  column "id" {
    null           = false
    type           = bigint
    auto_increment = true
  }
  column "name" {
    null = true
    type = varchar(255)
  }` + extra + `
  primary_key {
    columns = [column.id]
  }
}
`
	}
	expected := table("dev", "") + `schema "dev" {
  charset = "utf8mb4"
  collate = "utf8mb4_0900_ai_ci"
}
`
	live := table("shop", "") + `
table "framework_migrations" {
  schema = schema.shop
  column "version" {
    null = false
    type = varchar(64)
  }
}
schema "shop" {
  charset = "latin1"
  collate = "latin1_swedish_ci"
}
`
	steps, err := DatabaseDrift(config.DatabaseDriverMySQL, []byte(expected), []byte(live), bookkeeping)
	if err != nil {
		t.Fatalf("DatabaseDrift() error = %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("steps = %v, want none", stepIDs(steps))
	}

	drifted := table("shop", `
  column "note" {
    null = true
    type = text
  }`) + `schema "shop" {}
`
	steps, err = DatabaseDrift(config.DatabaseDriverMySQL, []byte(expected), []byte(drifted), bookkeeping)
	if err != nil {
		t.Fatalf("DatabaseDrift() error = %v", err)
	}
	if got, want := stepIDs(steps), []string{"add_column:widgets.note"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
}

const driftPostgresWidgets = `
table "widgets" {
  schema = schema.public
  column "id" {
    null = false
    type = bigserial
  }
  primary_key {
    columns = [column.id]
  }
}
schema "public" {
  comment = "standard public schema"
}
`

const driftPostgresBookkeeping = `
table "framework_migrations" {
  schema = schema.public
  column "version" {
    null = false
    type = character_varying
  }
}
table "atlas_schema_revisions" {
  schema = schema.public
  column "version" {
    null = false
    type = character_varying
  }
}
`

// TestDatabaseDriftPostgres: the bookkeeping tables and Atlas's own revisions
// schema are set aside, and nothing else is. A schema is an object the
// migrations create on PostgreSQL, so its name counts.
func TestDatabaseDriftPostgres(t *testing.T) {
	cases := []struct {
		name string
		live string
		want []string
	}{
		{
			name: "bookkeeping in public, as gombit db migrate keeps it",
			live: driftPostgresWidgets + driftPostgresBookkeeping,
		},
		{
			name: "Atlas's revisions in a schema of their own",
			live: driftPostgresWidgets + `
table "atlas_schema_revisions" {
  schema = schema.atlas_schema_revisions
  column "version" {
    null = false
    type = character_varying
  }
}
schema "atlas_schema_revisions" {}
`,
		},
		{
			// ALTER TABLE widgets SET SCHEMA app: public keeps only the
			// bookkeeping tables.
			name: "a table moved to another schema",
			live: driftPostgresBookkeeping + `
table "widgets" {
  schema = schema.app
  column "id" {
    null = false
    type = bigserial
  }
  primary_key {
    columns = [column.id]
  }
}
schema "app" {}
schema "public" {
  comment = "standard public schema"
}
`,
			want: []string{"extra_schema:app", "add_table:widgets", "drop_table:widgets"},
		},
		{
			name: "the application schema renamed",
			live: strings.ReplaceAll(strings.Replace(driftPostgresWidgets, `schema "public"`, `schema "app"`, 1), "schema.public", "schema.app"),
			want: []string{"extra_schema:app", "missing_schema:public", "add_table:widgets", "drop_table:widgets"},
		},
		{
			name: "an empty schema created by hand",
			live: driftPostgresWidgets + `schema "scratch" {}
`,
			want: []string{"extra_schema:scratch"},
		},
		{
			name: "a schema created by hand with a table in it",
			live: driftPostgresWidgets + `
table "notes" {
  schema = schema.reports
  column "id" {
    null = false
    type = bigint
  }
}
schema "reports" {}
`,
			want: []string{"add_table:notes", "extra_schema:reports"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			steps, err := DatabaseDrift(config.DatabaseDriverPostgres, []byte(driftPostgresWidgets), []byte(tc.live), bookkeeping)
			if err != nil {
				t.Fatalf("DatabaseDrift() error = %v", err)
			}
			got := stepIDs(steps)
			sort.Strings(got)
			want := append([]string{}, tc.want...)
			sort.Strings(want)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("steps = %v, want %v", got, want)
			}
		})
	}
}

// TestDatabaseDriftNothingAppliedYet: before the first migration the live
// database must hold nothing but bookkeeping.
func TestDatabaseDriftNothingAppliedYet(t *testing.T) {
	steps, err := DatabaseDrift(config.DatabaseDriverPostgres, nil, []byte(driftPostgresBookkeeping+`schema "public" {}
`), bookkeeping)
	if err != nil || len(steps) != 0 {
		t.Fatalf("steps = %v, err = %v, want none", stepIDs(steps), err)
	}
	steps, err = DatabaseDrift(config.DatabaseDriverPostgres, nil, []byte(driftPostgresWidgets), bookkeeping)
	if err != nil || strings.Join(stepIDs(steps), ",") != "add_table:widgets" {
		t.Fatalf("steps = %v, err = %v, want [add_table:widgets]", stepIDs(steps), err)
	}
}

func TestDatabaseDriftRejectsBadHCL(t *testing.T) {
	if _, err := DatabaseDrift(config.DatabaseDriverSQLite, []byte("table {"), []byte(driftSQLiteExpected), nil); err == nil {
		t.Fatal("DatabaseDrift() accepted malformed expected HCL")
	}
	if _, err := DatabaseDrift(config.DatabaseDriverSQLite, []byte(driftSQLiteExpected), []byte("table {"), nil); err == nil {
		t.Fatal("DatabaseDrift() accepted malformed live HCL")
	}
}
