package resourcepolicy

import (
	"fmt"
	"reflect"
	"sort"
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
// Selection and validation are separate: association fields live outside DBNames,
// so a `gombit` policy on one would never reach Resolve. FromSchema therefore
// VALIDATES every relationship field first (a relationship cannot carry API
// policy — the same rule Resolve enforces) and fails closed on a tagged one,
// rather than silently discarding the policy, before emitting the persisted set.
func FromSchema(sch *schema.Schema) ([]Field, error) {
	// Validate relationship fields (deterministic order for stable errors).
	rels := make([]*schema.Field, 0, len(sch.Relationships.Relations))
	for _, rel := range sch.Relationships.Relations {
		if rel.Field != nil {
			rels = append(rels, rel.Field)
		}
	}
	sort.Slice(rels, func(i, j int) bool { return rels[i].Name < rels[j].Name })
	for _, f := range rels {
		if _, err := Resolve(FactsFromSchema(f), f.Tag.Get("gombit")); err != nil {
			return nil, err
		}
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
