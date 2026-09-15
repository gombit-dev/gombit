package resourcepolicy

import (
	"fmt"
	"reflect"
	"strings"
	"sync"

	"gorm.io/gorm/schema"
)

// FactsFromSchema is the lossless projection of a parsed GORM field into
// FieldFacts. Soft-delete is detected BEHAVIORALLY, the way GORM dispatches it —
// the field's type implements the soft-delete clause interfaces (query + delete)
// on reflect.New(IndirectFieldType) — so gorm.DeletedAt, *gorm.DeletedAt, and any
// wrapper embedding it are all recognized, not just one concrete type. Create/read
// capability comes from GORM's own Creatable/Readable (the `->`/`<-` permission
// tags); nothing is re-derived by hand.
func FactsFromSchema(f *schema.Field) FieldFacts {
	return FieldFacts{
		GoName:        f.Name,
		Column:        f.DBName,
		PrimaryKey:    f.PrimaryKey,
		AutoIncrement: f.AutoIncrement,
		AutoTime:      f.AutoCreateTime != 0 || f.AutoUpdateTime != 0,
		SoftDelete:    isSoftDeleteField(f),
		NotNull:       f.NotNull,
		HasDefault:    f.HasDefaultValue,
		Creatable:     f.Creatable,
		Readable:      f.Readable,
	}
}

// isSoftDeleteField reports whether GORM treats the field as a soft-delete marker:
// its type implements both the query and delete clause interfaces GORM asserts
// during schema parsing (scope out deleted rows, turn DELETE into an UPDATE). This
// is the same behavioral dispatch GORM uses — matching gorm.DeletedAt and any type
// embedding it — rather than an exact concrete-type comparison.
func isSoftDeleteField(f *schema.Field) bool {
	if f.IndirectFieldType == nil {
		return false
	}
	v := reflect.New(f.IndirectFieldType).Interface()
	_, query := v.(schema.QueryClausesInterface)
	_, del := v.(schema.DeleteClausesInterface)
	return query && del
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
	// GORM flattens embedded-container fields into their children and discards the
	// container's non-gorm tags, so a gombit policy on the container never reaches
	// sch.Fields. Detect it from the Go struct (sch.ModelType) and fail closed —
	// container policy is unsupported (which child would it apply to?).
	if err := validateNoEmbeddedContainerPolicy(sch); err != nil {
		return nil, err
	}

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

// validateNoEmbeddedContainerPolicy rejects a gombit tag placed on an embedded
// container — a field GORM flattens into its children without carrying its
// non-gorm tags, so the policy would silently vanish before sch.Fields is built.
//
// It does NOT re-guess GORM's embedding rules (an anonymous time.Time or a
// Scanner/Valuer struct is a persisted column, not a container). Instead it asks
// GORM's parsed result directly: a source struct field is a container iff GORM
// kept no schema.Field at its bind path. Fields GORM did keep (columns,
// relationships, ignored fields) carry their tags into sch.Fields and are
// validated there; a policy on a non-kept field cannot be honored, so fail closed.
func validateNoEmbeddedContainerPolicy(sch *schema.Schema) error {
	kept := make(map[string]bool, len(sch.Fields))
	for _, f := range sch.Fields {
		kept[strings.Join(f.BindNames, ".")] = true // Go field names are dot-free identifiers
	}
	return walkContainerPolicy(sch.ModelType, nil, kept)
}

func walkContainerPolicy(t reflect.Type, prefix []string, kept map[string]bool) error {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" && !f.Anonymous { // unexported, non-embedded
			continue
		}
		path := append(append([]string(nil), prefix...), f.Name)
		key := strings.Join(path, ".")
		if kept[key] {
			continue // GORM kept this field; its policy (if any) is validated via sch.Fields
		}
		// Not kept: a flattened embedded container (or a dropped field). A policy
		// here vanished, so fail closed.
		if f.Tag.Get("gombit") != "" {
			return fmt.Errorf("resourcepolicy: field %q carries a gombit policy but GORM does not persist it as a column (it is an embedded container flattened into its children); tag the individual fields instead", key)
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct { // recurse into the flattened container
			if err := walkContainerPolicy(ft, path, kept); err != nil {
				return err
			}
		}
	}
	return nil
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
