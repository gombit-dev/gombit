package framework

import (
	"path/filepath"
	"testing"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	gormlogger "gorm.io/gorm/logger"
)

func openSQLite(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.Open(config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    "file:" + filepath.Join(t.TempDir(), "app.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// New routes an attached database's logging through the app's logger, so it
// follows GOMBIT_LOG_SINK / GOMBIT_LOG_LEVEL instead of GORM's own stdout
// logger (issue #439).
func TestNewRoutesDatabaseLoggingThroughTheAppLogger(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	db := openSQLite(t)
	app := newTestApp(t, WithDatabase(db), WithLogger(zap.New(core)))

	if err := app.Database().Exec("SELECT * FROM missing_table").Error; err == nil {
		t.Fatal("query on a missing table succeeded")
	}
	failures := logs.FilterMessage("database: query failed").All()
	if len(failures) != 1 || failures[0].LoggerName != "database" || failures[0].Level != zapcore.ErrorLevel {
		t.Fatalf("app logger entries = %v, want one database error entry", logs.All())
	}
}

// A GORM logger the application set on its database is its choice: New keeps
// it, including the default quieted with GORM's LogMode idiom.
func TestNewKeepsADatabaseLoggerTheAppChose(t *testing.T) {
	db := openSQLite(t)
	db.Logger = gormlogger.Discard
	newTestApp(t, WithDatabase(db))
	if db.Logger != gormlogger.Discard {
		t.Fatal("New replaced the database logger the application set")
	}

	core, logs := observer.New(zapcore.DebugLevel)
	quiet := openSQLite(t)
	quiet.Logger = quiet.Logger.LogMode(gormlogger.Silent)
	app := newTestApp(t, WithDatabase(quiet), WithLogger(zap.New(core)))
	_ = app.Database().Exec("SELECT * FROM missing_table").Error
	if logs.Len() != 0 {
		t.Fatalf("a database the app silenced logged through the app: %v", logs.All())
	}
}
