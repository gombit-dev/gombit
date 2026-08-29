package resourcegen

import (
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
	handler := collapse(string(mustFormatGo(renderHandler(ctx))))
	for label, src := range map[string]string{"model": model, "handler": handler} {
		if strings.Contains(src, "format error") {
			t.Fatalf("%s did not gofmt-parse:\n%s", label, src)
		}
	}

	// Model imports + column types.
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

	// Handler DTO uses the SAME Go types as the model (no drift), imports too.
	for _, want := range []string{
		`"time"`,
		`"github.com/gombit-dev/gombit/types"`,
		"Price types.Decimal",
		"StartsAt time.Time",
		`enum:"requested,confirmed,active"`,
	} {
		if !strings.Contains(handler, want) {
			t.Fatalf("handler missing %q:\n%s", want, handler)
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
	handler := collapse(string(mustFormatGo(renderHandler(ctx))))
	if strings.Contains(model, "format error") || strings.Contains(handler, "format error") {
		t.Fatalf("did not gofmt-parse:\nmodel:\n%s\nhandler:\n%s", model, handler)
	}

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

	// Thin handler DTO: belongs_to FK present, collection relations absent.
	if !strings.Contains(handler, "EngineID uint") || !strings.Contains(handler, `json:"engine_id"`) {
		t.Fatalf("handler DTO missing engine_id FK:\n%s", handler)
	}
	if strings.Contains(handler, "Warehouses") || strings.Contains(handler, "Parts") {
		t.Fatalf("handler DTO must not carry m2m/has_many fields:\n%s", handler)
	}
}
