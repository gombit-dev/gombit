package cli

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"strings"
	"testing"

	logical "github.com/gombit-dev/gombit/field"
	"github.com/gombit-dev/gombit/resourcegen"
)

func makeResourceHelp(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer
	if err := ExecuteRoot(context.Background(), NewRoot(&out, io.Discard), []string{"make", "resource", "--help"}); err != nil {
		t.Fatalf("make resource --help: %v", err)
	}
	return out.String()
}

// TestMakeResourceHelpListsEveryFieldType guards the type list against the
// field vocabulary: every CLI token (aliases included) of every kind the
// generator emits, and every relation cardinality, must be in the help.
func TestMakeResourceHelpListsEveryFieldType(t *testing.T) {
	help := makeResourceHelp(t)
	var tokens []string
	for _, kind := range logical.Kinds() {
		spec, ok := logical.Lookup(kind)
		if !ok || !spec.GeneratorReady {
			continue
		}
		tokens = append(tokens, spec.CLITokens...)
	}
	if len(tokens) == 0 {
		t.Fatal("field vocabulary has no generator-ready tokens")
	}
	for _, tok := range tokens {
		if !regexp.MustCompile(`(^|[\s,/])` + regexp.QuoteMeta(tok) + `($|[\s,/])`).MatchString(help) {
			t.Errorf("make resource --help does not list type %q", tok)
		}
	}
}

// TestMakeResourceHelpCapabilityTypes checks each list-query modifier's
// "Types:" line in the help is the vocabulary's list for that capability,
// placed under that modifier, and that the list is exactly the scalar
// kinds the vocabulary allows it on.
func TestMakeResourceHelpCapabilityTypes(t *testing.T) {
	help := makeResourceHelp(t)
	cases := []struct {
		modifier string
		allows   func(logical.Kind, logical.RelationKind) bool
	}{
		{"filterable", logical.AllowsFilter},
		{"sortable", logical.AllowsSort},
		{"searchable", logical.AllowsSearch},
		{"aggregatable", logical.AllowsAggregate},
	}
	for _, tc := range cases {
		line := resourceCapabilityTypes(tc.allows)
		modAt := strings.Index(help, "\n  "+tc.modifier+" ")
		lineAt := strings.Index(help, line)
		if modAt < 0 || lineAt < modAt {
			t.Errorf("help has no %s Types line under the modifier:\n%s", tc.modifier, line)
			continue
		}
		list := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "Types:"), ".")
		listed := strings.FieldsFunc(list, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\n'
		})
		var want []string
		for _, kind := range logical.Kinds() {
			spec, _ := logical.Lookup(kind)
			if spec.GeneratorReady && kind != logical.Relation && tc.allows(kind, "") {
				want = append(want, spec.CLITokens[0])
			}
		}
		if strings.Join(listed, ",") != strings.Join(want, ",") {
			t.Errorf("%s Types = %v, want %v", tc.modifier, listed, want)
		}
	}
}

// TestMakeResourceHelpDescribesModelFirstLayout pins the ADR-016 layout and
// where the AST wiring actually lands, and rejects the pre-model-first text.
func TestMakeResourceHelpDescribesModelFirstLayout(t *testing.T) {
	help := makeResourceHelp(t)
	for _, want := range []string{
		"dto.gen.go", "handler.gen.go", "hooks.go", ".gombit-resource",
		"cmd/server/main.go", resourcegen.PlatformDBRel(),
	} {
		if !strings.Contains(help, want) {
			t.Errorf("make resource --help does not mention %q", want)
		}
	}
	for _, stale := range []string{
		"AutoMigrate is updated the same way",
		"Supported types: string, text, int, int64, bool, uint, decimal, time,",
	} {
		if strings.Contains(help, stale) {
			t.Errorf("make resource --help still says %q", stale)
		}
	}
}

func TestWrapHelpList(t *testing.T) {
	got := wrapHelpList("Types: ", "  ", []string{strings.Repeat("a", 40), strings.Repeat("b", 40), "c"})
	want := "Types: " + strings.Repeat("a", 40) + ",\n  " + strings.Repeat("b", 40) + ", c"
	if got != want {
		t.Fatalf("wrapHelpList =\n%s\nwant\n%s", got, want)
	}
	for _, line := range strings.Split(got, "\n") {
		if len(line) > helpWidth {
			t.Errorf("line longer than %d: %q", helpWidth, line)
		}
	}
}
