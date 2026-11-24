//go:build integration

package admin_test

import (
	"strings"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/gombit-dev/gombit/config"
)

// TestAdminDateDriverLocationMySQL: go-sql-driver/mysql sends a time.Time
// in its loc, so west of UTC the admin stored every date a day early, and
// returns a DATE as midnight in it, which a DST change can skip (#478).
// 2011-12-29 is the eve of the day Pacific/Apia skipped whole.
func TestAdminDateDriverLocationMySQL(t *testing.T) {
	if *mysqlDSN == "" {
		t.Skip("set -admin.mysql-dsn to run MySQL admin integration tests")
	}
	for _, c := range []struct {
		name, zone string
		days       []string
	}{
		{"no loc", "", dateEdges},
		{"loc west of UTC", "America/New_York", dateEdges},
		{"loc east of UTC", "Asia/Tokyo", dateEdges},
		{"loc skips midnight west of UTC", "America/Sao_Paulo", []string{"2018-11-04"}},
		{"loc skips midnight east of UTC", "Asia/Beirut", []string{"2024-03-31"}},
		{"loc skips a whole day", "Pacific/Apia", []string{"2011-12-29"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := gomysql.ParseDSN(*mysqlDSN)
			if err != nil {
				t.Fatal(err)
			}
			if c.zone != "" {
				if cfg.Loc, err = time.LoadLocation(c.zone); err != nil {
					t.Fatal(err)
				}
			}
			runDateLocationAdmin(t, openAdminDriver(t, config.DatabaseDriverMySQL, cfg.FormatDSN()), c.days)
		})
	}
}

// TestAdminDateDriverLocationPostgres: pgx returns a date as UTC midnight
// whatever the session's time zone; the admin keeps the day there too.
func TestAdminDateDriverLocationPostgres(t *testing.T) {
	if *postgresDSN == "" {
		t.Skip("set -admin.postgres-dsn to run Postgres admin integration tests")
	}
	for _, c := range []struct{ name, zone string }{
		{"no timezone", ""},
		{"timezone west of UTC", "America/New_York"},
		{"timezone east of UTC", "Asia/Tokyo"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The zone name stays unescaped: gorm.io/driver/postgres reads
			// it from the DSN's text.
			dsn := *postgresDSN
			switch {
			case c.zone == "":
			case !strings.Contains(dsn, "://"):
				dsn += " timezone=" + c.zone
			case strings.Contains(dsn, "?"):
				dsn += "&timezone=" + c.zone
			default:
				dsn += "?timezone=" + c.zone
			}
			db := openAdminDriver(t, config.DatabaseDriverPostgres, dsn)
			if c.zone != "" {
				var tz string
				if err := db.Raw("SHOW TimeZone").Scan(&tz).Error; err != nil || tz != c.zone {
					t.Fatalf("session TimeZone = %q (%v); want %q", tz, err, c.zone)
				}
			}
			runDateLocationAdmin(t, db, dateEdges)
		})
	}
}
