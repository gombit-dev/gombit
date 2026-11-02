package database

import (
	"context"
	"errors"
	"strings"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/gombit-dev/gombit/contract"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

// IsUniqueViolation reports duplicate-key errors. Open enables
// gorm.Config.TranslateError, so ErrDuplicatedKey is the primary signal on
// SQLite, Postgres, and MySQL (all three dialectors translate their unique-
// violation error code to it). An untranslated driver error is classified by
// its code (driverConstraint), never by its text: a NOT NULL violation on a
// column named unique_code is not a duplicate (issue #456). The fixed wording
// of gorm's sentinel and of each driver's message stays as a fallback for an
// error that carries no driver error the framework knows (isConstraint).
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	return isConstraint(err, constraintUnique,
		duplicatedKeyText,            // gorm.ErrDuplicatedKey, flattened
		"unique constraint failed",   // SQLite
		"violates unique constraint", // PostgreSQL
		"duplicate entry",            // MySQL
	)
}

// The text of gorm's translated sentinels, lower case: what the fallback sees
// of a translation gorm flattened into a later error (isConstraint).
var (
	duplicatedKeyText      = strings.ToLower(gorm.ErrDuplicatedKey.Error())
	foreignKeyViolatedText = strings.ToLower(gorm.ErrForeignKeyViolated.Error())
)

// IsForeignKeyViolation reports foreign-key constraint violations, such as a
// belongs_to reference to a row that does not exist. Same primary/fallback
// shape as IsUniqueViolation: all three supported dialectors translate their
// foreign-key error code to gorm.ErrForeignKeyViolated; an untranslated
// driver error is classified by its code, and only an error without one by
// the drivers' message.
func IsForeignKeyViolation(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrForeignKeyViolated) {
		return true
	}
	return isConstraint(err, constraintForeignKey,
		foreignKeyViolatedText,            // gorm.ErrForeignKeyViolated, flattened
		"foreign key constraint failed",   // SQLite
		"violates foreign key constraint", // PostgreSQL
		"a foreign key constraint fails",  // MySQL
	)
}

// IsNotNullViolation reports NOT NULL constraint violations. Unlike unique
// and foreign-key violations, none of the SQLite/Postgres/MySQL GORM
// dialectors translate this case to a gorm sentinel error (it is a gap in
// gorm itself, not something Open configures around), so detection is by
// the driver's code, with the drivers' messages as the fallback: SQLite ("NOT
// NULL constraint failed"), Postgres ("violates not-null constraint"), MySQL
// ("cannot be null").
func IsNotNullViolation(err error) bool {
	if err == nil {
		return false
	}
	return isConstraint(err, constraintNotNull,
		"not null constraint failed",   // SQLite
		"violates not-null constraint", // PostgreSQL
		"cannot be null",               // MySQL
	)
}

// constraintKind is the constraint a driver error reports violated.
type constraintKind int

const (
	constraintOther constraintKind = iota
	constraintUnique
	constraintForeignKey
	constraintNotNull
)

// SQLite extended result codes (https://www.sqlite.org/rescode.html).
const (
	sqliteConstraintForeignKey = 787
	sqliteConstraintNotNull    = 1299
	sqliteConstraintPrimaryKey = 1555
	sqliteConstraintTrigger    = 1811
	sqliteConstraintUnique     = 2067
)

// isConstraint reports whether err violates a constraint of kind: by the
// driver's code when err carries a driver error the framework knows, else by
// the fixed wording of that violation (messages, lower case).
//
// The wording fallback sees two kinds of error: one from a driver or dialect
// the code table does not know (a database opened without TranslateError, a
// pure-Go SQLite), and, from Open/OpenConn, gorm's own sentinel flattened to
// text: when a second error follows a translated violation on one statement
// (a rollback failing after a cancelled context), gorm.AddError joins them
// with fmt.Errorf("%v; %w"), so errors.Is no longer finds the sentinel. The
// phrases are therefore gorm's sentinel texts as well as the drivers'
// messages. Only whole phrases are matched, so an identifier a message quotes,
// such as a column named unique_code, cannot match on its own; text a client
// partly controls (a value quoted by an earlier, flattened error) still
// reaches the match, it just has to spell a whole phrase.
func isConstraint(err error, kind constraintKind, messages ...string) bool {
	if got, ok := driverConstraint(err); ok {
		return got == kind
	}
	msg := strings.ToLower(err.Error())
	for _, m := range messages {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// driverConstraint classifies err by the driver's own code: PostgreSQL
// SQLSTATE 23505 / 23503 / 23502, MySQL errors 1062, 1586, 1761 and 1762 /
// 1216, 1217, 1451 and 1452 / 1048 and 1364, SQLite extended codes 2067 and 1555 / 787 (and 1811
// with SQLite's foreign-key wording) / 1299 (unique / foreign key / NOT NULL). ok is false when err carries none of the
// three drivers' errors; any other code of theirs is constraintOther.
func driverConstraint(err error) (kind constraintKind, ok bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return constraintUnique, true
		case "23503":
			return constraintForeignKey, true
		case "23502":
			return constraintNotNull, true
		}
		return constraintOther, true
	}
	var myErr *mysqldriver.MySQLError
	if errors.As(err, &myErr) {
		switch myErr.Number {
		case 1062, 1586, 1761, 1762:
			// 1761/1762: an ON UPDATE CASCADE that would duplicate a child's key.
			return constraintUnique, true
		case 1216, 1217, 1451, 1452:
			return constraintForeignKey, true
		case 1048, 1364:
			// 1364: an INSERT that omits a NOT NULL column with no default
			// (Omit, Select, a map Create), under strict mode.
			return constraintNotNull, true
		}
		return constraintOther, true
	}
	if code := sqliteExtendedCode(err); code != 0 {
		switch code {
		case sqliteConstraintUnique, sqliteConstraintPrimaryKey:
			return constraintUnique, true
		case sqliteConstraintForeignKey:
			return constraintForeignKey, true
		case sqliteConstraintTrigger:
			// An ON DELETE RESTRICT refusal is reported under the trigger
			// code, with SQLite's fixed foreign-key wording; an app
			// trigger's RAISE(ABORT, ...) shares the code with its own text.
			if strings.Contains(strings.ToLower(err.Error()), "foreign key constraint failed") {
				return constraintForeignKey, true
			}
		case sqliteConstraintNotNull:
			return constraintNotNull, true
		}
		return constraintOther, true
	}
	return constraintOther, false
}

