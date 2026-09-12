// Package resourcecheck guards against a generated resource's persistence schema
// and its generated API create-contract drifting silently into an invalid state
// (#218). `gombit make resource` generates a Huma handler whose create DTO is a
// snapshot of the fields given at generation time; because generated handlers are
// human-owned contracts, the generator cannot rewrite them when the model later
// gains a column. Left unchecked, a NOT NULL column the DTO doesn't know about is
// silently zero-filled on create (or the field is rejected as "unexpected
// property"), and nothing catches it until production.
//
// MissingCreateColumns is the invariant the generated `*_drift_test.go` asserts:
// every persistence-required column must have a known source.
package resourcecheck

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"gorm.io/gorm/schema"
)

// MissingCreateColumns reports the persistence-required columns of model that
// have no known source on create. A required column is one that is NOT NULL with
// no database default and is not auto-managed (primary key, auto-increment, auto
// create/update timestamp, or soft-delete). Such a column is satisfied — and so
// omitted from the result — when it is any of:
//
//   - writable through the create DTO (its json field name appears in createBody);
//   - explicitly server-managed (its column is listed in serverManaged, e.g. a
//     tenant or owner id the handler sets from the auth context);
//
// and it is never required in the first place when the column is nullable, has a
// DB default, or is auto-managed. Anything left over is model/handler drift.
//
// model is a pointer to the GORM model (e.g. &Widget{}); createBody is the create
// input's Body struct value (e.g. createWidgetInput{}.Body); serverManaged lists
// the columns the handler fills server-side. The returned column names are sorted.
//
// Only the create side is checked: a response DTO legitimately omits fields
// (intentional encapsulation), but a create DTO that cannot supply a
// persistence-required column violates a schema invariant.
func MissingCreateColumns(model any, createBody any, serverManaged []string) ([]string, error) {
	sch, err := schema.Parse(model, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		return nil, fmt.Errorf("resourcecheck: parse model schema: %w", err)
	}
	dto := jsonFieldNames(reflect.TypeOf(createBody))
	managed := toSet(serverManaged)

	var missing []string
	for _, f := range sch.Fields {
		if !requiresCreateValue(f) {
			continue
		}
		if dto[schemaJSONName(f)] || managed[f.DBName] {
			continue
		}
		missing = append(missing, f.DBName)
	}
	sort.Strings(missing)
	return missing, nil
}

// requiresCreateValue reports whether a column must be given a value on create:
// NOT NULL, no DB default, and not something the database or GORM fills itself.
func requiresCreateValue(f *schema.Field) bool {
	if f == nil || !f.NotNull || f.HasDefaultValue {
		return false
	}
	if f.PrimaryKey || f.AutoIncrement {
		return false
	}
	if f.AutoCreateTime != 0 || f.AutoUpdateTime != 0 {
		return false
	}
	// gorm.DeletedAt is nullable, but guard explicitly in case a model aliases it.
	if f.Name == "DeletedAt" {
		return false
	}
	return true
}

// schemaJSONName is the json field name a column is exposed under — matching how
// the generated create DTO names it — from the struct field's json tag, falling
// back to the GORM column name.
func schemaJSONName(f *schema.Field) string {
	if tag := f.StructField.Tag.Get("json"); tag != "" {
		if name, _, _ := strings.Cut(tag, ","); name != "" && name != "-" {
			return name
		}
	}
	return f.DBName
}

// jsonFieldNames collects the json field names of a struct type (the create
// DTO's Body). A non-struct type yields an empty set.
func jsonFieldNames(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return out
	}
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}

func toSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out[v] = true
		}
	}
	return out
}
