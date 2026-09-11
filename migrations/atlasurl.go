package migrations

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/gombit-dev/gombit/config"
)

var mysqlTCPPattern = regexp.MustCompile(`^(?:([^:@]+)?(?::([^@]*))?@)?tcp\(([^)]+)\)/([^?]*)(\?.*)?$`)

// AtlasURL converts a Gombit database config into an Atlas --url value.
func AtlasURL(cfg config.DatabaseConfig) (string, error) {
	if err := config.ValidateDatabase(cfg); err != nil {
		return "", err
	}
	dsn := strings.TrimSpace(cfg.DSN)
	switch cfg.Driver {
	case config.DatabaseDriverSQLite:
		return sqliteAtlasURL(dsn)
	case config.DatabaseDriverPostgres:
		return postgresAtlasURL(dsn)
	case config.DatabaseDriverMySQL:
		return mysqlAtlasURL(dsn)
	default:
		return "", fmt.Errorf("migrations: unsupported driver %q", cfg.Driver)
	}
}

func sqliteAtlasURL(dsn string) (string, error) {
	if strings.HasPrefix(dsn, "sqlite://") {
		return dsn, nil
	}
	if strings.HasPrefix(dsn, "file:") {
		return sqliteFileURIToAtlas(dsn)
	}
	// Bare mattn/go-sqlite3 in-memory DSN (":memory:" / ":memory:?params").
	if rest, ok := memoryDSNParams(dsn); ok {
		return sqliteMemoryAtlasURL(rest), nil
	}
	return "sqlite://" + dsn, nil
}

// sqliteFileURIToAtlas maps a SQLite file: URI onto Atlas's sqlite:// form.
// url.Parse is required so file:///abs (three slashes) becomes sqlite:///abs,
// not sqlite:////abs or sqlite://///abs from a naive "file:" strip (#135).
func sqliteFileURIToAtlas(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("migrations: sqlite file URI: %w", err)
	}
	path := u.Opaque
	if path == "" {
		path = u.Path
	}
	// A ":memory:" target cannot be carried through as the URL authority:
	// "sqlite://:memory:" has an empty host with a ":memory:" port, which
	// net/url rejects as an invalid port on Go 1.26+ — and Atlas, which parses
	// its --url with net/url too, rejects it identically. Emit Atlas's
	// canonical, well-formed in-memory dev URL (sqlite://file?mode=memory)
	// instead. See migrations/atlasurl_test.go and Atlas's --dev-url docs.
	if path == ":memory:" {
		return sqliteMemoryAtlasURL(u.RawQuery), nil
	}
	if u.RawQuery == "" {
		return "sqlite://" + path, nil
	}
	return "sqlite://" + path + "?" + u.RawQuery, nil
}

// memoryDSNParams reports whether dsn is a bare ":memory:" DSN and returns any
// query string after it (without the "?"). ":memory:?cache=shared" -> "cache=shared".
func memoryDSNParams(dsn string) (params string, ok bool) {
	const memory = ":memory:"
	if dsn == memory {
		return "", true
	}
	if rest, found := strings.CutPrefix(dsn, memory+"?"); found {
		return rest, true
	}
	return "", false
}

// sqliteMemoryAtlasURL builds Atlas's canonical in-memory SQLite dev URL,
// sqlite://file?mode=memory[&extra], from any extra query parameters carried by
// the source DSN. mode=memory is prepended so the resulting URL is well-formed
// (host "file", no empty-host:port authority) and understood by Atlas as an
// in-memory database.
func sqliteMemoryAtlasURL(extraQuery string) string {
	query := "mode=memory"
	if extraQuery != "" {
		query += "&" + extraQuery
	}
	return "sqlite://file?" + query
}

func postgresAtlasURL(dsn string) (string, error) {
	switch {
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		return dsn, nil
	case strings.Contains(dsn, "="):
		return postgresKeyValueURL(dsn)
	default:
		return "", fmt.Errorf("migrations: unsupported postgres DSN %q", dsn)
	}
}

func postgresKeyValueURL(dsn string) (string, error) {
	values := make(map[string]string)
	for _, part := range strings.Fields(dsn) {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return "", fmt.Errorf("migrations: invalid postgres DSN fragment %q", part)
		}
		values[key] = value
	}
	host := values["host"]
	if host == "" {
		host = "localhost"
	}
	port := values["port"]
	if port == "" {
		port = "5432"
	}
	user := values["user"]
	password := values["password"]
	dbname := values["dbname"]
	if dbname == "" {
		return "", fmt.Errorf("migrations: postgres DSN missing dbname")
	}

	query := url.Values{}
	for key, value := range values {
		switch key {
		case "host", "port", "user", "password", "dbname":
			continue
		default:
			query.Set(key, value)
		}
	}

	u := &url.URL{
		Scheme: "postgres",
		Path:   "/" + dbname,
	}
	if user != "" {
		if password != "" {
			u.User = url.UserPassword(user, password)
		} else {
			u.User = url.User(user)
		}
	}
	if isPostgresUnixSocket(host) {
		// libpq URI form: empty host, socket directory in the host query
		// parameter (postgres://user@/dbname?host=/var/run/postgresql).
		query.Set("host", host)
		query.Set("port", port)
	} else {
		u.Host = net.JoinHostPort(unbracketHost(host), port)
	}
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func isPostgresUnixSocket(host string) bool {
	return strings.HasPrefix(host, "/")
}

func unbracketHost(host string) string {
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		return host[1 : len(host)-1]
	}
	return host
}

func mysqlAtlasURL(dsn string) (string, error) {
	if strings.HasPrefix(dsn, "mysql://") {
		return dsn, nil
	}
	matches := mysqlTCPPattern.FindStringSubmatch(dsn)
	if matches == nil {
		return "", fmt.Errorf("migrations: unsupported mysql DSN %q", dsn)
	}
	user := matches[1]
	password := matches[2]
	host := matches[3]
	dbname := matches[4]
	rawQuery := strings.TrimPrefix(matches[5], "?")

	u := &url.URL{
		Scheme:   "mysql",
		Host:     host,
		Path:     "/" + dbname,
		RawQuery: rawQuery,
	}
	if user != "" {
		if password != "" {
			u.User = url.UserPassword(user, password)
		} else {
			u.User = url.User(user)
		}
	}
	return u.String(), nil
}

// withAtlasRevisionsSchema appends Atlas CLI flags so revision bookkeeping stays
// in the public schema on PostgreSQL.
//
// Atlas Community Edition defaults PostgreSQL revisions to a dedicated schema
// named atlas_schema_revisions (schema.table). Gombit's ledger sync and
// rollback read/write the table atlas_schema_revisions via GORM's default
// search_path (public). Pinning --revisions-schema public keeps all three
// drivers on the same table name in the default schema.
//
// Call sites that talk to an existing DB must use this helper:
// Migrate (atlas migrate apply) and Status (atlas migrate status).
// MakeMigrations (migrate diff) and migrate hash do not touch the app DB
// revisions table and must not add this flag.
func withAtlasRevisionsSchema(driver config.DatabaseDriver, args []string) []string {
	if driver != config.DatabaseDriverPostgres {
		return args
	}
	return append(args, "--revisions-schema", "public")
}