// IsDataException reports a value the database refused as data that a
// client sends and can fix by sending another (issue #444): PostgreSQL's
// string too long (22001), numeric out of range (22003), invalid datetime
// format or field overflow (22007, 22008), and a NUL byte or invalid UTF-8
// (22021, 22P05); MySQL's out-of-range (1264), truncated (1265), too-long
// (1406) and incorrect-string (1366) errors, and an incorrect date or time
// literal (1292). MapPersistError and MapLoadError answer it with a 422.
//
// It is an allowlist of the codes a value the client sends can cause. A code
// does not say what caused it, so the same code raised by server-side SQL (an
// expression overflowing its column) is a 422 too. The rest of PostgreSQL's
// class 22 (division by zero, an invalid cast) and MySQL's "Truncated
// incorrect ... value" on an expression stay 500s.
func IsDataException(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "22001", "22003", "22007", "22008", "22021", "22P05":
			return true
		}
		return false
	}
	var myErr *mysqldriver.MySQLError
	if errors.As(err, &myErr) {
		switch myErr.Number {
		case 1264, 1265, 1366, 1406:
			return true
		case 1292:
			return strings.HasPrefix(myErr.Message, "Incorrect ")
		}
	}
	return false
}

// dataExceptionError is the 422 for a data exception. It names no field: a
// driver names a column, if at all, not the field the API names, and the
// write checks that run before the SQL name the field where they can.
func dataExceptionError(ctx context.Context) error {
	return contract.WithContext(ctx, contract.Validation("The request contains a value the database cannot store.", nil))
}

// MapLoadError maps a GORM read/load error to a D10 category error:
// record-not-found becomes not_found; a data exception (a filter value the
// column type cannot hold) becomes validation; any other driver failure
// becomes internal. Unique/duplicate is not treated as conflict on load.
func MapLoadError(ctx context.Context, err error, notFound, internal string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.WithContext(ctx, contract.NotFound(notFound))
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		return contract.WithContext(ctx, contract.Validation(ve.Message, ve.Fields))
	}
	if IsDataException(err) {
		return dataExceptionError(ctx)
	}
	return contract.WithContext(ctx, contract.Internal(internal))
}

// MapPersistError maps a GORM write error to a D10 category error:
// unique/duplicate becomes conflict; a foreign-key or NOT NULL violation, or
// a data exception (a value the column cannot hold), becomes validation,
// since each means the client submitted a value that references, omits or
// spells something invalid, not that the server failed; any other failure
// becomes internal.
func MapPersistError(ctx context.Context, err error, conflict, internal string) error {
	if err == nil {
		return nil
	}
	// A domain Validate hook failure is an intentional 422 with field detail,
	// not a driver error — check it before the constraint classifiers.
	var ve *ValidationError
	if errors.As(err, &ve) {
		return contract.WithContext(ctx, contract.Validation(ve.Message, ve.Fields))
	}
	if IsUniqueViolation(err) {
		return contract.WithContext(ctx, contract.Conflict(conflict))
	}
	if IsForeignKeyViolation(err) {
		return contract.WithContext(ctx, contract.Validation("The request references a resource that does not exist.", nil))
	}
	if IsNotNullViolation(err) {
		return contract.WithContext(ctx, contract.Validation("The request is missing a required value.", nil))
	}
	if IsDataException(err) {
		return dataExceptionError(ctx)
	}
	return contract.WithContext(ctx, contract.Internal(internal))
}

// MapDeleteError maps a GORM delete error to a D10 category error. A
// foreign-key violation here means another row still references the one
// being deleted, which is a state conflict caused by other data, not an
// invalid value in the request (a delete has no body to validate) — the
// opposite meaning a foreign-key violation has on create/update, so this is
// deliberately not MapPersistError. Unique and NOT NULL violations cannot
// occur on delete, so there is no equivalent branch for them here.
func MapDeleteError(ctx context.Context, err error, conflict, internal string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrReferenced) || IsForeignKeyViolation(err) {
		return contract.WithContext(ctx, contract.Conflict(conflict))
	}
	return contract.WithContext(ctx, contract.Internal(internal))
}
