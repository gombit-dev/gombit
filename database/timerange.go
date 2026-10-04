package database

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/gombit-dev/gombit/types"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

var (
	rangeTimeType     = reflect.TypeOf(time.Time{})
	rangeNullTimeType = reflect.TypeOf(sql.NullTime{})
	rangeDateType     = reflect.TypeOf(types.Date{})
)

// registerTimeRangeCallback refuses, before the SQL runs on every create and
// update, a timestamp or date outside what every supported database can store
// and return (issue #443): types.TimeWithin / types.DateWithin. PostgreSQL
// stored year 0 as 1 BC, after which every read of the row failed to encode,
// and MySQL refused it. The failure is a *ValidationError naming the field, so
// MapPersistError answers it with a D10 422 on the generated API, the admin
// data plane and any other write alike: the bound has one home, like Validate.
//
// The check is keyed on the assignment set, the columns a statement actually
// writes: a create's rows (or map), an update's map or the struct it was given
// (matched to the model's columns by name, any struct type), filtered by
// Select/Omit as GORM filters them. An update's model is checked only when it
// is that struct (Save, Updates(&row)); otherwise it holds the row's old
// values, which the statement does not write, and refusing them would block
// every other change to a row that already holds one.
func registerTimeRangeCallback(db *gorm.DB) error {
	if err := db.Use(&timeRangeGuard{}); err != nil {
		return fmt.Errorf("database: register time range callbacks: %w", err)
	}
	return nil
}

// timeRangeGuard is the gombit:timerange check as a GORM plugin, so its
// per-schema cache of ranged columns belongs to one *gorm.DB and is released
// with it, rather than pinning every opened database's schemas in a process
// global.
type timeRangeGuard struct {
	ranged sync.Map // *schema.Schema -> map[string]*schema.Field
}

func (*timeRangeGuard) Name() string { return "gombit:timerange" }

func (g *timeRangeGuard) Initialize(db *gorm.DB) error {
	create := db.Callback().Create().Before("gorm:create")
	if err := create.Register("gombit:timerange", func(tx *gorm.DB) { g.run(tx, true) }); err != nil {
		return fmt.Errorf("create: %w", err)
	}
	update := db.Callback().Update().Before("gorm:update")
	if err := update.Register("gombit:timerange", func(tx *gorm.DB) { g.run(tx, false) }); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	return nil
}

func (g *timeRangeGuard) run(db *gorm.DB, creating bool) {
	if db.Error != nil || db.Statement == nil || db.Statement.Schema == nil {
		return
	}
	targets := g.rangedTimeFields(db.Statement.Schema)
	if len(targets) == 0 {
		return
	}
	c := timeRangeCheck{skipHooks: db.Statement.SkipHooks}
	forEachAssigned(db, creating, targets, c.check)
	if len(c.fields) > 0 {
		_ = db.AddError(NewValidationError("The request contains invalid fields.", c.fields))
	}
}

type timeRangeCheck struct {
	skipHooks bool
	fields    map[string][]string // allocated on the first problem
}

func (c *timeRangeCheck) check(a assignedValue) {
	f := a.Field
	if a.FromStruct {
		// GORM writes now over an auto-update timestamp on a struct update that
		// runs hooks, whatever the struct holds.
		if !a.Creating && !c.skipHooks && f.AutoUpdateTime > 0 {
			return
		}
		// A zero struct value GORM does not write is left alone. GORM's zero
		// is reflect's (field.ValueOf), not the instant's: a zero time in a
		// Location, a non-nil pointer to one, or a valid NullTime holding
		// one is written, and refused below as the zero instant. On a
		// create GORM fills a zero auto timestamp and the database a zero
		// defaulted column, Select or not. On an update GORM writes a zero
		// struct field only when Select names the column or selects "*"
		// (Save); Updates skips it.
		if a.Value.IsValid() && a.Value.IsZero() {
			if a.Creating {
				if f.AutoCreateTime > 0 || f.AutoUpdateTime > 0 || f.HasDefaultValue {
					return
				}
			} else if !a.Named && !a.AllSelected {
				return
			}
		}
	}
	msg := timeRangeProblem(f, a.Value)
	if msg == "" {
		return
	}
	key := fieldKey(f)
	for _, have := range c.fields[key] {
		if have == msg {
			return
		}
	}
	if c.fields == nil {
		c.fields = map[string][]string{}
	}
	c.fields[key] = append(c.fields[key], msg)
}

