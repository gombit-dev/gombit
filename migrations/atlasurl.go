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
	path, query, hasQuery := strings.Cut(dsn, "?")
	suffix := ""
	if hasQuery {
		suffix = "?" + query
	}
	switch {
	case strings.HasPrefix(path, "file:///"):
		// file:///tmp/app.db → sqlite:///tmp/app.db (three slashes, #135).
		return "sqlite://" + strings.TrimPrefix(path, "file://") + suffix, nil
	case strings.HasPrefix(path, "file:"):
		return "sqlite://" + strings.TrimPrefix(path, "file:") + suffix, nil
	default:
		return "sqlite://" + path + suffix, nil
	}
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
