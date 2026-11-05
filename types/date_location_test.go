package types

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"
	_ "time/tzdata" // the zones below, wherever the test runs

	"gorm.io/driver/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// dateLocationRow is the table the driver time zone round trips write (#478).
type dateLocationRow struct {
	ID  uint
	Day Date  `gorm:"type:date;not null"`
	Opt *Date `gorm:"type:date"`
}

// dateLocationDays are the calendar edges a shift by the driver's time zone
// moves into another year or month.
var dateLocationDays = []string{"2024-01-01", "2024-02-29", "2024-12-31"}

// checkDateLocation writes each day as a Date and a *Date through db, whose
// driver converts times to some location, and checks that the column holds
// that day (cast reads it as text, stored is the text expected for a day),
// that an equality, a struct, an IN and two range filters with the date as
// the parameter find the row, and that it reads back, saves and reads back
// again unchanged. A NULL *Date reads back as nil.
func checkDateLocation(t *testing.T, db *gorm.DB, days []string, cast string, stored func(day string) string) {
	t.Helper()
	if err := db.Migrator().DropTable(&dateLocationRow{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&dateLocationRow{}) })
	if err := db.AutoMigrate(&dateLocationRow{}); err != nil {
		t.Fatal(err)
	}
	storedText := func(id uint) (day, opt string) {
		t.Helper()
		q := fmt.Sprintf("SELECT %s, %s FROM date_location_rows WHERE id = ?", fmt.Sprintf(cast, "day"), fmt.Sprintf(cast, "opt"))
		var o sql.NullString
		if err := db.Raw(q, id).Row().Scan(&day, &o); err != nil {
			t.Fatal(err)
		}
		return day, o.String
	}
	ids := func(q *gorm.DB) []uint {
		t.Helper()
		var out []uint
		if err := q.Model(&dateLocationRow{}).Order("id").Pluck("id", &out).Error; err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, s := range days {
		want, err := ParseDate(s)
		if err != nil {
			t.Fatal(err)
		}
		row := dateLocationRow{Day: want, Opt: &want}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		if day, opt := storedText(row.ID); day != stored(s) || opt != stored(s) {
			t.Errorf("%s: stored day %q, opt %q; want %q", s, day, opt, stored(s))
		}
		next := NewDate(want.Time().AddDate(0, 0, 1))
		filters := []struct {
			name string
			q    *gorm.DB
		}{
			{"day = ?", db.Where("day = ?", want)},
			{"opt = ?", db.Where("opt = ?", &want)},
			{"struct", db.Where(&dateLocationRow{Day: want})},
			{"day IN ?", db.Where("day IN ?", []Date{want})},
			{"BETWEEN", db.Where("day BETWEEN ? AND ?", want, want)},
			{">= and <", db.Where("day >= ? AND day < ?", want, next)},
		}
		for _, f := range filters {
			if got := ids(f.q); !slices.Equal(got, []uint{row.ID}) {
				t.Errorf("%s: filter %s found rows %v; want [%d]", s, f.name, got, row.ID)
			}
		}
		var got dateLocationRow
		if err := db.First(&got, row.ID).Error; err != nil {
			t.Fatal(err)
		}
		if got.Day.String() != s || got.Opt == nil || got.Opt.String() != s {
			t.Errorf("%s: read day %s, opt %v", s, got.Day, got.Opt)
		}
		if err := db.Save(&got).Error; err != nil {
			t.Fatal(err)
		}
		if day, opt := storedText(row.ID); day != stored(s) || opt != stored(s) {
			t.Errorf("%s: after a save, stored day %q, opt %q; want %q", s, day, opt, stored(s))
		}
		var again dateLocationRow
		if err := db.First(&again, row.ID).Error; err != nil {
			t.Fatal(err)
		}
		if again.Day.String() != s || again.Opt == nil || again.Opt.String() != s {
			t.Errorf("%s: after a save, read day %s, opt %v", s, again.Day, again.Opt)
		}
	}
	first, err := ParseDate(days[0])
	if err != nil {
		t.Fatal(err)
	}
	blank := dateLocationRow{Day: first}
	if err := db.Create(&blank).Error; err != nil {
		t.Fatal(err)
	}
	var empty dateLocationRow
	if err := db.First(&empty, blank.ID).Error; err != nil {
		t.Fatal(err)
	}
	if empty.Opt != nil {
		t.Errorf("null opt read back as %v", empty.Opt)
	}
}

