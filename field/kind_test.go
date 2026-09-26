package field

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/gombit-dev/gombit/types"
)

func TestCatalogCoversEveryKindOnce(t *testing.T) {
	t.Parallel()
	seen := map[Kind]int{}
	for _, spec := range catalog {
		seen[spec.Kind]++
	}
	for _, k := range []Kind{
		String, Text, Integer, Integer64, Unsigned, Float, Decimal, Boolean,
		Date, DateTime, TimeOfDay, Duration, UUID, JSON, Email, URL, Slug, IP,
		Enum, Relation,
	} {
		if seen[k] != 1 {
			t.Errorf("kind %q appears %d times in the catalog, want 1", k, seen[k])
		}
	}
	if len(seen) != len(catalog) {
		t.Fatalf("catalog has %d entries, %d distinct kinds", len(catalog), len(seen))
	}
}

func TestCLIAliasesShareAKind(t *testing.T) {
	t.Parallel()
	pairs := [][2]string{
		{"int", "integer"},
		{"int64", "integer64"},
		{"uint", "unsigned"},
		{"bool", "boolean"},
		{"time", "datetime"},
	}
	for _, pair := range pairs {
		k1, r1, ok1 := ParseCLI(pair[0])
		k2, r2, ok2 := ParseCLI(pair[1])
		if !ok1 || !ok2 {
			t.Fatalf("ParseCLI(%q, %q) = ok %v %v", pair[0], pair[1], ok1, ok2)
		}
		if k1 != k2 || r1 != r2 {
			t.Fatalf("tokens %q and %q parsed as %s/%s and %s/%s", pair[0], pair[1], k1, r1, k2, r2)
		}
		spec, ok := Lookup(k1)
		if !ok || !spec.GeneratorReady {
			t.Fatalf("alias pair %q is not generator-ready", pair[0])
		}
	}
}

func TestTimeTokenIsDateTime(t *testing.T) {
	t.Parallel()
	k, rel, ok := ParseCLI("time")
	if !ok || rel != "" || k != DateTime {
		t.Fatalf("ParseCLI(time) = %s %s %v, want datetime", k, rel, ok)
	}
	if _, _, ok := ParseCLI("blob"); ok {
		t.Fatal("ParseCLI(blob) succeeded")
	}
	k, _, ok = ParseCLI("float")
	k64, _, ok64 := ParseCLI("float64")
	if !ok || !ok64 || k != Float || k64 != Float {
		t.Fatalf("ParseCLI float/float64 = %s %v / %s %v", k, ok, k64, ok64)
	}
	spec, _ := Lookup(Float)
	if !spec.GeneratorReady || spec.GoType != "float64" {
		t.Fatalf("float spec = %+v", spec)
	}
	for _, kind := range []Kind{Email, URL, Slug, IP} {
		spec, ok := Lookup(kind)
		if !ok || !spec.GeneratorReady || spec.GoType != "string" || spec.AdminWire != "string" {
			t.Fatalf("%s spec = %+v", kind, spec)
		}
	}
}

func TestRelationTokens(t *testing.T) {
	t.Parallel()
	for _, tok := range []RelationKind{RelBelongsTo, RelHasMany, RelManyToMany, RelOneToOne} {
		k, rel, ok := ParseCLI(string(tok))
		if !ok || k != Relation || rel != tok {
			t.Fatalf("ParseCLI(%s) = %s %s %v", tok, k, rel, ok)
		}
	}
	spec, ok := Lookup(Relation)
	if !ok || !spec.GeneratorReady {
		t.Fatal("relation is generated today; GeneratorReady must be set")
	}
	if AllowsFilter(Relation, RelBelongsTo) != true {
		t.Fatal("belongs_to should be filterable")
	}
	if AllowsSort(Relation, RelHasMany) || AllowsSort(Relation, RelManyToMany) {
		t.Fatal("collection relations should not be sortable")
	}
	if !AllowsAggregate(Integer, "") || AllowsAggregate(Boolean, "") {
		t.Fatal("aggregate capability drifted")
	}
}

