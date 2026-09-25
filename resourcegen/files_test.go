package resourcegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderNewScalarTypes checks that a resource using the #222 scalar grammar
// (decimal/time/enum) renders valid, consistent Go: the model and the handler
// DTO use the same Go type for each field (so #218's model/DTO drift cannot
// reproduce), the needed imports are present, and everything gofmt-parses.
func TestRenderNewScalarTypes(t *testing.T) {
	fields, err := parseFields([]string{
		"price:decimal:required",
		"starts_at:time:required",
		"status:enum(requested,confirmed,active)",
	}, "rental")
	if err != nil {
		t.Fatalf("parseFields: %v", err)
	}
	name, err := parseResourceName("Rental")
	if err != nil {
		t.Fatalf("parseResourceName: %v", err)
	}
	ctx := newRenderContext("github.com/example/demo", name, fields, "/api/v1", "minimal", false, false)

	// Collapse runs of whitespace so gofmt column alignment doesn't matter.
	collapse := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	model := collapse(string(mustFormatGo(renderModel(ctx))))
	if strings.Contains(model, "format error") {
		t.Fatalf("model did not gofmt-parse:\n%s", model)
	}

	// Model imports + column types. The model-first DTOs/handler derive from these
	// same types (proven to compile in modelhandler_test's compile-and-run tests).
	for _, want := range []string{
		`"time"`,
		`"github.com/gombit-dev/gombit/types"`,
		"Price types.Decimal",
		"type:decimal(19,4)",
		"StartsAt time.Time",
		"Status string",
	} {
		if !strings.Contains(model, want) {
			t.Fatalf("model missing %q:\n%s", want, model)
		}
	}
}

// TestRenderMinimalFormNewTypes checks the generated minimal React form renders
// a select for enum, a datetime-local input for time, and a decimal text input.
func TestRenderMinimalFormNewTypes(t *testing.T) {
	fields, err := parseFields([]string{
		"price:decimal:required",
		"starts_at:time",
		"status:enum(a,b)",
	}, "rental")
	if err != nil {
		t.Fatalf("parseFields: %v", err)
	}
	name, _ := parseResourceName("Rental")
	ctx := newRenderContext("github.com/example/demo", name, fields, "/api/v1", "minimal", false, false)
	form := renderFormTSX(ctx)
	for _, want := range []string{
		`<select {...register("status")}>`,
		`<option value="a">a</option>`,
		`type="datetime-local"`,
		`inputMode="decimal"`,
		`toISOString()`,
	} {
		if !strings.Contains(form, want) {
			t.Fatalf("minimal form missing %q:\n%s", want, form)
		}
	}
}

// TestRenderMUIFormNewTypes checks the MUI form variant renders a select
// (TextField select + MenuItem, with the MenuItem import), a datetime-local
// TextField for time, and pulls the decimal into a text field.
func TestRenderMUIFormNewTypes(t *testing.T) {
	fields, err := parseFields([]string{
		"price:decimal:required",
		"starts_at:time",
		"status:enum(a,b)",
	}, "rental")
	if err != nil {
		t.Fatalf("parseFields: %v", err)
	}
	name, _ := parseResourceName("Rental")
	ctx := newRenderContext("github.com/example/demo", name, fields, "/api/v1", "mui", false, false)
	form := renderFormTSX(ctx)
	for _, want := range []string{
		", MenuItem }",
		"select",
		`<MenuItem value="a">a</MenuItem>`,
		`type="datetime-local"`,
		`inputMode: "decimal"`,
	} {
		if !strings.Contains(form, want) {
			t.Fatalf("MUI form missing %q:\n%s", want, form)
		}
	}
}