// TestDateDriverLocationSQLite: mattn/go-sqlite3 returns a date column in
// its _loc, so UTC midnight, the stored value, is 19:00 of the day before in
// New York (#478). The stored text stays what Value has always written, so
// rows written before keep matching.
func TestDateDriverLocationSQLite(t *testing.T) {
	for _, c := range []struct{ name, param string }{
		{"no _loc", ""},
		{"_loc west of UTC", "?_loc=America%2FNew_York"},
		{"_loc east of UTC", "?_loc=Asia%2FTokyo"},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "dates.db")+c.param), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			checkDateLocation(t, db, dateLocationDays, "CAST(%s AS TEXT)", func(day string) string {
				return day + " 00:00:00+00:00"
			})
		})
	}
}

// TestDateScanDriverTimes: the times the drivers return for a stored date.
// SQLite and PostgreSQL return the UTC midnight Value wrote, in whatever
// location; MySQL returns midnight of the stored day in its loc, which
// time.Date moves when a DST change skips it: to 23:00 the day before in
// America/Sao_Paulo on 2018-11-04, to 01:00 in Asia/Beirut on 2024-03-31.
// Where a zone skipped a whole day, time.Date gives that day's midnight and
// the eve's as the same time: the eve reads as itself, and the skipped day,
// which no Scan can tell from it, as the eve. Any other time keeps its own
// calendar day, as before. The two "changed" cases are the two kinds of time
// Scan reads differently from NewDate.
func TestDateScanDriverTimes(t *testing.T) {
	t.Parallel()
	zone := func(name string) *time.Location {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatal(err)
		}
		return loc
	}
	ny, tokyo, saoPaulo := zone("America/New_York"), zone("Asia/Tokyo"), zone("America/Sao_Paulo")
	apia, kiritimati, kwajalein := zone("Pacific/Apia"), zone("Pacific/Kiritimati"), zone("Pacific/Kwajalein")
	utcMidnight := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	for _, c := range []struct {
		name string
		src  time.Time
		want string
	}{
		{"UTC midnight", utcMidnight(2024, 1, 1), "2024-01-01"},
		{"changed: UTC midnight west of UTC is its UTC day", utcMidnight(2024, 1, 1).In(ny), "2024-01-01"},
		{"UTC midnight in a fixed zone west of UTC", utcMidnight(2024, 2, 29).In(time.FixedZone("W", -3*3600)), "2024-02-29"},
		{"UTC midnight in Tokyo", utcMidnight(2024, 12, 31).In(tokyo), "2024-12-31"},
		{"New York midnight", time.Date(2024, 1, 1, 0, 0, 0, 0, ny), "2024-01-01"},
		{"Tokyo midnight", time.Date(2024, 12, 31, 0, 0, 0, 0, tokyo), "2024-12-31"},
		{"skipped midnight, west", time.Date(2018, 11, 4, 0, 0, 0, 0, saoPaulo), "2018-11-04"},
		{"changed: 23:00 exactly on the eve of a skipped midnight is the next day", time.Date(2018, 11, 3, 23, 0, 0, 0, saoPaulo), "2018-11-04"},
		{"22:59 on that eve", time.Date(2018, 11, 3, 22, 59, 0, 0, saoPaulo), "2018-11-03"},
		{"23:00 on an ordinary eve", time.Date(2018, 11, 2, 23, 0, 0, 0, saoPaulo), "2018-11-02"},
		{"skipped midnight, east", time.Date(2024, 3, 31, 0, 0, 0, 0, zone("Asia/Beirut")), "2024-03-31"},
		{"midnight on the eve of a day Pacific/Apia skipped", time.Date(2011, 12, 29, 0, 0, 0, 0, apia), "2011-12-29"},
		{"midnight on the eve of a day Pacific/Kiritimati skipped", time.Date(1994, 12, 30, 0, 0, 0, 0, kiritimati), "1994-12-30"},
		{"midnight on the eve of a day Pacific/Kwajalein skipped", time.Date(1993, 8, 20, 0, 0, 0, 0, kwajalein), "1993-08-20"},
		{"limitation: the day Pacific/Apia skipped reads as its eve", time.Date(2011, 12, 30, 0, 0, 0, 0, apia), "2011-12-29"},
		{"1ns after UTC midnight west of UTC is its own calendar day", utcMidnight(2024, 1, 1).Add(time.Nanosecond).In(ny), "2023-12-31"},
		{"late evening in New York", time.Date(2024, 1, 1, 23, 30, 0, 0, ny), "2024-01-01"},
		{"morning in Tokyo", time.Date(2024, 1, 1, 8, 0, 0, 0, tokyo), "2024-01-01"},
	} {
		var d Date
		if err := d.Scan(c.src); err != nil {
			t.Fatal(err)
		}
		if d.String() != c.want {
			t.Errorf("%s: Scan(%s) = %s; want %s", c.name, c.src.Format(time.RFC3339), d, c.want)
		}
	}
}

