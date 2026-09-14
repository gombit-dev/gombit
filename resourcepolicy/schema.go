package resourcepolicy

import (
	"fmt"
	"reflect"
	"sync"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// FactsFromSchema is the lossless projection of a parsed GORM field into
// FieldFacts. Soft-delete is detected by the gorm.DeletedAt TYPE via
// IndirectFieldType (GORM dispatches its query/delete clauses on
// reflect.New(IndirectFieldType), so both `gorm.DeletedAt` and `*gorm.DeletedAt`
// are recognized); create/read capability comes from GORM's own Creatable/
// Readable (the `->`/`<-` permission tags); nothing is re-derived by hand.
func FactsFromSchema(f *schema.Field) FieldFacts {
	return FieldFacts{
		GoName:        f.Name,
		Column:        f.DBName,
		PrimaryKey:    f.PrimaryKey,
		AutoIncrement: f.AutoIncrement,
		AutoTime:      f.AutoCreateTime != 0 || f.AutoUpdateTime != 0,
		SoftDelete:    f.IndirectFieldType == reflect.TypeOf(gorm.DeletedAt{}),
		NotNull:       f.NotNull,
		HasDefault:    f.HasDefaultValue,
		Creatable:     f.Creatable,
		Readable:      f.Readable,
	}
}

// FromSchema maps a parsed GORM schema to resolver input over its EFFECTIVE
// persisted columns — ordered sch.DBNames → sch.FieldsByDBName — so a
// duplicate-column or shadowed field is included once, each carrying its own
// `gombit` tag. A belongs-to FK column is a persisted column and is included; the
// association object/slice fields (belongs-to/has-one/has-many/many2many) are
// not columns and are NOT emitted.
//
// Selection and validation are separate. An explicit `gombit` policy on a field
// that is not an effective persisted column (a relationship, a `gorm:"-"` ignored
// field, or a column shadowed by another field) would otherwise be silently
// dropped — it lives outside the emitted set. FromSchema therefore validates
// EVERY policy-bearing field from GORM's authoritative per-field set (sch.Fields),
// keyed by field identity, not a name-keyed secondary index that collapses
// same-named embedded fields. A policy that cannot be honored fails closed. Then
// it emits the effective persisted columns.
func FromSchema(sch *schema.Schema) ([]Field, error) {
	for _, f := range sch.Fields {
		if f.Tag.Get("gombit") == "" {
			continue // no explicit policy
		}
		if f.DBName != "" && sch.FieldsByDBName[f.DBName] == f {
			continue // the effective persisted column: validated when its Field is resolved
		}
		// A tagged field that is not the effective persisted column. If it has no
		// column (relationship or `gorm:"-"`), Resolve rejects the tag with the
		// right message; if it has a column but is shadowed, its policy can never
		// be honored, so reject it explicitly.
		if _, err := Resolve(FactsFromSchema(f), f.Tag.Get("gombit")); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("resourcepolicy: field %q carries a gombit policy but is shadowed by another field mapped to column %q; only the effective field may define policy", f.Name, f.DBName)
	}

	// Emit the effective persisted columns only.
	out := make([]Field, 0, len(sch.DBNames))
	for _, name := range sch.DBNames {
		f := sch.FieldsByDBName[name]
		if f == nil {
			continue
		}
		out = append(out, Field{FieldFacts: FactsFromSchema(f), Tag: f.Tag.Get("gombit")})
	}
	return out, nil
}

// FromModel parses a GORM model (a pointer to a value, e.g. &Book{}) and returns
// the resolver input for its effective persisted columns, after validating that
// no relationship field carries a gombit policy (see FromSchema).
func FromModel(model any) ([]Field, error) {
	sch, err := schema.Parse(model, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		return nil, fmt.Errorf("resourcepolicy: parse model schema: %w", err)
	}
	return FromSchema(sch)
}

// ResolvedFromModel parses a model and resolves the API policy for each of its
// effective persisted columns. It returns the first policy error (a contradictory
// tag or an unsatisfiable required column), so a bad model fails closed.
func ResolvedFromModel(model any) ([]Resolved, error) {
	fields, err := FromModel(model)
	if err != nil {
		return nil, err
	}
	return ResolveAll(fields)
}