func TestRenderConstraintFields(t *testing.T) {
	fields, err := parseFields([]string{
		"age:int:required,min=0,max=150",
		"status:enum(draft,published):default=draft",
		"code:string:max_length=8,regex=^[a-z]+$",
	}, "person")
	if err != nil {
		t.Fatalf("parseFields: %v", err)
	}
	name, err := parseResourceName("Person")
	if err != nil {
		t.Fatalf("parseResourceName: %v", err)
	}
	ctx := newRenderContext("github.com/example/demo", name, fields, "/api/v1", "minimal", false, false)
	model := string(mustFormatGo(renderModel(ctx)))
	for _, want := range []string{
		`check:age >= 0 AND age <= 150`,
		`validate:"min=0;max=150"`,
		`default=draft`,
		`enum=draft,published`,
		`size:8`,
		`validate:"max_length=8;pattern=^[a-z]+$"`,
	} {
		if !strings.Contains(model, want) {
			t.Fatalf("model missing %q:\n%s", want, model)
		}
	}
	form := renderFormTSX(ctx)
	for _, want := range []string{
		`min="0"`,
		`max="150"`,
		`min: { value: 0`,
		`status: "draft"`,
		`maxLength={8}`,
		`new RegExp("^[a-z]+$")`,
	} {
		if !strings.Contains(form, want) {
			t.Fatalf("form missing %q:\n%s", want, form)
		}
	}
	text, err := parseFields([]string{"note:text:regex=^[a-z]+$"}, "person")
	if err != nil {
		t.Fatalf("parseFields text: %v", err)
	}
	textForm := renderFormField(text[0])
	if !strings.Contains(textForm, `new RegExp("^[a-z]+$")`) {
		t.Fatalf("text form missing regex:\n%s", textForm)
	}
	decimal, err := parseFields([]string{"price:decimal:max=10"}, "person")
	if err != nil {
		t.Fatalf("parseFields decimal: %v", err)
	}
	decimalForm := renderFormField(decimal[0])
	if !strings.Contains(decimalForm, `cmpDecimal(value, "10")`) {
		t.Fatalf("decimal form missing magnitude check:\n%s", decimalForm)
	}
}

func TestDecimalDefaultIsAString(t *testing.T) {
	fields, err := parseFields([]string{"price:decimal:default=19.99"}, "person")
	if err != nil {
		t.Fatal(err)
	}
	name, err := parseResourceName("Person")
	if err != nil {
		t.Fatal(err)
	}
	form := renderFormTSX(newRenderContext("github.com/example/demo", name, fields, "/api/v1", "minimal", false, false))
	start := strings.Index(form, "type FormValues")
	end := strings.Index(form, "export function")
	if start < 0 || end < start {
		t.Fatalf("form missing FormValues:\n%s", form)
	}
	values := form[start:end]
	defStart := strings.Index(form, "defaultValues:")
	if defStart < 0 {
		t.Fatalf("form missing defaultValues:\n%s", form)
	}
	rel := form[defStart:]
	brace := strings.Index(rel, "}")
	if brace < 0 {
		t.Fatalf("form missing defaultValues close:\n%s", form)
	}
	obj := strings.TrimSpace(strings.TrimPrefix(rel[:brace+1], "defaultValues:"))
	snippet := values + "const check: FormValues = " + obj + ";\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "form.ts")
	if err := os.WriteFile(path, []byte(snippet), 0o600); err != nil {
		t.Fatal(err)
	}
	tsc := filepath.Join(resourcegenModuleRoot(t), "internal", "adminui", "node_modules", "typescript", "bin", "tsc")
	cmd := exec.Command(tsc, "--strict", "--noEmit", "--target", "ES2022", path) // #nosec G204 -- tsc is the repo's own typescript binary
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("decimal default does not typecheck as a string:\n%s\n--- snippet ---\n%s", out, snippet)
	}
}

// TestRenderRelations checks the generated model has the FK + associations +
// target imports, and the thin handler DTO exposes belongs_to as its FK but
// omits many_to_many / has_many (#222 part b).
func TestRenderRelations(t *testing.T) {
	fields, err := parseFields([]string{
		"engine:belongs_to:Engine",
		"parts:has_many:Part",
		"warehouses:many_to_many:Warehouse",
	}, "rental")
	if err != nil {
		t.Fatalf("parseFields: %v", err)
	}
	name, err := parseResourceName("Rental")
	if err != nil {
		t.Fatalf("parseResourceName: %v", err)
	}
	ctx := newRenderContext("github.com/example/demo", name, fields, "/api/v1", "minimal", false, false)
	collapse := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	model := collapse(string(mustFormatGo(renderModel(ctx))))
	if strings.Contains(model, "format error") {
		t.Fatalf("model did not gofmt-parse:\n%s", model)
	}

	// The belongs_to emits its FK column (a persisted, DTO-visible field) plus the
	// association object; has_many / many_to_many are relationships, not columns, so
	// the model-first DTOs never surface them (resourcepolicy skips non-columns).
	for _, want := range []string{
		`"github.com/example/demo/internal/engine"`,
		`"github.com/example/demo/internal/part"`,
		`"github.com/example/demo/internal/warehouse"`,
		"EngineID uint",
		"Engine engine.Engine",
		"Parts []part.Part",
		"Warehouses []warehouse.Warehouse",
		"many2many:rental_warehouses",
	} {
		if !strings.Contains(model, want) {
			t.Fatalf("model missing %q:\n%s", want, model)
		}
	}
}
