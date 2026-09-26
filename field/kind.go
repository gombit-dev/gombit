// Package field is the canonical logical field vocabulary shared by the
// resource generator, the admin data plane, and (as later issues land) OpenAPI,
// the TypeScript client, and generated forms.
//
// A Kind is a domain type, not a SQL column and not a wire string. Two
// projections hang off each kind:
//
//   - CLI tokens accepted by `gombit make resource` (resourcegen). Historical
//     tokens stay valid: `int`, `bool`, and `time` parse as integer, boolean,
//     and datetime. `time` is a datetime alias; a clock-time kind has no CLI
//     token until that kind is generated.
//   - Admin meta strings (`integer`, `boolean`, `datetime`, `relation`, …).
//     Finer kinds share a widget when the admin UI does not distinguish them
//     yet: integer64 and unsigned both emit `integer`.
//
// # Adding a kind
//
//  1. Add a Kind constant and one catalog entry in this file.
//  2. Set GoType, CLI tokens, AdminWire, and the capability flags.
//  3. Set GeneratorReady only when resourcegen emits the kind. Until then
//     `gombit make resource` rejects the token with a "not generated yet" error.
//  4. If the admin widget string is new, the admin SPA must learn it in the
//     same change. Reusing an existing AdminWire needs no SPA change.
//  5. Extend the doc table in docs/fields.md.
//
// A relation cardinality is not a new Kind. Add a RelationKind, a relationCaps
// row, and that token on the Relation catalog entry. Resourcegen still has a
// switch arm per cardinality when it emits the association; a catalog row
// alone does not.
//
// Do not add a parallel type list in resourcegen or admin. Those packages
// project this catalog. Capability flags on the spec are the filter, search,
// sort, and aggregate policy both generators run.
package field

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/gombit-dev/gombit/types"
)

// Kind is one logical field kind.
type Kind string

const (
	String    Kind = "string"
	Text      Kind = "text"
	Integer   Kind = "integer"
	Integer64 Kind = "integer64"
	Unsigned  Kind = "unsigned"
	Float     Kind = "float"
	Decimal   Kind = "decimal"
	Boolean   Kind = "boolean"
	Date      Kind = "date"
	DateTime  Kind = "datetime"
	// TimeOfDay is a clock time. It is not the `time` CLI token; that token
	// remains a datetime alias so existing resources keep generating time.Time.
	TimeOfDay Kind = "time"
	Duration  Kind = "duration"
	UUID      Kind = "uuid"
	JSON      Kind = "json"
	Email     Kind = "email"
	URL       Kind = "url"
	Slug      Kind = "slug"
	IP        Kind = "ip"
	Enum      Kind = "enum"
	Relation  Kind = "relation"
)

// RelationKind is the cardinality of a KindRelation field.
type RelationKind string

const (
	RelBelongsTo  RelationKind = "belongs_to"
	RelHasMany    RelationKind = "has_many"
	RelManyToMany RelationKind = "many_to_many"
	RelOneToOne   RelationKind = "one_to_one"
)

// Spec is the single definition of a kind: storage, generator readiness, and
// the two projections.
type Spec struct {
	Kind Kind
	// GoType is the generated Go type for a scalar the generator emits.
	// Empty for relations and for kinds that are not generated yet.
	GoType string
	// GeneratorReady is true when `gombit make resource` emits this kind.
	GeneratorReady bool
	// CLITokens are accepted by the resource grammar. The first entry is the
	// token printed in "supported types" errors. Empty means the kind is in
	// the vocabulary but has no grammar token yet.
	CLITokens []string
	// AdminWire is the admin meta `type` string. Empty means the kind has no
	// admin widget yet. Several kinds may share one wire.
	AdminWire string

	Filterable   bool
	Searchable   bool
	Sortable     bool
	Aggregatable bool
}

type relCaps struct {
	Filterable   bool
	Searchable   bool
	Sortable     bool
	Aggregatable bool
}

