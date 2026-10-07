package database

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

var (
	floatNullType   = reflect.TypeOf(sql.NullFloat64{})
	floatValuerType = reflect.TypeOf((*driver.Valuer)(nil)).Elem()
)

// registerFloatCallback refuses, before the SQL runs on every create and
// update, a float a statement writes that is not finite (issue #449): +Inf,
// -Inf or NaN, including a float32 overflow and a string such as "Inf" bound
// for a float column, and a value beyond float32 range bound for a float32
// field. PostgreSQL and SQLite stored them (SQLite NaN as NULL) and MySQL
// refused them with a 500, and a stored one has no JSON form (or no float32
// one), so every response holding the row failed, the admin's list page and
// the generated API's alike. The failure is a *ValidationError naming the
// field, so MapPersistError answers it with a 422. What a statement writes is
// forEachAssigned's assignment set, as for the time range check.
func registerFloatCallback(db *gorm.DB) error {
	if err := db.Use(&floatGuard{}); err != nil {
		return fmt.Errorf("database: register float callbacks: %w", err)
	}
	return nil
}

// floatGuard is the gombit:float check as a GORM plugin, so its per-schema
// cache of float columns belongs to one *gorm.DB.
type floatGuard struct {
	columns sync.Map // *schema.Schema -> map[string]*schema.Field
}

func (*floatGuard) Name() string { return "gombit:float" }

func (g *floatGuard) Initialize(db *gorm.DB) error {
	create := db.Callback().Create().Before("gorm:create")
	if err := create.Register("gombit:float", func(tx *gorm.DB) { g.run(tx, true) }); err != nil {
		return fmt.Errorf("create: %w", err)
	}
	update := db.Callback().Update().Before("gorm:update")
	if err := update.Register("gombit:float", func(tx *gorm.DB) { g.run(tx, false) }); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	return nil
}

func (g *floatGuard) run(db *gorm.DB, creating bool) {
	if db.Error != nil || db.Statement == nil || db.Statement.Schema == nil {
		return
	}
	targets := g.floatFields(db.Statement.Schema)
	if len(targets) == 0 {
		return
	}
	var c floatCheck
	forEachAssigned(db, creating, targets, c.check)
	if len(c.fields) > 0 {
		_ = db.AddError(NewValidationError("The request contains invalid fields.", c.fields))
	}
}

type floatCheck struct {
	fields map[string][]string // allocated on the first problem
}

func (c *floatCheck) check(a assignedValue) {
	f, ok := floatValue(a.Value)
	if !ok {
		return
	}
	var msg string
	switch {
	case math.IsInf(f, 0) || math.IsNaN(f):
		msg = "must be a finite number"
	case float32Column(a.Field) && math.IsInf(float64(float32(f)), 0):
		// Finite as a float64, written through a map, Update or an upsert
		// literal: SQLite and PostgreSQL store it, and the row can no
		// longer be read into the float32 field.
		msg = fmt.Sprintf("must be between %g and %g", -math.MaxFloat32, math.MaxFloat32)
	default:
		return
	}
	key := fieldKey(a.Field)
	if len(c.fields[key]) > 0 {
		return
	}
	if c.fields == nil {
		c.fields = map[string][]string{}
	}
	c.fields[key] = []string{msg}
}

// float32Column reports a field whose Go value is a float32.
func float32Column(f *schema.Field) bool {
	t := f.FieldType
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Float32
}

// floatFields returns sch's float columns by DBName, cached per schema.
func (g *floatGuard) floatFields(sch *schema.Schema) map[string]*schema.Field {
	if cached, ok := g.columns.Load(sch); ok {
		return cached.(map[string]*schema.Field)
	}
	out := map[string]*schema.Field{}
	for _, f := range sch.Fields {
		if isFloatField(f) {
			out[f.DBName] = f
		}
	}
	actual, _ := g.columns.LoadOrStore(sch, out)
	return actual.(map[string]*schema.Field)
}

// isFloatField reports a float column the Go value is written to as is: a
// float32 or float64 (or a named float type), a pointer to one, or an
// sql.NullFloat64. A type with its own driver.Valuer writes something else,
// and is not checked.
func isFloatField(f *schema.Field) bool {
	if f.DBName == "" || f.Serializer != nil {
		return false
	}
	t := f.FieldType
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == floatNullType {
		return true
	}
	if t.Kind() != reflect.Float32 && t.Kind() != reflect.Float64 {
		return false
	}
	return !t.Implements(floatValuerType) && !reflect.PointerTo(t).Implements(floatValuerType)
}

// floatValue reads the number v writes to a float column. A nil pointer, an
// invalid NullFloat64 and an expression are not numbers, and are left to the
// database; a string is read as the drivers read it ("Inf", "NaN", "1e400"
// overflowing to +Inf all parse), and one that is no number is left to the
// database to refuse.
func floatValue(v reflect.Value) (float64, bool) {
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return 0, false
		}
		v = v.Elem()
	}
	switch {
	case !v.IsValid():
		return 0, false
	case v.Type() != floatNullType && v.Type().Implements(floatValuerType):
		return 0, false // writes what its Value returns, not this Go value
	case v.Kind() == reflect.Float32 || v.Kind() == reflect.Float64:
		return v.Float(), true
	case v.Type() == floatNullType:
		nf := readValue[sql.NullFloat64](v)
		return nf.Float64, nf.Valid
	case v.Kind() == reflect.String:
		f, err := strconv.ParseFloat(strings.TrimSpace(v.String()), 64)
		if err != nil && !math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	}
	return 0, false
}
