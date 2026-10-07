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
// violation error code to it). The driver error string stays as a fallback
// for any dialect that does not implement translation.
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate")
}

// IsForeignKeyViolation reports foreign-key constraint violations, such as a
// belongs_to reference to a row that does not exist. Same primary/fallback
// shape as IsUniqueViolation: all three supported dialectors translate their
// foreign-key error code to gorm.ErrForeignKeyViolated, with the driver
// error string as a fallback.
func IsForeignKeyViolation(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrForeignKeyViolated) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "foreign key")
}

// IsNotNullViolation reports NOT NULL constraint violations. Unlike unique
// and foreign-key violations, none of the SQLite/Postgres/MySQL GORM
// dialectors translate this case to a gorm sentinel error (it is a gap in
// gorm itself, not something Open configures around), so detection is
// driver-message-only. The three drivers phrase it differently: SQLite
// ("NOT NULL constraint failed"), Postgres ("violates not-null
// constraint"), MySQL ("cannot be null").
func IsNotNullViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not null") || strings.Contains(msg, "not-null") || strings.Contains(msg, "cannot be null")
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