// catalog is the vocabulary. Order is the documentation order.
var catalog = []Spec{
	{Kind: String, GoType: "string", GeneratorReady: true, CLITokens: []string{"string"}, AdminWire: "string", Filterable: true, Searchable: true, Sortable: true},
	{Kind: Text, GoType: "string", GeneratorReady: true, CLITokens: []string{"text"}, AdminWire: "text", Searchable: true, Sortable: true},
	{Kind: Integer, GoType: "int", GeneratorReady: true, CLITokens: []string{"int", "integer"}, AdminWire: "integer", Filterable: true, Sortable: true, Aggregatable: true},
	{Kind: Integer64, GoType: "int64", GeneratorReady: true, CLITokens: []string{"int64", "integer64"}, AdminWire: "integer", Filterable: true, Sortable: true, Aggregatable: true},
	{Kind: Unsigned, GoType: "uint", GeneratorReady: true, CLITokens: []string{"uint", "unsigned"}, AdminWire: "integer", Filterable: true, Sortable: true, Aggregatable: true},
	{Kind: Float, GoType: "float64", GeneratorReady: true, CLITokens: []string{"float", "float64"}, AdminWire: "float", Sortable: true},
	{Kind: Decimal, GoType: "types.Decimal", GeneratorReady: true, CLITokens: []string{"decimal"}, AdminWire: "decimal", Sortable: true, Aggregatable: true},
	{Kind: Boolean, GoType: "bool", GeneratorReady: true, CLITokens: []string{"bool", "boolean"}, AdminWire: "boolean", Filterable: true, Sortable: true},
	{Kind: Date, GoType: "types.Date", GeneratorReady: true, CLITokens: []string{"date"}, AdminWire: "date", Sortable: true},
	{Kind: DateTime, GoType: "time.Time", GeneratorReady: true, CLITokens: []string{"time", "datetime"}, AdminWire: "datetime", Sortable: true},
	{Kind: TimeOfDay, Sortable: true},
	{Kind: Duration, CLITokens: []string{"duration"}, Sortable: true},
	{Kind: UUID, GoType: "uuid.UUID", GeneratorReady: true, CLITokens: []string{"uuid"}, AdminWire: "uuid", Filterable: true, Sortable: true},
	{Kind: JSON, GoType: "types.JSON", GeneratorReady: true, CLITokens: []string{"json"}, AdminWire: "json"},
	{Kind: Email, GoType: "string", GeneratorReady: true, CLITokens: []string{"email"}, AdminWire: "string", Sortable: true, Searchable: true},
	{Kind: URL, GoType: "string", GeneratorReady: true, CLITokens: []string{"url"}, AdminWire: "string", Sortable: true},
	{Kind: Slug, GoType: "string", GeneratorReady: true, CLITokens: []string{"slug"}, AdminWire: "string", Sortable: true, Searchable: true},
	{Kind: IP, GoType: "string", GeneratorReady: true, CLITokens: []string{"ip"}, AdminWire: "string", Sortable: true},
	{Kind: Enum, GoType: "string", GeneratorReady: true, CLITokens: []string{"enum"}, AdminWire: "string", Filterable: true, Searchable: true, Sortable: true},
	{Kind: Relation, GeneratorReady: true, CLITokens: []string{"belongs_to", "has_many", "many_to_many", "one_to_one"}, AdminWire: "relation"},
}

var relationCaps = map[RelationKind]relCaps{
	RelBelongsTo:  {Filterable: true, Sortable: true},
	RelHasMany:    {},
	RelManyToMany: {},
	RelOneToOne:   {Filterable: true, Sortable: true},
}

var (
	byKind    map[Kind]Spec
	byCLI     map[string]parsed
	adminWire map[string]struct{}
)

type parsed struct {
	kind Kind
	rel  RelationKind
}

func init() {
	if err := validateVocabulary(); err != nil {
		panic("field: " + err.Error())
	}
	byKind = make(map[Kind]Spec, len(catalog))
	byCLI = make(map[string]parsed)
	adminWire = make(map[string]struct{})
	for _, spec := range catalog {
		byKind[spec.Kind] = spec
		for _, tok := range spec.CLITokens {
			p := parsed{kind: spec.Kind}
			if spec.Kind == Relation {
				p.rel = RelationKind(tok)
			}
			byCLI[tok] = p
		}
		if spec.AdminWire != "" {
			adminWire[spec.AdminWire] = struct{}{}
		}
	}
}

// validateVocabulary fails when two catalog entries claim the same CLI token,
// or when a relation token and relationCaps disagree. init panics on that
// error so a colliding token cannot boot.
func validateVocabulary() error {
	seenKind := make(map[Kind]struct{}, len(catalog))
	seenTok := make(map[string]Kind)
	var relationTokens []string
	for _, spec := range catalog {
		if _, ok := seenKind[spec.Kind]; ok {
			return fmt.Errorf("duplicate kind %q", spec.Kind)
		}
		seenKind[spec.Kind] = struct{}{}
		if spec.Kind == Relation {
			relationTokens = spec.CLITokens
		}
		for _, tok := range spec.CLITokens {
			if prev, ok := seenTok[tok]; ok {
				return fmt.Errorf("duplicate CLI token %q on %s and %s", tok, prev, spec.Kind)
			}
			seenTok[tok] = spec.Kind
		}
	}
	for _, tok := range relationTokens {
		if _, ok := relationCaps[RelationKind(tok)]; !ok {
			return fmt.Errorf("relation token %q has no relationCaps entry", tok)
		}
	}
	for rel := range relationCaps {
		owner, ok := seenTok[string(rel)]
		if !ok || owner != Relation {
			return fmt.Errorf("relationCaps %q is not a Relation CLI token", rel)
		}
	}
	return nil
}

// Kinds returns every kind in catalog order.
func Kinds() []Kind {
	out := make([]Kind, len(catalog))
	for i, spec := range catalog {
		out[i] = spec.Kind
	}
	return out
}

// Lookup returns the spec for k.
func Lookup(k Kind) (Spec, bool) {
	spec, ok := byKind[k]
	return spec, ok
}