// rangedTimeFields returns sch's ranged columns by DBName, cached per schema
// for this DB, so a model with none skips the check without reflecting over
// anything.
func (g *timeRangeGuard) rangedTimeFields(sch *schema.Schema) map[string]*schema.Field {
	if cached, ok := g.ranged.Load(sch); ok {
		return cached.(map[string]*schema.Field)
	}
	out := map[string]*schema.Field{}
	for _, f := range sch.Fields {
		if isRangedTimeField(f) {
			out[f.DBName] = f
		}
	}
	g.ranged.Store(sch, out)
	return out
}

// isRangedTimeField reports whether f is a stored time.Time, sql.NullTime or
// types.Date column, pointer or not. Auto create/update timestamps are
// included: GORM fills them only when they are zero, and keeps a value a
// caller sets (a hook, a seeder, a map update naming updated_at).
func isRangedTimeField(f *schema.Field) bool {
	if f.DBName == "" {
		return false
	}
	t := f.FieldType
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t == rangeTimeType || t == rangeNullTimeType || t == rangeDateType ||
		// Named time types: gorm.DeletedAt (a sql.NullTime), type Stamp time.Time.
		(t.Kind() == reflect.Struct && (t.ConvertibleTo(rangeNullTimeType) || t.ConvertibleTo(rangeTimeType)))
}

// timeRangeProblem describes why v, assigned to column, cannot be stored and
// returned on every driver, or returns "" when it can. A string headed for the
// column is read as the drivers read it (RFC 3339, "YYYY-MM-DD hh:mm:ss" with
// or without an offset, "YYYY-MM-DD"); one that does not parse is refused,
// since PostgreSQL accepts forms ('infinity', '... BC') no Go time can be read
// back from. Expressions arrive as clause values, not strings, and are left to
// the database, as is NULL (a nil pointer, an invalid sql.NullTime).
func timeRangeProblem(column *schema.Field, v reflect.Value) string {
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if !v.IsValid() {
		return ""
	}
	dateColumn := isRangedDateField(column)
	var t time.Time
	switch {
	case v.Type() == rangeDateType:
		t = readValue[types.Date](v).Time()
	case v.Type() == rangeTimeType:
		t = readValue[time.Time](v)
	case v.Kind() == reflect.Struct && v.Type().ConvertibleTo(rangeNullTimeType):
		nt := readConverted[sql.NullTime](v, rangeNullTimeType)
		if !nt.Valid {
			return ""
		}
		t = nt.Time
	case v.Kind() == reflect.Struct && v.Type().ConvertibleTo(rangeTimeType):
		t = readConverted[time.Time](v, rangeTimeType)
	case v.Kind() == reflect.String:
		return parsedRangeProblem(v.String(), dateColumn)
	case v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8:
		return parsedRangeProblem(string(v.Bytes()), dateColumn)
	default:
		return ""
	}
	return rangeProblem(t, dateColumn)
}

// readValue reads v as T without boxing it when v is addressable (a struct
// field), which is every value forEachAssigned reads from a struct.
func readValue[T any](v reflect.Value) T {
	if v.CanAddr() {
		return *(v.Addr().Interface().(*T))
	}
	return v.Interface().(T)
}

func readConverted[T any](v reflect.Value, to reflect.Type) T {
	return v.Convert(to).Interface().(T)
}

func parsedRangeProblem(raw string, dateColumn bool) string {
	t, ok := parseAssignedTime(raw)
	if !ok {
		if dateColumn {
			return "must be a date (YYYY-MM-DD)"
		}
		return "must be an RFC 3339 timestamp"
	}
	return rangeProblem(t, dateColumn)
}

func rangeProblem(t time.Time, dateColumn bool) string {
	var err error
	if dateColumn {
		err = types.DateWithin(types.NewDate(t))
	} else {
		err = types.TimeWithin(t)
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// assignedTimeLayouts are the forms a timestamp or date string takes on its way
// to a driver: RFC 3339, ISO without a zone, and what PostgreSQL and MySQL
// print themselves (a space, an offset of "+00", "+0000" or "+00:00", or a zone
// name such as UTC).
var assignedTimeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05.999999999-07",
	"2006-01-02T15:04:05.999999999-0700",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999-07",
	"2006-01-02 15:04:05.999999999-0700",
	"2006-01-02 15:04:05.999999999 MST",
	"2006-01-02 15:04:05.999999999",
	time.DateOnly,
}

// parseAssignedTime reads a string assigned to a timestamp or date column in
// the forms the drivers accept.
func parseAssignedTime(s string) (time.Time, bool) {
	for _, layout := range assignedTimeLayouts {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func isRangedDateField(f *schema.Field) bool {
	t := f.FieldType
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t == rangeDateType
}
