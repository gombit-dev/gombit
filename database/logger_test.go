package database

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mattn/go-sqlite3"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

type loggedUser struct {
	ID           uint   `gorm:"primaryKey"`
	Email        string `gorm:"size:191;uniqueIndex"`
	PasswordHash string `gorm:"size:191"`
}

type loggedStock struct {
	ID  uint `gorm:"primaryKey"`
	Qty int  `gorm:"check:logged_stock_qty_positive,qty > 0"`
}

type loggedNote struct {
	ID   uint    `gorm:"primaryKey"`
	Body *string `gorm:"size:191;not null"`
}

const secretHash = "$2a$10$SECRET-HASH-THAT-MUST-NEVER-BE-LOGGED"

// The parameter values exercise passes. None may appear in a log line.
var loggedValues = []string{secretHash, "a@example.com", "nobody@example.com", "c@example.com", "d@example.com", "-7"}

func openSQLiteLoggedDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    "file:" + filepath.Join(t.TempDir(), "log.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func migrateLogged(t *testing.T, db *DB) {
	t.Helper()
	if err := db.AutoMigrate(&loggedUser{}, &loggedStock{}, &loggedNote{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&loggedUser{}, &loggedStock{}, &loggedNote{}) })
}

// exercise runs what issue #439 reproduced, plus server-side failures: a lookup
// that finds nothing, a duplicate insert carrying a password hash (a 409), a
// NOT NULL violation (a 422), a CHECK violation (a 500), a statement on a
// missing table, and the same through Scan, which GORM traces through its
// recorder rather than the session logger.
func exercise(t *testing.T, db *DB) {
	t.Helper()
	if err := db.Create(&loggedUser{Email: "a@example.com", PasswordHash: secretHash}).Error; err != nil {
		t.Fatal(err)
	}
	var u loggedUser
	if err := db.Where("email = ?", "nobody@example.com").First(&u).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("First() = %v, want not found", err)
	}
	if err := db.Create(&loggedUser{Email: "a@example.com", PasswordHash: secretHash}).Error; !IsUniqueViolation(err) {
		t.Fatalf("duplicate Create() = %v, want a unique violation", err)
	}
	if err := db.Create(&loggedNote{}).Error; !IsNotNullViolation(err) {
		t.Fatalf("NULL insert = %v, want a NOT NULL violation", err)
	}
	if err := db.Create(&loggedStock{Qty: -7}).Error; err == nil {
		t.Fatal("CHECK violation was accepted")
	}
	if err := db.Exec("SELECT * FROM missing_table WHERE email = ?", "c@example.com").Error; err == nil {
		t.Fatal("query on a missing table succeeded")
	}
	var found []loggedUser
	if err := db.Model(&loggedUser{}).Where("password_hash = ?", secretHash).Scan(&found).Error; err != nil || len(found) != 1 {
		t.Fatalf("Model().Scan() = %d rows, %v", len(found), err)
	}
	var none []loggedUser
	if err := db.Raw("SELECT * FROM missing_scan_table WHERE email = ?", "d@example.com").Scan(&none).Error; err == nil {
		t.Fatal("Raw().Scan() on a missing table succeeded")
	}
}

// testLoggerOnDriver proves the zap logger's contract on a real driver: the
// 409 and the not-found lookup are debug, the CHECK violation and the missing
// table are errors, and no entry carries a parameter value or a mangled
// placeholder. The integration tests run it on Postgres and MySQL.
func testLoggerOnDriver(t *testing.T, db *DB) {
	t.Helper()
	migrateLogged(t, db)
	core, logs := observer.New(zapcore.DebugLevel)
	db.Logger = NewLogger(zap.New(core))
	exercise(t, db)

	var failures []string
	for _, e := range logs.All() {
		sql, _ := e.ContextMap()["sql"].(string)
		errText, _ := e.ContextMap()["error"].(string)
		for _, leaked := range loggedValues {
			if strings.Contains(sql, leaked) || strings.Contains(errText, leaked) {
				t.Errorf("logged entry carries the value %q: %s %v", leaked, e.Message, e.ContextMap())
			}
		}
		if strings.Contains(sql, "$1$") {
			t.Errorf("logged SQL has a mangled Postgres placeholder: %s", sql)
		}
		if errText != "" && isNormalTraffic(errText) && e.Level != zapcore.DebugLevel {
			t.Errorf("a not-found, duplicate or NOT NULL statement is normal traffic, got %v: %v", e.Level, e.ContextMap())
		}
		if e.Level == zapcore.ErrorLevel {
			failures = append(failures, sql)
		}
	}
	if len(failures) != 3 ||
		!strings.Contains(strings.ToLower(failures[0]), "logged_stocks") ||
		!strings.Contains(failures[1], "missing_table") ||
		!strings.Contains(failures[2], "missing_scan_table") {
		t.Fatalf("error entries = %q, want the CHECK violation, the missing table, and the Scan on a missing table", failures)
	}
	if !strings.Contains(failures[2], "?") && !strings.Contains(failures[2], "$1") {
		t.Errorf("the Scan statement lost its placeholder: %s", failures[2])
	}
}

