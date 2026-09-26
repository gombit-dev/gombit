package database

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// ErrReferenced reports a DELETE the database refused because other rows
// still reference the row under ON DELETE RESTRICT or NO ACTION.
var ErrReferenced = errors.New("database: the row is still referenced by other rows")

// Delete removes the rows value (and conds) select, physically. That is
// Gombit's deletion semantics (ADR-019): what deletion does is what the
// database's foreign keys say. RESTRICT and NO ACTION refuse the delete, and
// the error wraps ErrReferenced; CASCADE removes the referencing rows; SET
// NULL clears their key. It deletes even a model that embeds gorm.DeletedAt
// (gorm.Model): GORM would otherwise only set deleted_at, no foreign key would
// fire, and a live row would go on referencing one the API reports as gone.
// It returns the number of rows deleted.
func Delete(ctx context.Context, db *gorm.DB, value any, conds ...any) (int64, error) {
	if db == nil {
		return 0, errors.New("database: nil *gorm.DB")
	}
	res := db.WithContext(ctx).Unscoped().Delete(value, conds...)
	if res.Error != nil {
		if IsForeignKeyViolation(res.Error) {
			return 0, fmt.Errorf("%w: %w", ErrReferenced, res.Error)
		}
		return 0, res.Error
	}
	return res.RowsAffected, nil
}
