package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"sync"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	"gorm.io/gorm/utils"
)

// SlowQueryThreshold is how long a statement may run before the database
// logger reports it as slow.
const SlowQueryThreshold = 200 * time.Millisecond

// NewLogger returns a GORM logger that writes through z, so database logging
// follows the app's sink, level and format (issue #439):
//
//   - a statement that succeeds, finds nothing (gorm.ErrRecordNotFound), or
//     fails with a client error the API answers with a 4xx, identified by
//     the driver's code (a unique, foreign-key or NOT NULL violation) or a
//     model Validate error, is logged at debug: it is normal traffic;
//   - a slow statement (SlowQueryThreshold) at warn;
//   - any other failed statement at error.
//
// The logged SQL keeps its placeholders and never carries parameter values,
// which may be password hashes, tokens or personal data. A failed statement's
// driver error text is logged as the driver reports it.
//
// GORM's Debug() (LogMode(Info)) raises a session's statements to info, so
// they show at the default log level.
//
// framework.New installs it on an attached database that still has the logger
// Open installed.
func NewLogger(z *zap.Logger) gormlogger.Interface {
	// Scan's SQL is built by GORM's recorder, not by this logger, so the
	// guarantee above needs the recorder's filter too, whoever opened the
	// database (#439 review round 2).
	dropRecorderParams()
	if z == nil {
		z = zap.NewNop()
	}
	return zapLogger{z: z.WithOptions(zap.WithCaller(false)), level: gormlogger.Info}
}

type zapLogger struct {
	z     *zap.Logger
	level gormlogger.LogLevel
	// verbose is set by an explicit LogMode(Info), which is what GORM's
	// Debug() does: the caller asked to see the statements.
	verbose bool
}

func (l zapLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	l.level = level
	l.verbose = level >= gormlogger.Info
	return l
}

func (l zapLogger) Info(_ context.Context, msg string, data ...any) {
	if l.level >= gormlogger.Info {
		l.z.Info(fmt.Sprintf(msg, data...), zap.String("source", utils.FileWithLineNum()))
	}
}

func (l zapLogger) Warn(_ context.Context, msg string, data ...any) {
	if l.level >= gormlogger.Warn {
		l.z.Warn(fmt.Sprintf(msg, data...), zap.String("source", utils.FileWithLineNum()))
	}
}

func (l zapLogger) Error(_ context.Context, msg string, data ...any) {
	if l.level >= gormlogger.Error {
		l.z.Error(fmt.Sprintf(msg, data...), zap.String("source", utils.FileWithLineNum()))
	}
}

func (l zapLogger) Trace(_ context.Context, begin time.Time, fc func() (string, int64), err error) {
	if l.level <= gormlogger.Silent {
		return
	}
	elapsed := time.Since(begin)
	statement := zapcore.DebugLevel
	if l.verbose {
		statement = zapcore.InfoLevel
	}
	var level zapcore.Level
	var msg string
	switch {
	case err != nil && !errors.Is(err, gorm.ErrRecordNotFound) && !isClientWriteError(err):
		if l.level < gormlogger.Error {
			return
		}
		level, msg = zapcore.ErrorLevel, "database: query failed"
	case elapsed > SlowQueryThreshold:
		if l.level < gormlogger.Warn {
			return
		}
		level, msg = zapcore.WarnLevel, "database: slow query"
	default:
		if l.level < gormlogger.Info {
			return
		}
		level, msg = statement, "database: query"
	}
	// fc builds the SQL; skip it unless the entry is going to be written.
	entry := l.z.Check(level, msg)
	if entry == nil {
		return
	}
	sql, rows := fc()
	fields := []zap.Field{
		zap.String("sql", restorePlaceholders(sql)),
		zap.Duration("elapsed", elapsed),
		zap.String("source", utils.FileWithLineNum()),
	}
	if rows >= 0 {
		fields = append(fields, zap.Int64("rows", rows))
	}
	if err != nil {
		fields = append(fields, zap.Error(err))
	}
	entry.Write(fields...)
}

// ParamsFilter drops the statement's parameter values, so the logged SQL keeps
// its placeholders.
func (zapLogger) ParamsFilter(_ context.Context, sql string, _ ...any) (string, []any) {
	return sql, nil
}

// isClientWriteError reports whether err is, by a structured signal, one
// MapPersistError answers with a 4xx: a failed Validate hook, a unique or
// foreign-key violation (translated to gorm's sentinels from the driver's
// code by TranslateError), or a NOT NULL violation (the driver's own code).
//
// It deliberately never matches error text. The logger applies it to every
// statement, reads and DDL included, and text such as "no such table:
// unique_codes" or "Not unique table/alias" is a server error that must stay
// visible (#439 review). It is therefore a strict subset of what
// MapPersistError answers with a 4xx, whose text fallbacks serve databases
// opened without TranslateError.
func isClientWriteError(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve) ||
		errors.Is(err, gorm.ErrDuplicatedKey) ||
		errors.Is(err, gorm.ErrForeignKeyViolated) ||
		isNotNullViolationCode(err)
}

