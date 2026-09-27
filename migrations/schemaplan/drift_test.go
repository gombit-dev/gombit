package schemaplan

import (
	"reflect"
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

// TestDatabaseDriftPostgresIgnoresTheRevisionsSchema: Atlas keeps its
// revisions table in a schema of its own on PostgreSQL.
func TestDatabaseDriftPostgresIgnoresTheRevisionsSchema(t *testing.T) {
	const widgets = `
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
	live := widgets + `
table "atlas_schema_revisions" {
  schema = schema.atlas_schema_revisions
  column "version" {
    null = false
    type = character_varying
  }
}
schema "atlas_schema_revisions" {}
`
	steps, err := DatabaseDrift(config.DatabaseDriverPostgres, []byte(widgets), []byte(live), bookkeeping)
	if err != nil {
		t.Fatalf("DatabaseDrift() error = %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("steps = %v, want none", stepIDs(steps))
	}

	// Only the revisions schema is set aside, so an application schema of
	// another name is still compared with the migrations' one.
	renamed := strings.ReplaceAll(live, "schema.public", "schema.app")
	renamed = strings.Replace(renamed, `schema "public"`, `schema "app"`, 1)
	steps, err = DatabaseDrift(config.DatabaseDriverPostgres, []byte(widgets), []byte(renamed), bookkeeping)
	if err != nil {
		t.Fatalf("DatabaseDrift() error = %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("steps with the application schema named app = %v, want none", stepIDs(steps))
	}

	// A schema the application created by hand is drift.
	steps, err = DatabaseDrift(config.DatabaseDriverPostgres, []byte(widgets), []byte(widgets+`
table "notes" {
  schema = schema.reports
  column "id" {
    null = false
    type = bigint
  }
}
schema "reports" {}
`), bookkeeping)
	if err != nil {
		t.Fatalf("DatabaseDrift() error = %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("a hand-made schema is not reported")
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
