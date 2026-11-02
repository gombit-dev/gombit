package database

import (
	"context"
	"errors"
	"net/http"
	"testing"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/gombit-dev/gombit/contract"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

func TestIsUniqueViolation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "ErrDuplicatedKey", err: gorm.ErrDuplicatedKey, want: true},
		{name: "wrapped ErrDuplicatedKey", err: errors.Join(errors.New("wrap"), gorm.ErrDuplicatedKey), want: true},
		{name: "sqlite unique", err: errors.New("UNIQUE constraint failed: widgets.sku"), want: true},
		{name: "postgres duplicate", err: errors.New("ERROR: duplicate key value violates unique constraint \"widgets_sku_key\" (SQLSTATE 23505)"), want: true},
		{name: "mysql duplicate", err: errors.New("Error 1062 (23000): Duplicate entry 'a' for key 'widgets.sku'"), want: true},
		{name: "record not found", err: gorm.ErrRecordNotFound, want: false},
		{name: "no such table", err: errors.New("no such table: widgets"), want: false},
		{name: "generic", err: errors.New("connection refused"), want: false},
		// A name holding "unique" or "duplicate" in another violation's
		// message is not a duplicate (issue #456).
		{name: "sqlite not null on unique_code", err: errors.New("NOT NULL constraint failed: repro_uniq.unique_code"), want: false},
		{name: "no such table unique_codes", err: errors.New("no such table: unique_codes"), want: false},
		{name: "postgres 23505", err: &pgconn.PgError{Code: "23505", Message: `duplicate key value violates unique constraint "widgets_sku_key"`}, want: true},
		{name: "postgres 23502 on unique_code", err: &pgconn.PgError{Code: "23502", Message: `null value in column "unique_code" of relation "duplicate_codes" violates not-null constraint`}, want: false},
		{name: "postgres 42P01 duplicate_codes", err: &pgconn.PgError{Code: "42P01", Message: `relation "duplicate_codes" does not exist`}, want: false},
		{name: "mysql 1062", err: &mysqldriver.MySQLError{Number: 1062, Message: "Duplicate entry 'a' for key 'widgets.sku'"}, want: true},
		{name: "mysql 1586", err: &mysqldriver.MySQLError{Number: 1586, Message: "Duplicate entry 'a' for key 'uniq_sku'"}, want: true},
		{name: "mysql 1048 on duplicate_of", err: &mysqldriver.MySQLError{Number: 1048, Message: "Column 'duplicate_of' cannot be null"}, want: false},
		{name: "mysql 1146 unique_codes", err: &mysqldriver.MySQLError{Number: 1146, Message: "Table 'app.unique_codes' doesn't exist"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsUniqueViolation(tt.err); got != tt.want {
				t.Fatalf("IsUniqueViolation(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsForeignKeyViolation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "ErrForeignKeyViolated", err: gorm.ErrForeignKeyViolated, want: true},
		{name: "wrapped ErrForeignKeyViolated", err: errors.Join(errors.New("wrap"), gorm.ErrForeignKeyViolated), want: true},
		{name: "sqlite foreign key", err: errors.New("FOREIGN KEY constraint failed"), want: true},
		{name: "postgres foreign key", err: errors.New("ERROR: insert or update on table \"widgets\" violates foreign key constraint \"fk_widgets_category\" (SQLSTATE 23503)"), want: true},
		{name: "mysql foreign key", err: errors.New("Error 1452 (23000): Cannot add or update a child row: a foreign key constraint fails"), want: true},
		{name: "unique, not foreign key", err: errors.New("UNIQUE constraint failed: widgets.sku"), want: false},
		{name: "generic", err: errors.New("connection refused"), want: false},
		{name: "postgres 23503", err: &pgconn.PgError{Code: "23503", Message: `insert or update on table "widgets" violates foreign key constraint "fk_widgets_category"`}, want: true},
		{name: "postgres 23502", err: &pgconn.PgError{Code: "23502", Message: `null value in column "category_id" violates not-null constraint`}, want: false},
		{name: "mysql 1451", err: &mysqldriver.MySQLError{Number: 1451, Message: "Cannot delete or update a parent row: a foreign key constraint fails"}, want: true},
		{name: "mysql 1452", err: &mysqldriver.MySQLError{Number: 1452, Message: "Cannot add or update a child row: a foreign key constraint fails"}, want: true},
		{name: "mysql 1048", err: &mysqldriver.MySQLError{Number: 1048, Message: "Column 'foreign_key_id' cannot be null"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsForeignKeyViolation(tt.err); got != tt.want {
				t.Fatalf("IsForeignKeyViolation(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsNotNullViolation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "sqlite not null", err: errors.New("NOT NULL constraint failed: widgets.name"), want: true},
		{name: "postgres not null", err: errors.New("ERROR: null value in column \"name\" of relation \"widgets\" violates not-null constraint (SQLSTATE 23502)"), want: true},
		{name: "mysql not null", err: errors.New("Error 1048 (23000): Column 'name' cannot be null"), want: true},
		{name: "unique, not null-related", err: errors.New("UNIQUE constraint failed: widgets.sku"), want: false},
		{name: "foreign key, not null-related", err: errors.New("FOREIGN KEY constraint failed"), want: false},
		{name: "generic", err: errors.New("connection refused"), want: false},
		{name: "postgres 23502", err: &pgconn.PgError{Code: "23502", Message: `null value in column "unique_code" violates not-null constraint`}, want: true},
		{name: "postgres 23505 on not_null_key", err: &pgconn.PgError{Code: "23505", Message: `duplicate key value violates unique constraint "not null constraint failed"`}, want: false},
		{name: "mysql 1048", err: &mysqldriver.MySQLError{Number: 1048, Message: "Column 'name' cannot be null"}, want: true},
		{name: "mysql 1062", err: &mysqldriver.MySQLError{Number: 1062, Message: "Duplicate entry 'cannot be null' for key 'widgets.name'"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsNotNullViolation(tt.err); got != tt.want {
				t.Fatalf("IsNotNullViolation(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestMapLoadError(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "nil", err: nil, wantStatus: 0, wantCode: ""},
		{name: "not found", err: gorm.ErrRecordNotFound, wantStatus: http.StatusNotFound, wantCode: "not_found"},
		{name: "wrapped not found", err: errors.Join(errors.New("wrap"), gorm.ErrRecordNotFound), wantStatus: http.StatusNotFound, wantCode: "not_found"},
		{name: "no such table", err: errors.New("no such table: widgets"), wantStatus: http.StatusInternalServerError, wantCode: "internal"},
		{name: "unique on load", err: errors.New("UNIQUE constraint failed: widgets.sku"), wantStatus: http.StatusInternalServerError, wantCode: "internal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MapLoadError(ctx, tt.err, "widget not found", "load widget")
			assertMapped(t, got, tt.err, tt.wantStatus, tt.wantCode)
		})
	}
}

func TestMapPersistError(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "nil", err: nil, wantStatus: 0, wantCode: ""},
		{name: "sqlite unique", err: errors.New("UNIQUE constraint failed: widgets.sku"), wantStatus: http.StatusConflict, wantCode: "conflict"},
		{name: "ErrDuplicatedKey", err: gorm.ErrDuplicatedKey, wantStatus: http.StatusConflict, wantCode: "conflict"},
		{name: "sqlite foreign key", err: errors.New("FOREIGN KEY constraint failed"), wantStatus: http.StatusUnprocessableEntity, wantCode: contract.CodeValidationError},
		{name: "ErrForeignKeyViolated", err: gorm.ErrForeignKeyViolated, wantStatus: http.StatusUnprocessableEntity, wantCode: contract.CodeValidationError},
		{name: "sqlite not null", err: errors.New("NOT NULL constraint failed: widgets.name"), wantStatus: http.StatusUnprocessableEntity, wantCode: contract.CodeValidationError},
		{name: "no such table", err: errors.New("no such table: widgets"), wantStatus: http.StatusInternalServerError, wantCode: "internal"},
		{name: "not found", err: gorm.ErrRecordNotFound, wantStatus: http.StatusInternalServerError, wantCode: "internal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MapPersistError(ctx, tt.err, "resource already exists", "persist widget")
			assertMapped(t, got, tt.err, tt.wantStatus, tt.wantCode)
		})
	}
}

func TestMapPersistErrorSQLiteUniqueConstraint(t *testing.T) {
	db := openSQLite(t)
	type uniqueWidget struct {
		ID  uint   `gorm:"primaryKey"`
		SKU string `gorm:"uniqueIndex;size:64;not null"`
	}
	if err := db.AutoMigrate(&uniqueWidget{}); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	if err := db.Create(&uniqueWidget{SKU: "dup"}).Error; err != nil {
		t.Fatalf("first Create() error = %v", err)
	}
	err := db.Create(&uniqueWidget{SKU: "dup"}).Error
	if err == nil {
		t.Fatal("second Create() error = nil, want unique violation")
	}
	if !IsUniqueViolation(err) {
		t.Fatalf("IsUniqueViolation(%v) = false, want true", err)
	}
	mapped := MapPersistError(context.Background(), err, "resource already exists", "persist widget")
	assertMapped(t, mapped, err, http.StatusConflict, "conflict")
}

func TestMapDeleteError(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "nil", err: nil, wantStatus: 0, wantCode: ""},
		{name: "sqlite foreign key", err: errors.New("FOREIGN KEY constraint failed"), wantStatus: http.StatusConflict, wantCode: "conflict"},
		{name: "ErrForeignKeyViolated", err: gorm.ErrForeignKeyViolated, wantStatus: http.StatusConflict, wantCode: "conflict"},
		{name: "unique is not a delete conflict here", err: errors.New("UNIQUE constraint failed: widgets.sku"), wantStatus: http.StatusInternalServerError, wantCode: "internal"},
		{name: "generic", err: errors.New("connection refused"), wantStatus: http.StatusInternalServerError, wantCode: "internal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MapDeleteError(ctx, tt.err, "resource is still referenced by other records", "delete widget")
			assertMapped(t, got, tt.err, tt.wantStatus, tt.wantCode)
		})
	}
}

func TestMapDeleteErrorSQLiteForeignKeyConstraint(t *testing.T) {
	db := openSQLite(t)
	type deleteFKParent struct {
		ID uint `gorm:"primaryKey"`
	}
	type deleteFKChild struct {
		ID       uint `gorm:"primaryKey"`
		ParentID uint
		Parent   deleteFKParent `gorm:"constraint:OnUpdate:CASCADE,OnDelete:RESTRICT;"`
	}
	if err := db.AutoMigrate(&deleteFKParent{}, &deleteFKChild{}); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	parent := deleteFKParent{}
	if err := db.Create(&parent).Error; err != nil {
		t.Fatalf("create parent: %v", err)
	}
	if err := db.Create(&deleteFKChild{ParentID: parent.ID}).Error; err != nil {
		t.Fatalf("create child: %v", err)
	}

	err := db.Delete(&parent).Error
	if err == nil {
		t.Fatal("Delete() error = nil, want foreign key violation (parent still referenced)")
	}
	if !IsForeignKeyViolation(err) {
		t.Fatalf("IsForeignKeyViolation(%v) = false, want true", err)
	}
	mapped := MapDeleteError(context.Background(), err, "resource is still referenced by other records", "delete widget")
	assertMapped(t, mapped, err, http.StatusConflict, "conflict")
}

func TestMapPersistErrorSQLiteForeignKeyConstraint(t *testing.T) {
	db := openSQLite(t)
	type fkCategory struct {
		ID uint `gorm:"primaryKey"`
	}
	type fkWidget struct {
		ID         uint `gorm:"primaryKey"`
		CategoryID uint
		Category   fkCategory `gorm:"constraint:OnUpdate:CASCADE,OnDelete:RESTRICT;"`
	}
	if err := db.AutoMigrate(&fkCategory{}, &fkWidget{}); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	err := db.Create(&fkWidget{CategoryID: 999}).Error
	if err == nil {
		t.Fatal("Create() error = nil, want foreign key violation")
	}
	if !IsForeignKeyViolation(err) {
		t.Fatalf("IsForeignKeyViolation(%v) = false, want true", err)
	}
	mapped := MapPersistError(context.Background(), err, "resource already exists", "persist widget")
	assertMapped(t, mapped, err, http.StatusUnprocessableEntity, contract.CodeValidationError)
}

func TestMapPersistErrorSQLiteNotNullConstraint(t *testing.T) {
	db := openSQLite(t)
	type notNullWidget struct {
		ID   uint    `gorm:"primaryKey"`
		Name *string `gorm:"not null"`
	}
	if err := db.AutoMigrate(&notNullWidget{}); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	err := db.Create(&notNullWidget{Name: nil}).Error
	if err == nil {
		t.Fatal("Create() error = nil, want NOT NULL violation")
	}
	if !IsNotNullViolation(err) {
		t.Fatalf("IsNotNullViolation(%v) = false, want true", err)
	}
	mapped := MapPersistError(context.Background(), err, "resource already exists", "persist widget")
	assertMapped(t, mapped, err, http.StatusUnprocessableEntity, contract.CodeValidationError)
}

// A NOT NULL violation on a column named unique_code, in a table named
// duplicate_codes, is a 422, not a 409: no other row exists to conflict with
// (issue #456).
func TestMapPersistErrorSQLiteNotNullOnUniqueNamedColumn(t *testing.T) {
	db := openSQLite(t)
	type duplicateCode struct {
		ID         uint    `gorm:"primaryKey"`
		UniqueCode *string `gorm:"not null"`
	}
	if err := db.AutoMigrate(&duplicateCode{}); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	err := db.Create(&duplicateCode{}).Error
	if err == nil {
		t.Fatal("Create() error = nil, want NOT NULL violation")
	}
	if IsUniqueViolation(err) {
		t.Fatalf("IsUniqueViolation(%v) = true, want false", err)
	}
	if !IsNotNullViolation(err) {
		t.Fatalf("IsNotNullViolation(%v) = false, want true", err)
	}
	mapped := MapPersistError(context.Background(), err, "resource already exists", "persist code")
	assertMapped(t, mapped, err, http.StatusUnprocessableEntity, contract.CodeValidationError)
}

func assertMapped(t *testing.T, got error, original error, wantStatus int, wantCode string) {
	t.Helper()
	if original == nil {
		if got != nil {
			t.Fatalf("mapped nil error = %v, want nil", got)
		}
		return
	}
	var env *contract.ErrorEnvelope
	if !errors.As(got, &env) {
		t.Fatalf("mapped type = %T (%v), want *contract.ErrorEnvelope", got, got)
	}
	if env.GetStatus() != wantStatus {
		t.Fatalf("status = %d, want %d; code = %q", env.GetStatus(), wantStatus, env.Body.Code)
	}
	if env.Body.Code != wantCode {
		t.Fatalf("code = %q, want %q", env.Body.Code, wantCode)
	}
}
