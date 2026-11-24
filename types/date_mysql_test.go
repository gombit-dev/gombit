//go:build integration

package types

import (
	"flag"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

var mysqlDSN = flag.String("types.mysql-dsn", "", "MySQL DSN for types integration tests")

// TestDateDriverLocationMySQL: go-sql-driver/mysql sends a time.Time in its
// loc and returns a DATE as midnight in it, so UTC midnight west of UTC was
// stored as the day before (#478). With parseTime=false the column comes
// back as text, and only the write depends on loc. Pacific/Apia skipped
// 2011-12-30: the days around it keep their day, and 2011-12-30 itself is
// stored right but reads as 2011-12-29 (see Scan).
func TestDateDriverLocationMySQL(t *testing.T) {
	if *mysqlDSN == "" {
		t.Skip("set -types.mysql-dsn to run the MySQL date round trips")
	}
	edges := dateLocationDays
	for _, c := range []struct {
		name, loc string
		parseTime bool
		days      []string
		// skipped is a day loc skipped whole, which reads as eve.
		skipped, eve string
	}{
		{"no loc", "", true, edges, "", ""},
		{"loc west of UTC", "America/New_York", true, edges, "", ""},
		{"loc east of UTC", "Asia/Tokyo", true, edges, "", ""},
		{"loc west of UTC without parseTime", "America/New_York", false, edges, "", ""},
		{"loc skips midnight west of UTC", "America/Sao_Paulo", true, []string{"2018-11-04"}, "", ""},
		{"loc skips midnight east of UTC", "Asia/Beirut", true, []string{"2024-03-31"}, "", ""},
		{"loc skips a whole day", "Pacific/Apia", true, []string{"2011-12-28", "2011-12-29", "2011-12-31"}, "2011-12-30", "2011-12-29"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := gomysql.ParseDSN(*mysqlDSN)
			if err != nil {
				t.Fatal(err)
			}
			cfg.ParseTime = c.parseTime
			if c.loc != "" {
				if cfg.Loc, err = time.LoadLocation(c.loc); err != nil {
					t.Fatal(err)
				}
			}
			db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if sqlDB, err := db.DB(); err == nil {
				t.Cleanup(func() { _ = sqlDB.Close() })
			}
			checkDateLocation(t, db, c.days, "CAST(%s AS CHAR)", func(day string) string { return day })
			if c.skipped != "" {
				checkDateSkippedDay(t, db, c.skipped, c.eve)
			}
		})
	}
}

// checkDateSkippedDay: a day db's loc skipped whole is stored right, but
// go-sql-driver/mysql returns it as the same time as eve, so it reads as
// eve, and saving the row read stores eve (the limitation Scan documents).
func checkDateSkippedDay(t *testing.T, db *gorm.DB, skipped, eve string) {
	t.Helper()
	if err := db.AutoMigrate(&dateLocationRow{}); err != nil {
		t.Fatal(err)
	}
	day, err := ParseDate(skipped)
	if err != nil {
		t.Fatal(err)
	}
	stored := func(id uint) string {
		t.Helper()
		var s string
		if err := db.Raw("SELECT CAST(day AS CHAR) FROM date_location_rows WHERE id = ?", id).Row().Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	row := dateLocationRow{Day: day}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if got := stored(row.ID); got != skipped {
		t.Errorf("%s: stored %q; want %q", skipped, got, skipped)
	}
	var got dateLocationRow
	if err := db.First(&got, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Day.String() != eve {
		t.Errorf("limitation: %s reads as %s; want %s", skipped, got.Day, eve)
	}
	if err := db.Save(&got).Error; err != nil {
		t.Fatal(err)
	}
	if s := stored(row.ID); s != eve {
		t.Errorf("limitation: %s read and saved is stored as %q; want %q", skipped, s, eve)
	}
}
