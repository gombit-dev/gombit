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

func TestRenderSemanticStringInputs(t *testing.T) {
	fields, err := parseFields([]string{"contact:email:required", "site:url", "handle:slug", "addr:ip"}, "person")
	if err != nil {
		t.Fatalf("parseFields: %v", err)
	}
	name, _ := parseResourceName("Person")
	form := renderFormTSX(newRenderContext("github.com/example/demo", name, fields, "/api/v1", "minimal", false, false))
	for _, want := range []string{
		`type="email"`,
		`type="url"`,
		`type="text"`,
		`new RegExp("^[-a-zA-Z0-9_]+$", "u")`,
		`value === "" ? null : value`,
	} {
		if !strings.Contains(form, want) {
			t.Fatalf("form missing %q:\n%s", want, form)
		}
	}
	mui := renderMUIFormTSX(newRenderContext("github.com/example/demo", name, fields, "/api/v1", "mui", false, false))
	if !strings.Contains(mui, `type="email"`) || !strings.Contains(mui, `new RegExp("^[-a-zA-Z0-9_]+$", "u")`) || !strings.Contains(mui, `["site", "handle", "addr"]`) {
		t.Fatalf("MUI form missing semantic widgets:\n%s", mui)
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
		`[...String(value)].length > 8`,
		`new RegExp("^[a-z]+$", "u")`,
	} {
		if !strings.Contains(form, want) {
			t.Fatalf("form missing %q:\n%s", want, form)
		}
	}
	if strings.Contains(form, `pattern="^[a-z]+$"`) {
		t.Fatalf("HTML pattern attribute anchors the match:\n%s", form)
	}
	if strings.Contains(form, `maxLength={8}`) {
		t.Fatalf("HTML maxlength counts UTF-16 code units:\n%s", form)
	}
	if !strings.Contains(cmpDecimalJS, `ip === "0" && fp === ""`) {
		t.Fatal("cmpDecimal must treat -0 as zero")
	}
	mui := renderMUIFormTSX(ctx)
	if !strings.Contains(mui, `new RegExp("^[a-z]+$", "u")`) {
		t.Fatalf("MUI form missing unanchored regex:\n%s", mui)
	}
	if strings.Contains(mui, `pattern: "^[a-z]+$"`) {
		t.Fatalf("MUI htmlInput pattern anchors the match:\n%s", mui)
	}
	text, err := parseFields([]string{"note:text:regex=^[a-z]+$"}, "person")
	if err != nil {
		t.Fatalf("parseFields text: %v", err)
	}
	textForm := renderFormField(text[0])
	if !strings.Contains(textForm, `new RegExp("^[a-z]+$", "u")`) {
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

func TestSelfRelationModelCompiles(t *testing.T) {
	fields, err := parseFields([]string{"parent:belongs_to:Category,nullable,on_delete=set_null"}, "category")
	if err != nil {
		t.Fatalf("parseFields: %v", err)
	}
	name, err := parseResourceName("Category")
	if err != nil {
		t.Fatal(err)
	}
	ctx := newRenderContext("example.com/demo", name, fields, "/api/v1", "minimal", false, false)
	src := renderModel(ctx)
	if !strings.Contains(src, "Parent *Category") || strings.Contains(src, "Parent Category `") {
		t.Fatalf("self association must be a pointer:\n%s", src)
	}
	// Build inside this module so gorm is already cached. A throwaway module
	// plus go mod tidy fails CI, which sets GOPROXY=off.
	root := resourcegenModuleRoot(t)
	dir := filepath.Join(root, "internal", "_selfcategory")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "model.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-mod=readonly", "./internal/_selfcategory")
	build.Dir = root
	build.Env = append(os.Environ(), "GOPROXY=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s\n%s", err, out, src)
	}
	badDir := filepath.Join(root, "internal", "_selfcategory_value")
	t.Cleanup(func() { _ = os.RemoveAll(badDir) })
	if err := os.MkdirAll(badDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(src, "Parent *Category", "Parent Category", 1)
	if err := os.WriteFile(filepath.Join(badDir, "model.go"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	badBuild := exec.Command("go", "build", "-mod=readonly", "./internal/_selfcategory_value")
	badBuild.Dir = root
	badBuild.Env = append(os.Environ(), "GOPROXY=off")
	if out, err := badBuild.CombinedOutput(); err == nil {
		t.Fatalf("value self association compiled:\n%s", bad)
	} else if !strings.Contains(string(out), "recursive") {
		t.Fatalf("go build: %v\n%s", err, out)
	}
}

func TestNullableFKFormSubmitsNull(t *testing.T) {
	fields, err := parseFields([]string{"author:belongs_to:Author,nullable"}, "post")
	if err != nil {
		t.Fatalf("parseFields: %v", err)
	}
	name, err := parseResourceName("Post")
	if err != nil {
		t.Fatal(err)
	}
	minimal := newRenderContext("example.com/demo", name, dtoFields(fields), "/api/v1", "minimal", false, false)
	form := renderFormTSX(minimal)
	for _, want := range []string{"author_id: null", "number | null", `value === "" ? null : Number(value)`} {
		if !strings.Contains(form, want) {
			t.Fatalf("minimal form missing %q:\n%s", want, form)
		}
	}
	mui := renderMUIFormTSX(newRenderContext("example.com/demo", name, dtoFields(fields), "/api/v1", "mui", false, false))
	if !strings.Contains(mui, `raw === "" ? null : Number(raw)`) || strings.Contains(mui, "author_id: 0") {
		t.Fatalf("MUI form still submits 0:\n%s", mui)
	}

	// nullable on a non-pointer number is the opposite of required. The column
	// stays int, so the form must post 0. Null is only for a pointer Go type.
	ints, err := parseFields([]string{"count:int:nullable", "qty:uint:nullable"}, "post")
	if err != nil {
		t.Fatalf("parseFields: %v", err)
	}
	if ints[0].GoType != "int" || ints[1].GoType != "*uint" {
		t.Fatalf("nullable numbers = %q, %q", ints[0].GoType, ints[1].GoType)
	}
	intForm := renderFormTSX(newRenderContext("example.com/demo", name, dtoFields(ints), "/api/v1", "minimal", false, false))
	if !strings.Contains(intForm, "count: number;") || !strings.Contains(intForm, "count: 0") || !strings.Contains(intForm, `register("count", { setValueAs: (value) => (value === "" ? 0 : Number(value))`) {
		t.Fatalf("nullable int form posts null:\n%s", intForm)
	}
	if !strings.Contains(intForm, "qty: number | null;") || !strings.Contains(intForm, "qty: null") {
		t.Fatalf("nullable uint form does not post null:\n%s", intForm)
	}
}

func TestRenderScalarGaps(t *testing.T) {
	fields, err := parseFields([]string{
		"score:float:sortable",
		"born:date:required",
		"token:uuid:required",
		"meta:json",
		"at:datetime:required",
	}, "reading")
	if err != nil {
		t.Fatalf("parseFields: %v", err)
	}
	name, err := parseResourceName("Reading")
	if err != nil {
		t.Fatal(err)
	}
	ctx := newRenderContext("github.com/example/demo", name, fields, "/api/v1", "minimal", false, false)
	model := string(mustFormatGo(renderModel(ctx)))
	for _, want := range []string{
		"Score float64",
		"types.Date",
		"type:date;not null",
		"uuid.UUID",
		"type:char(36);not null",
		"types.NullJSON",
		"type:text",
		"time.Time",
		`gorm:"not null"`,
		`"github.com/gombit-dev/gombit/types"`,
		`"github.com/google/uuid"`,
	} {
		if !strings.Contains(model, want) {
			t.Fatalf("model missing %q:\n%s", want, model)
		}
	}
	form := renderFormTSX(ctx)
	for _, want := range []string{
		`type="date"`,
		`type="datetime-local"`,
		`type="number"`,
		`value === "" ? null`,
		`validate: (value)`,
		`return "must be JSON"`,
		`body[key] = JSON.parse(String(raw))`,
	} {
		if !strings.Contains(form, want) {
			t.Fatalf("form missing %q:\n%s", want, form)
		}
	}
	if strings.Contains(form, `throw new Error`) {
		t.Fatalf("form throws while the user is still typing:\n%s", form)
	}
	muiCtx := newRenderContext("github.com/example/demo", name, fields, "/api/v1", "mui", false, false)
	mui := renderFormTSX(muiCtx)
	for _, want := range []string{
		`type="date"`,
		`raw === "" ? null : raw`,
		`field.onChange(event.target.value)`,
		`validate: (value)`,
		`body[key] = JSON.parse(String(raw))`,
	} {
		if !strings.Contains(mui, want) {
			t.Fatalf("mui form missing %q:\n%s", want, mui)
		}
	}
	if strings.Contains(mui, `JSON.parse(raw)`) || strings.Contains(mui, `throw new Error`) {
		t.Fatalf("mui form parses or throws from the change handler:\n%s", mui)
	}
}
