package database

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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

const secretHash = "$2a$10$SECRET-HASH-THAT-MUST-NEVER-BE-LOGGED"

// The parameter values exercise passes. None may appear in a log line.
var loggedValues = []string{secretHash, "a@example.com", "nobody@example.com", "c@example.com", "-7"}

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
	if err := db.AutoMigrate(&loggedUser{}, &loggedStock{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&loggedUser{}, &loggedStock{}) })
}

// exercise runs what issue #439 reproduced, plus a server-side failure: a
// lookup that finds nothing, a duplicate insert carrying a password hash (a
// 409), a CHECK violation (a 500), and a statement on a missing table.
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
	if err := db.Create(&loggedStock{Qty: -7}).Error; err == nil {
		t.Fatal("CHECK violation was accepted")
	}
	if err := db.Exec("SELECT * FROM missing_table WHERE email = ?", "c@example.com").Error; err == nil {
		t.Fatal("query on a missing table succeeded")
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
			if strings.Contains(sql, leaked) {
				t.Errorf("logged SQL carries the value %q: %s", leaked, sql)
			}
		}
		if strings.Contains(sql, "$1$") {
			t.Errorf("logged SQL has a mangled Postgres placeholder: %s", sql)
		}
		if strings.Contains(errText, "record not found") || strings.Contains(errText, "duplicate") || strings.Contains(errText, "Duplicate") {
			if e.Level != zapcore.DebugLevel {
				t.Errorf("a not-found or duplicate statement is normal traffic, got %v: %v", e.Level, e.ContextMap())
			}
		}
		if e.Level == zapcore.ErrorLevel {
			failures = append(failures, sql)
		}
	}
	if len(failures) != 2 {
		t.Fatalf("error entries = %q, want the CHECK violation and the missing table", failures)
	}
	if !strings.Contains(strings.ToLower(failures[0]), "logged_stocks") || !strings.Contains(failures[1], "missing_table") {
		t.Errorf("error entries = %q, want the CHECK violation then the missing table", failures)
	}
}

func TestNewLoggerOnSQLite(t *testing.T) {
	testLoggerOnDriver(t, openSQLiteLoggedDB(t))
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

// The logger's "client error" is MapPersistError's: an error it demotes to
// debug is exactly one the API answers with a 4xx, so a 500 is never hidden.
func TestLoggerClassifiesErrorsAsMapPersistErrorDoes(t *testing.T) {
	for _, err := range []error{
		gorm.ErrDuplicatedKey,
		gorm.ErrForeignKeyViolated,
		gorm.ErrCheckConstraintViolated,
		errors.New("NOT NULL constraint failed: widgets.name"),
		errors.New(`ERROR: null value in column "name" violates not-null constraint (SQLSTATE 23502)`),
		&ValidationError{Message: "bad"},
		errors.New("connection refused"),
		errors.New("CHECK constraint failed: qty"),
	} {
		mapped := MapPersistError(context.Background(), err, "conflict", "internal")
		var envelope *contract.ErrorEnvelope
		if !errors.As(mapped, &envelope) {
			t.Fatalf("MapPersistError(%v) = %v, want a contract error", err, mapped)
		}
		if client := envelope.GetStatus() < http.StatusInternalServerError; isClientWriteError(err) != client {
			t.Errorf("%v: logger client error = %v, MapPersistError status = %d", err, isClientWriteError(err), envelope.GetStatus())
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
