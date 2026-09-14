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
// `gombit` tag. Relationship/association fields have no column and so are not
// persisted columns and not included (a belongs-to FK column is). Feed the result
// to ResolveAll (or use ResolvedFromModel).
func FromSchema(sch *schema.Schema) []Field {
	out := make([]Field, 0, len(sch.DBNames))
	for _, name := range sch.DBNames {
		f := sch.FieldsByDBName[name]
		if f == nil {
			continue
		}
		out = append(out, Field{FieldFacts: FactsFromSchema(f), Tag: f.Tag.Get("gombit")})
	}
	return out
}

// FromModel parses a GORM model (a pointer to a value, e.g. &Book{}) and returns
// the resolver input for its effective persisted columns (see FromSchema).
func FromModel(model any) ([]Field, error) {
	sch, err := schema.Parse(model, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		return nil, fmt.Errorf("resourcepolicy: parse model schema: %w", err)
	}
	return FromSchema(sch), nil
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
