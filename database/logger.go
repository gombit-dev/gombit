package database

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"time"

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
//     fails with a client error the API answers with a 4xx (the unique,
//     foreign-key and NOT NULL violations and the validation errors
//     MapPersistError classifies) is logged at debug: it is normal traffic;
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

// isClientWriteError reports whether err is one MapPersistError answers with a
// 4xx: a failed Validate hook, or a unique, foreign-key or NOT NULL violation.
// Anything else it answers with a 500, and the logger reports it as a failure.
func isClientWriteError(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve) || IsUniqueViolation(err) || IsForeignKeyViolation(err) || IsNotNullViolation(err)
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