// isNotNullViolationCode reports a NOT NULL violation by the driver's error
// code: Postgres SQLSTATE 23502, MySQL error 1048, SQLite extended code 1299.
func isNotNullViolationCode(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23502"
	}
	var myErr *mysqldriver.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Number == 1048
	}
	return sqliteExtendedCode(err) == sqliteConstraintNotNull
}

// sqliteConstraintNotNull is SQLITE_CONSTRAINT_NOTNULL
// (https://www.sqlite.org/rescode.html#constraint_notnull).
const sqliteConstraintNotNull = 1299

// sqliteExtendedCode reads a SQLite driver error's extended result code the way
// gorm.io/driver/sqlite's translator does, through the error's exported
// fields, so this package still builds without cgo (CGO_ENABLED=0), where the
// go-sqlite3 types do not exist. It walks the wrap chain; 0 means none found.
func sqliteExtendedCode(err error) int {
	for ; err != nil; err = errors.Unwrap(err) {
		raw, marshalErr := json.Marshal(err)
		if marshalErr != nil {
			continue
		}
		var coded struct {
			ExtendedCode int `json:"ExtendedCode"`
		}
		if json.Unmarshal(raw, &coded) == nil && coded.ExtendedCode != 0 {
			return coded.ExtendedCode
		}
	}
	return 0
}

// postgresPlaceholder matches what GORM's Explain leaves of a Postgres
// placeholder ($1 becomes $1$) when, as here, it is given no values. A
// dollar-quote tag cannot start with a digit, so $1$ is never real SQL.
var postgresPlaceholder = regexp.MustCompile(`\$(\d+)\$`)

func restorePlaceholders(sql string) string {
	return postgresPlaceholder.ReplaceAllString(sql, `$$$1`)
}

// defaultLogger is what Open and OpenConn install: GORM's own text logger,
// moved off stdout onto stderr and made safe. It prints no parameter values,
// no colour codes, nothing for gorm.ErrRecordNotFound, and only warnings and
// failed statements.
type defaultLogger struct{ gormlogger.Interface }

func newDefaultLogger() gormlogger.Interface {
	return defaultLogger{gormlogger.New(log.New(os.Stderr, "", log.LstdFlags), gormlogger.Config{
		SlowThreshold:             SlowQueryThreshold,
		LogLevel:                  gormlogger.Warn,
		IgnoreRecordNotFoundError: true,
		ParameterizedQueries:      true,
		Colorful:                  false,
	})}
}

func (l defaultLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	return defaultLogger{l.Interface.LogMode(level)}
}

func (l defaultLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	l.Interface.Trace(ctx, begin, func() (string, int64) {
		sql, rows := fc()
		return restorePlaceholders(sql), rows
	}, err)
}

// dropRecorderParams makes GORM's trace recorder drop parameter values too.
// Scan (db.Raw(...).Scan, db.Model(...).Scan) does not ask the session's
// logger for ParamsFilter: it records the statement through
// gormlogger.Recorder, whose filter is the package-wide
// gormlogger.RecorderParamsFilter, and hands the recorded SQL to the logger
// already built, values inlined (#439 review). The setting is process-wide;
// Open, OpenConn and NewLogger make it on first use.
var dropRecorderParams = sync.OnceFunc(func() {
	gormlogger.RecorderParamsFilter = func(_ context.Context, sql string, _ ...any) (string, []any) {
		return sql, nil
	}
})

// ParamsFilter must be on the wrapper: GORM looks for it on the logger it was
// given, not on what that logger wraps.
func (defaultLogger) ParamsFilter(_ context.Context, sql string, _ ...any) (string, []any) {
	return sql, nil
}

// ReplaceDefaultLogger installs l as the database's GORM logger if it still
// has the exact logger Open or OpenConn installed, and reports whether it did.
// Any logger the caller set, including one derived from the default with
// LogMode (db.Logger = db.Logger.LogMode(gormlogger.Silent)), is kept. Sessions
// and transactions derived from db afterwards use l; ones derived before keep
// the logger they copied.
func (db *DB) ReplaceDefaultLogger(l gormlogger.Interface) bool {
	if db == nil || db.DB == nil || l == nil || db.installedLogger == nil {
		return false
	}
	if db.Logger != db.installedLogger {
		return false
	}
	db.Logger = l
	return true
}