// ParseCLI parses a resource-grammar type token (no arguments). The bool is
// false when the token is not in the vocabulary. A recognized kind may still
// have GeneratorReady false; callers that emit code must check that.
//
// Relation tokens (`belongs_to`, `has_many`, `many_to_many`) return
// KindRelation and the relation kind.
func ParseCLI(token string) (Kind, RelationKind, bool) {
	p, ok := byCLI[strings.ToLower(strings.TrimSpace(token))]
	if !ok {
		return "", "", false
	}
	return p.kind, p.rel, true
}

// PreferredGeneratorTokens is the historical scalar grammar printed when a
// type token is unknown. Relation tokens are a separate grammar production.
func PreferredGeneratorTokens() []string {
	var out []string
	for _, spec := range catalog {
		if !spec.GeneratorReady || spec.Kind == Relation || len(spec.CLITokens) == 0 {
			continue
		}
		out = append(out, spec.CLITokens[0])
	}
	return out
}

// AdminWire is the admin meta type for this kind. The empty string means the
// kind has no admin widget.
func (k Kind) AdminWire() string {
	spec, ok := byKind[k]
	if !ok {
		return ""
	}
	return spec.AdminWire
}

// AdminWires returns the closed admin meta type set, in first-seen catalog order.
func AdminWires() []string {
	seen := make(map[string]struct{})
	var out []string
	for _, spec := range catalog {
		if spec.AdminWire == "" {
			continue
		}
		if _, ok := seen[spec.AdminWire]; ok {
			continue
		}
		seen[spec.AdminWire] = struct{}{}
		out = append(out, spec.AdminWire)
	}
	return out
}

// IsAdminWire reports whether s is an admin meta type string.
func IsAdminWire(s string) bool {
	_, ok := adminWire[s]
	return ok
}

// AllowsFilter reports whether an exact-match list filter can target this field.
func AllowsFilter(k Kind, rel RelationKind) bool {
	if k == Relation {
		return relationCaps[rel].Filterable
	}
	spec, ok := byKind[k]
	return ok && spec.Filterable
}

// AllowsSearch reports whether ?search= can LIKE this field.
func AllowsSearch(k Kind, rel RelationKind) bool {
	if k == Relation {
		return relationCaps[rel].Searchable
	}
	spec, ok := byKind[k]
	return ok && spec.Searchable
}

// AllowsSort reports whether the field maps to one orderable column.
func AllowsSort(k Kind, rel RelationKind) bool {
	if k == Relation {
		return relationCaps[rel].Sortable
	}
	spec, ok := byKind[k]
	return ok && spec.Sortable
}

// AllowsAggregate reports whether SUM/AVG/MIN/MAX apply to this column.
func AllowsAggregate(k Kind, rel RelationKind) bool {
	if k == Relation {
		return relationCaps[rel].Aggregatable
	}
	spec, ok := byKind[k]
	return ok && spec.Aggregatable
}

var (
	goTime     = reflect.TypeOf(time.Time{})
	goRawJSON  = reflect.TypeOf(json.RawMessage(nil))
	goUUID     = reflect.TypeOf(uuid.UUID{})
	goDecimal  = reflect.TypeOf(decimal.Decimal{})
	goTypesDec = reflect.TypeOf(types.Decimal{})
	goDate     = reflect.TypeOf(types.Date{})
	goJSON     = reflect.TypeOf(types.JSON(nil))
	goNullJSON = reflect.TypeOf(types.NullJSON(nil))
)

// SemanticKind recovers email, url, ip, or slug from model tags. format is
// the format struct tag. tagPattern is the pattern struct tag make resource
// writes for a slug. validatePattern is a user regex and is not a kind: a
// string whose regex happens to be the slug alphabet stays a string.
func SemanticKind(format, tagPattern, _ string) (Kind, bool) {
	switch format {
	case "email":
		return Email, true
	case "uri":
		return URL, true
	case "ip":
		return IP, true
	}
	if tagPattern == `^[-a-zA-Z0-9_]+$` {
		return Slug, true
	}
	return "", false
}

// KindFromGo infers a kind from a Go field type. dataType is the GORM data
// type name (used to tell string columns from text columns). Pointers are
// unwrapped. This is the registration-time inference the admin uses; the
// returned kind's AdminWire is the meta type.
func KindFromGo(t reflect.Type, dataType string) Kind {
	if t == nil {
		return String
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t {
	case goTime:
		return DateTime
	case goDate:
		return Date
	case goRawJSON, goJSON, goNullJSON:
		return JSON
	case goUUID:
		return UUID
	case goDecimal, goTypesDec:
		return Decimal
	}
	switch t.Kind() {
	case reflect.String:
		if strings.Contains(strings.ToLower(dataType), "text") {
			return Text
		}
		return String
	case reflect.Bool:
		return Boolean
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32:
		return Integer
	case reflect.Int64:
		return Integer64
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return Unsigned
	case reflect.Float32, reflect.Float64:
		return Float
	case reflect.Struct, reflect.Map, reflect.Slice, reflect.Array:
		return JSON
	default:
		return String
	}
}
