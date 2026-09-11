package migrations

import (
	"net/url"
	"testing"

	"github.com/gombit-dev/gombit/config"
)

func TestAtlasURL(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.DatabaseConfig
		want    string
		wantErr bool
	}{
		{
			name: "sqlite file",
			cfg: config.DatabaseConfig{
				Driver: config.DatabaseDriverSQLite,
				DSN:    "file:gombit.db?cache=shared&_fk=1",
			},
			want: "sqlite://gombit.db?cache=shared&_fk=1",
		},
		{
			name: "sqlite file absolute uri",
			cfg: config.DatabaseConfig{
				Driver: config.DatabaseDriverSQLite,
				DSN:    "file:///tmp/gombit.db",
			},
			want: "sqlite:///tmp/gombit.db",
		},
		{
			name: "sqlite file absolute uri with query",
			cfg: config.DatabaseConfig{
				Driver: config.DatabaseDriverSQLite,
				DSN:    "file:///tmp/gombit.db?cache=shared&_fk=1",
			},
			want: "sqlite:///tmp/gombit.db?cache=shared&_fk=1",
		},
		{
			name: "sqlite file single-slash absolute",
			cfg: config.DatabaseConfig{
				Driver: config.DatabaseDriverSQLite,
				DSN:    "file:/tmp/gombit.db",
			},
			want: "sqlite:///tmp/gombit.db",
		},
		{
			// A ":memory:" target maps to Atlas's canonical in-memory dev URL
			// (sqlite://file?mode=memory&...), not "sqlite://:memory:?..." —
			// the latter's empty-host:":memory:" authority is an invalid port
			// that net/url (and thus Atlas) rejects on Go 1.26+.
			name: "sqlite file memory uri",
			cfg: config.DatabaseConfig{
				Driver: config.DatabaseDriverSQLite,
				DSN:    "file::memory:?cache=shared&_fk=1",
			},
			want: "sqlite://file?mode=memory&cache=shared&_fk=1",
		},
		{
			name: "sqlite bare memory dsn",
			cfg: config.DatabaseConfig{
				Driver: config.DatabaseDriverSQLite,
				DSN:    ":memory:?cache=shared&_fk=1",
			},
			want: "sqlite://file?mode=memory&cache=shared&_fk=1",
		},
		{
			name: "sqlite already atlas",
			cfg: config.DatabaseConfig{
				Driver: config.DatabaseDriverSQLite,
				DSN:    "sqlite://file?mode=memory&_fk=1",
			},
			want: "sqlite://file?mode=memory&_fk=1",
		},
		{
			name: "postgres url",
			cfg: config.DatabaseConfig{
				Driver: config.DatabaseDriverPostgres,
				DSN:    "postgres://gombit:gombit@127.0.0.1:5432/gombit?sslmode=disable", // #nosec G101 -- fake local test DSN.
			},
			want: "postgres://gombit:gombit@127.0.0.1:5432/gombit?sslmode=disable", // #nosec G101 -- fake local test DSN.
		},
		{
			name: "postgres key value",
			cfg: config.DatabaseConfig{
				Driver: config.DatabaseDriverPostgres,
				DSN:    "host=localhost user=gombit password=secret dbname=app sslmode=disable", // #nosec G101 -- fake local test DSN.
			},
			want: "postgres://gombit:secret@localhost:5432/app?sslmode=disable", // #nosec G101 -- fake local test DSN.
		},
		{
			name: "postgres unix socket",
			cfg: config.DatabaseConfig{
				Driver: config.DatabaseDriverPostgres,
				DSN:    "host=/var/run/postgresql user=gombit dbname=app",
			},
			want: "postgres://gombit@/app?host=%2Fvar%2Frun%2Fpostgresql&port=5432",
		},
		{
			name: "postgres ipv6",
			cfg: config.DatabaseConfig{
				Driver: config.DatabaseDriverPostgres,
				DSN:    "host=::1 user=gombit dbname=app sslmode=disable",
			},
			want: "postgres://gombit@[::1]:5432/app?sslmode=disable",
		},
		{
			name: "postgres ipv6 already bracketed",
			cfg: config.DatabaseConfig{
				Driver: config.DatabaseDriverPostgres,
				DSN:    "host=[::1] user=gombit dbname=app sslmode=disable",
			},
			want: "postgres://gombit@[::1]:5432/app?sslmode=disable",
		},
		{
			name: "postgres unix socket with sslmode",
			cfg: config.DatabaseConfig{
				Driver: config.DatabaseDriverPostgres,
				DSN:    "host=/var/run/postgresql user=gombit dbname=app sslmode=disable",
			},
			want: "postgres://gombit@/app?host=%2Fvar%2Frun%2Fpostgresql&port=5432&sslmode=disable",
		},
		{
			name: "mysql tcp",
			cfg: config.DatabaseConfig{
				Driver: config.DatabaseDriverMySQL,
				DSN:    "gombit:gombit@tcp(127.0.0.1:3306)/gombit?parseTime=true", // #nosec G101 -- fake local test DSN.
			},
			want: "mysql://gombit:gombit@127.0.0.1:3306/gombit?parseTime=true", // #nosec G101 -- fake local test DSN.
		},
		{
			name: "mysql already atlas",
			cfg: config.DatabaseConfig{
				Driver: config.DatabaseDriverMySQL,
				DSN:    "mysql://gombit:gombit@127.0.0.1:3306/gombit", // #nosec G101 -- fake local test DSN.
			},
			want: "mysql://gombit:gombit@127.0.0.1:3306/gombit", // #nosec G101 -- fake local test DSN.
		},
		{
			name: "mysql unsupported",
			cfg: config.DatabaseConfig{
				Driver: config.DatabaseDriverMySQL,
				DSN:    "unix(/tmp/mysql.sock)/gombit",
			},
			wantErr: true,
		},
		{
			name: "empty dsn",
			cfg: config.DatabaseConfig{
				Driver: config.DatabaseDriverSQLite,
				DSN:    "",
			},
			wantErr: true,
		},
		{
			name: "sqlite file uri invalid percent escape",
			cfg: config.DatabaseConfig{
				Driver: config.DatabaseDriverSQLite,
				DSN:    "file:///tmp/foo%2.db",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := AtlasURL(tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatal("AtlasURL() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("AtlasURL() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("AtlasURL() = %q, want %q", got, tt.want)
			}
			parsed, err := url.Parse(got)
			if err != nil {
				t.Fatalf("AtlasURL() produced unparseable URL %q: %v", got, err)
			}
			switch tt.name {
			case "postgres unix socket", "postgres unix socket with sslmode":
				if parsed.Host != "" {
					t.Fatalf("unix socket URL host = %q, want empty (libpq ?host= form)", parsed.Host)
				}
				if gotHost := parsed.Query().Get("host"); gotHost != "/var/run/postgresql" {
					t.Fatalf("unix socket query host = %q, want %q", gotHost, "/var/run/postgresql")
				}
			case "postgres ipv6", "postgres ipv6 already bracketed":
				if parsed.Hostname() != "::1" || parsed.Port() != "5432" {
					t.Fatalf("ipv6 URL hostname:port = %q:%q, want [::1]:5432", parsed.Hostname(), parsed.Port())
				}
			case "sqlite file absolute uri", "sqlite file single-slash absolute":
				if parsed.Path != "/tmp/gombit.db" {
					t.Fatalf("sqlite absolute path = %q, want %q", parsed.Path, "/tmp/gombit.db")
				}
			}
		})
	}
}

func TestWithAtlasRevisionsSchema(t *testing.T) {
	base := []string{"migrate", "apply", "--url", "postgres://localhost/db", "--dir", "file://migrations"}

	gotPG := withAtlasRevisionsSchema(config.DatabaseDriverPostgres, append([]string{}, base...))
	wantPG := append(append([]string{}, base...), "--revisions-schema", "public")
	if len(gotPG) != len(wantPG) {
		t.Fatalf("postgres args = %v, want %v", gotPG, wantPG)
	}
	for i := range wantPG {
		if gotPG[i] != wantPG[i] {
			t.Fatalf("postgres args[%d] = %q, want %q", i, gotPG[i], wantPG[i])
		}
	}

	for _, driver := range []config.DatabaseDriver{config.DatabaseDriverSQLite, config.DatabaseDriverMySQL} {
		got := withAtlasRevisionsSchema(driver, append([]string{}, base...))
		if len(got) != len(base) {
			t.Fatalf("%s args = %v, want unchanged %v", driver, got, base)
		}
	}
}