// isNormalTraffic recognises the driver texts of the statements exercise
// expects at debug: not found, duplicate key, NOT NULL.
func isNormalTraffic(errText string) bool {
	lower := strings.ToLower(errText)
	for _, s := range []string{"record not found", "duplicate", "not null", "not-null", "cannot be null"} {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

func TestNewLoggerOnSQLite(t *testing.T) {
	testLoggerOnDriver(t, openSQLiteLoggedDB(t))
}

// NewLogger keeps its no-values guarantee on a database it did not open:
// installed on a plain gorm.Open, Scan's SQL still carries no values. The
// recorder filter is process-wide and set once, so this runs in a fresh process.
func TestNewLoggerDropsScanValuesOnAPlainGormDB(t *testing.T) {
	if os.Getenv("GOMBIT_PLAIN_GORM_SCAN") == "1" {
		core, logs := observer.New(zapcore.DebugLevel)
		gdb, err := gorm.Open(sqlite.Open("file:"+filepath.Join(t.TempDir(), "plain.db")), &gorm.Config{Logger: NewLogger(zap.New(core))})
		if err != nil {
			t.Fatal(err)
		}
		var out []loggedUser
		_ = gdb.Raw("SELECT * FROM missing_table WHERE email = ?", "plain@example.com").Scan(&out).Error
		for _, e := range logs.All() {
			if sql, _ := e.ContextMap()["sql"].(string); strings.Contains(sql, "plain@example.com") {
				t.Fatalf("Scan on a plain gorm.DB logged the value: %s", sql)
			}
		}
		if logs.Len() == 0 {
			t.Fatal("the failed Scan was not logged")
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestNewLoggerDropsScanValuesOnAPlainGormDB$", "-test.count=1") //nolint:gosec // re-runs this test binary
	cmd.Env = append(os.Environ(), "GOMBIT_PLAIN_GORM_SCAN=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fresh process: %v\n%s", err, out)
	}
}

// Open's default logger replaces GORM's: stderr instead of stdout, no
// parameter values, no colour codes, nothing for a not-found lookup.
func TestOpenDefaultLoggerIsQuietAndNeverPrintsValues(t *testing.T) {
	stdout, stderr := captureStd(t)
	db := openSQLiteLoggedDB(t)
	migrateLogged(t, db)
	exercise(t, db)
	out, errOut := stdout(), stderr()

	if out != "" {
		t.Errorf("database logging must not reach stdout, got:\n%s", out)
	}
	for _, leaked := range loggedValues {
		if strings.Contains(errOut, leaked) {
			t.Errorf("logged a parameter value %q:\n%s", leaked, errOut)
		}
	}
	if strings.Contains(errOut, "record not found") {
		t.Errorf("a not-found lookup is normal traffic, not an error:\n%s", errOut)
	}
	if strings.Contains(errOut, "\x1b[") {
		t.Errorf("logged ANSI colour codes:\n%s", errOut)
	}
	if !strings.Contains(errOut, "missing_table") {
		t.Errorf("a failed statement must still be reported:\n%s", errOut)
	}
}

// The logger demotes a failure to debug only on a structured signal, and every
// error it demotes is one MapPersistError answers with a 4xx, so a 500 is never
// hidden. Error text that merely mentions "unique" or "not null", from a read
// or DDL, stays a failure.
func TestLoggerDemotesOnlyStructuredClientErrors(t *testing.T) {
	// A real driver error: sqlite3.Error's message text is unexported.
	db := openSQLiteLoggedDB(t)
	migrateLogged(t, db)
	sqliteNotNull := db.Create(&loggedNote{}).Error
	var liteErr sqlite3.Error
	if !errors.As(sqliteNotNull, &liteErr) {
		t.Fatalf("NULL insert = %#v, want a sqlite3.Error", sqliteNotNull)
	}
	for name, err := range map[string]error{
		"translated duplicate": gorm.ErrDuplicatedKey,
		"translated FK":        gorm.ErrForeignKeyViolated,
		"validation":           &ValidationError{Message: "bad"},
		"postgres NOT NULL":    &pgconn.PgError{Code: "23502", Message: `null value in column "name" violates not-null constraint`},
		"mysql NOT NULL":       &mysqldriver.MySQLError{Number: 1048, Message: "Column 'name' cannot be null"},
		"sqlite NOT NULL":      sqliteNotNull,
		"wrapped duplicate":    fmt.Errorf("create user: %w", gorm.ErrDuplicatedKey),
	} {
		if !isClientWriteError(err) {
			t.Errorf("%s: not demoted", name)
		}
		var envelope *contract.ErrorEnvelope
		if !errors.As(MapPersistError(context.Background(), err, "conflict", "internal"), &envelope) || envelope.GetStatus() >= http.StatusInternalServerError {
			t.Errorf("%s: demoted, but MapPersistError answers it with a 500", name)
		}
	}
	for name, err := range map[string]error{
		"sqlite missing table":     errors.New("no such table: unique_codes"),
		"mysql join alias":         &mysqldriver.MySQLError{Number: 1066, Message: "Not unique table/alias: 'users'"},
		"sqlite NOT NULL DDL":      errors.New("Cannot add a NOT NULL column with default value NULL"),
		"postgres ON CONFLICT":     &pgconn.PgError{Code: "42P10", Message: "there is no unique or exclusion constraint matching the ON CONFLICT specification"},
		"untranslated check":       gorm.ErrCheckConstraintViolated,
		"untranslated unique text": errors.New("UNIQUE constraint failed: users.email"),
		"connection":               errors.New("connection refused"),
	} {
		if isClientWriteError(err) {
			t.Errorf("%s: demoted to debug, but it is a server error", name)
		}
	}
}

func trace(l gormlogger.Interface, begin time.Time, err error) (calls int) {
	l.Trace(context.Background(), begin, func() (string, int64) {
		calls++
		return `SELECT * FROM "users" WHERE id = $1$ AND name = $2$`, 1
	}, err)
	return calls
}

func TestNewLoggerLevels(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	l := NewLogger(zap.New(core))

	// At info a successful statement is not written, and its SQL is never built.
	if calls := trace(l, time.Now(), nil); calls != 0 || logs.Len() != 0 {
		t.Fatalf("a debug entry at info level built the SQL %d times and wrote %d entries", calls, logs.Len())
	}
	trace(l, time.Now().Add(-time.Second), nil)
	if e := logs.TakeAll(); len(e) != 1 || e[0].Level != zapcore.WarnLevel || e[0].Message != "database: slow query" {
		t.Fatalf("a slow statement = %v, want one warn entry", e)
	}
	trace(l, time.Now(), errors.New("connection refused"))
	e := logs.TakeAll()
	if len(e) != 1 || e[0].Level != zapcore.ErrorLevel {
		t.Fatalf("a failed statement = %v, want one error entry", e)
	}
	if sql := e[0].ContextMap()["sql"]; sql != `SELECT * FROM "users" WHERE id = $1 AND name = $2` {
		t.Errorf("logged SQL = %q, want Postgres placeholders restored", sql)
	}

	// GORM's Debug() is LogMode(Info): it raises statements to info.
	trace(l.LogMode(gormlogger.Info), time.Now(), nil)
	if e := logs.TakeAll(); len(e) != 1 || e[0].Level != zapcore.InfoLevel {
		t.Fatalf("a Debug() statement = %v, want one info entry", e)
	}
	// Silent is silent, failures included.
	trace(l.LogMode(gormlogger.Silent), time.Now(), errors.New("connection refused"))
	if logs.Len() != 0 {
		t.Fatalf("Silent wrote %v", logs.All())
	}
}

// Only the exact logger Open installed is replaced: a logger the caller set,
// including the default quieted with GORM's LogMode idiom, is kept.
func TestReplaceDefaultLoggerKeepsTheCallersLogger(t *testing.T) {
	for name, choose := range map[string]func(*DB) gormlogger.Interface{
		"another logger":         func(*DB) gormlogger.Interface { return gormlogger.Discard },
		"the default, LogMode'd": func(db *DB) gormlogger.Interface { return db.Logger.LogMode(gormlogger.Silent) },
	} {
		t.Run(name, func(t *testing.T) {
			db := openSQLiteLoggedDB(t)
			chosen := choose(db)
			db.Logger = chosen
			if db.ReplaceDefaultLogger(NewLogger(zap.NewNop())) {
				t.Fatal("ReplaceDefaultLogger replaced a logger the caller set")
			}
			if fmt.Sprint(db.Logger) != fmt.Sprint(chosen) {
				t.Fatal("the caller's logger was changed")
			}
		})
	}
	db := openSQLiteLoggedDB(t)
	if !db.ReplaceDefaultLogger(NewLogger(zap.NewNop())) {
		t.Fatal("ReplaceDefaultLogger left Open's own logger in place")
	}
}

// captureStd redirects os.Stdout and os.Stderr for the rest of the test and
// returns readers of what was written.
func captureStd(t *testing.T) (stdout, stderr func() string) {
	t.Helper()
	capture := func(target **os.File) func() string {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		orig := *target
		*target = w
		done := make(chan string, 1)
		go func() {
			b, _ := io.ReadAll(r)
			done <- string(b)
		}()
		var out *string
		restore := func() string {
			if out == nil {
				*target = orig
				_ = w.Close()
				s := <-done
				out = &s
			}
			return *out
		}
		t.Cleanup(func() { restore() })
		return restore
	}
	return capture(&os.Stdout), capture(&os.Stderr)
}