func TestAdminWiresAreTheHistoricalSet(t *testing.T) {
	t.Parallel()
	got := map[string]struct{}{}
	for _, w := range AdminWires() {
		if !IsAdminWire(w) {
			t.Fatalf("AdminWires returned %q, IsAdminWire false", w)
		}
		got[w] = struct{}{}
	}
	want := []string{"string", "text", "integer", "float", "decimal", "boolean", "datetime", "date", "time", "duration", "uuid", "json", "relation"}
	if len(got) != len(want) {
		t.Fatalf("admin wires = %v, want %v", AdminWires(), want)
	}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("missing admin wire %q", w)
		}
	}
	if Integer64.AdminWire() != "integer" || Unsigned.AdminWire() != "integer" {
		t.Fatal("finer integer kinds must keep the integer admin widget")
	}
	if DateTime.AdminWire() != "datetime" {
		t.Fatal("datetime admin wire drifted")
	}
	for _, kind := range []Kind{TimeOfDay, Duration} {
		spec, ok := Lookup(kind)
		if !ok || !spec.GeneratorReady || spec.GoType == "" || spec.AdminWire == "" {
			t.Fatalf("%s spec = %+v", kind, spec)
		}
	}
	if k, _, ok := ParseCLI("time_of_day"); !ok || k != TimeOfDay {
		t.Fatalf("ParseCLI(time_of_day) = %s %v", k, ok)
	}
	if k, _, ok := ParseCLI("duration"); !ok || k != Duration {
		t.Fatalf("ParseCLI(duration) = %s %v", k, ok)
	}
}

func TestKindFromGoMatchesAdminInference(t *testing.T) {
	t.Parallel()
	cases := []struct {
		typ      reflect.Type
		dataType string
		wire     string
	}{
		{reflect.TypeOf(""), "", "string"},
		{reflect.TypeOf(""), "text", "text"},
		{reflect.TypeOf(true), "", "boolean"},
		{reflect.TypeOf(0), "", "integer"},
		{reflect.TypeOf(int64(0)), "", "integer"},
		{reflect.TypeOf(uint(0)), "", "integer"},
		{reflect.TypeOf(float64(0)), "", "float"},
		{reflect.TypeOf(time.Time{}), "", "datetime"},
		{reflect.TypeOf((*time.Time)(nil)), "", "datetime"},
		{reflect.TypeOf(uuid.UUID{}), "", "uuid"},
		{reflect.TypeOf(decimal.Decimal{}), "", "decimal"},
		{reflect.TypeOf(types.Decimal{}), "", "decimal"},
		{reflect.TypeOf(types.Date{}), "", "date"},
		{reflect.TypeOf(types.TimeOfDay{}), "", "time"},
		{reflect.TypeOf(types.Duration{}), "", "duration"},
		{reflect.TypeOf(types.JSON(nil)), "", "json"},
		{reflect.TypeOf(types.NullJSON(nil)), "", "json"},
	}
	for _, tc := range cases {
		k := KindFromGo(tc.typ, tc.dataType)
		if k.AdminWire() != tc.wire {
			t.Errorf("KindFromGo(%s, %q) = %s wire %q, want %q", tc.typ, tc.dataType, k, k.AdminWire(), tc.wire)
		}
	}
}

func TestCLITokensAreUnique(t *testing.T) {
	t.Parallel()
	if err := validateVocabulary(); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, spec := range catalog {
		n += len(spec.CLITokens)
	}
	if len(byCLI) != n {
		t.Fatalf("byCLI has %d tokens, catalog declares %d", len(byCLI), n)
	}
	for rel := range relationCaps {
		p, ok := byCLI[string(rel)]
		if !ok || p.kind != Relation || p.rel != rel {
			t.Fatalf("relationCaps %q registered as %+v", rel, p)
		}
	}
}

func TestPreferredGeneratorTokensParse(t *testing.T) {
	t.Parallel()
	tokens := PreferredGeneratorTokens()
	if len(tokens) == 0 {
		t.Fatal("no generator tokens")
	}
	for _, tok := range tokens {
		k, rel, ok := ParseCLI(tok)
		if !ok || rel != "" {
			t.Fatalf("preferred token %q parsed as rel %q ok %v", tok, rel, ok)
		}
		spec, ok := Lookup(k)
		if !ok || !spec.GeneratorReady || spec.GoType == "" {
			t.Fatalf("preferred token %q kind %s is not an emitted scalar", tok, k)
		}
	}
}