// skippedDays are the days the zones of dateSweepZones skipped whole.
// go-sql-driver/mysql returns each as the same time as the day before, so
// it reads as that day (see Scan).
var skippedDays = map[string]string{
	"Pacific/Apia":       "2011-12-30",
	"Pacific/Fakaofo":    "2011-12-30",
	"Pacific/Kanton":     "1994-12-31",
	"Pacific/Enderbury":  "1994-12-31",
	"Pacific/Kiritimati": "1994-12-31",
	"Pacific/Kwajalein":  "1993-08-21",
	"Kwajalein":          "1993-08-21",
}

// dateSweepZones are the zones that skipped a whole day, then zones with a
// DST change at midnight (America/Sao_Paulo, Asia/Beirut), a 30-minute DST
// shift (Australia/Lord_Howe), a fractional offset (Asia/Kathmandu,
// Asia/Kolkata, Pacific/Chatham) or an offset of +13 (Pacific/Tongatapu),
// and plain ones.
var dateSweepZones = []string{
	"Pacific/Apia", "Pacific/Fakaofo", "Pacific/Kanton", "Pacific/Enderbury",
	"Pacific/Kiritimati", "Pacific/Kwajalein", "Kwajalein",
	"America/Sao_Paulo", "Asia/Beirut", "Australia/Lord_Howe", "Asia/Kathmandu",
	"Asia/Kolkata", "Pacific/Chatham", "Pacific/Tongatapu",
	"America/New_York", "Asia/Tokyo", "UTC",
}

// TestDateScanZoneSweep: for every day from 1970 to 2037 in dateSweepZones,
// Scan reads the time go-sql-driver/mysql returns for that DATE (midnight in
// its loc, as time.Date gives it) as that day, except the days in
// skippedDays, which read as the day before.
func TestDateScanZoneSweep(t *testing.T) {
	t.Parallel()
	first, last := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2037, 12, 31, 0, 0, 0, 0, time.UTC)
	exceptions := 0
	for _, name := range dateSweepZones {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatal(err)
		}
		wrong := 0
		for day := first; !day.After(last); day = day.AddDate(0, 0, 1) {
			want := day
			if skippedDays[name] == day.Format(time.DateOnly) {
				want = day.AddDate(0, 0, -1)
				exceptions++
			}
			var d Date
			if err := d.Scan(time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)); err != nil {
				t.Fatal(err)
			}
			if !d.Time().Equal(want) {
				if wrong++; wrong <= 5 {
					t.Errorf("%s: DATE %s reads as %s; want %s", name, day.Format(time.DateOnly), d, want.Format(time.DateOnly))
				}
			}
		}
		if wrong > 5 {
			t.Errorf("%s: %d days read wrong in all", name, wrong)
		}
	}
	if exceptions != len(skippedDays) {
		t.Errorf("checked %d skipped days; want %d", exceptions, len(skippedDays))
	}
}

