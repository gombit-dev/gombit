package database

import (
	"context"
	"errors"
	"strings"

	"github.com/gombit-dev/gombit/contract"
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

// MapLoadError maps a GORM read/load error to a D10 category error:
// record-not-found becomes not_found; any other driver failure becomes
// internal. Unique/duplicate is not treated as conflict on load.
func MapLoadError(ctx context.Context, err error, notFound, internal string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.WithContext(ctx, contract.NotFound(notFound))
	}
	return contract.WithContext(ctx, contract.Internal(internal))
}

// MapPersistError maps a GORM write error to a D10 category error:
// unique/duplicate becomes conflict; a foreign-key or NOT NULL violation
// becomes validation, since both mean the client submitted a value that
// references or omits something invalid, not that the server failed; any
// other failure becomes internal.
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