// TestDateGormValueByDialect: the parameter GORM binds for a Date, built
// without a database (DryRun): the "YYYY-MM-DD" text on MySQL and Value's
// UTC midnight on SQLite, for a write and every kind of condition alike. A
// zero Date is still refused by Value on both.
func TestDateGormValueByDialect(t *testing.T) {
	// A *gorm.DB holds its *gorm.Config, Dialector included: one each.
	dryRun := func() *gorm.Config {
		return &gorm.Config{DryRun: true, DisableAutomaticPing: true, SkipDefaultTransaction: true}
	}
	my, err := gorm.Open(mysql.New(mysql.Config{DSN: "u:p@tcp(127.0.0.1:1)/none?parseTime=true", SkipInitializeWithVersion: true}), dryRun())
	if err != nil {
		t.Fatal(err)
	}
	sq, err := gorm.Open(sqlite.Open(":memory:"), dryRun())
	if err != nil {
		t.Fatal(err)
	}
	bound := func(v any) (driver.Value, error) {
		if valuer, ok := v.(driver.Valuer); ok {
			return valuer.Value()
		}
		return v, nil
	}
	day, err := ParseDate("2024-01-01")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		db   *gorm.DB
		want driver.Value
	}{
		{"mysql", my, "2024-01-01"},
		{"sqlite", sq, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		var rows []dateLocationRow
		for name, tx := range map[string]*gorm.DB{
			"create":  c.db.Create(&dateLocationRow{Day: day, Opt: &day}),
			"save":    c.db.Save(&dateLocationRow{ID: 7, Day: day, Opt: &day}),
			"where":   c.db.Where("day = ?", day).Find(&rows),
			"pointer": c.db.Where("opt = ?", &day).Find(&rows),
			"struct":  c.db.Where(&dateLocationRow{Day: day}).Find(&rows),
			"in":      c.db.Where("day IN ?", []Date{day}).Find(&rows),
			"between": c.db.Where("day BETWEEN ? AND ?", day, day).Find(&rows),
		} {
			if tx.Error != nil {
				t.Fatalf("%s %s: %v", c.name, name, tx.Error)
			}
			stmt := tx.Statement
			var dates int
			for _, v := range stmt.Vars {
				got, err := bound(v)
				if err != nil {
					t.Fatalf("%s %s: %v", c.name, name, err)
				}
				if _, isID := got.(uint); isID {
					continue
				}
				dates++
				if gt, ok := got.(time.Time); ok {
					if wt, ok := c.want.(time.Time); ok && gt.Equal(wt) && gt.Location() == time.UTC {
						continue
					}
				}
				if got != c.want {
					t.Errorf("%s %s: bound %#v (%T); want %#v", c.name, name, got, v, c.want)
				}
			}
			if dates == 0 {
				t.Errorf("%s %s: no date bound in %#v", c.name, name, stmt.Vars)
			}
		}
		zero := c.db.Create(&dateLocationRow{}).Statement
		if len(zero.Vars) == 0 {
			t.Fatalf("%s: zero date: no vars (%v)", c.name, zero.Error)
		}
		if _, err := bound(zero.Vars[0]); err == nil {
			t.Errorf("%s: zero date bound as %#v", c.name, zero.Vars[0])
		}
	}
}
