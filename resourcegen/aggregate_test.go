package resourcegen

import (
	"strings"
	"testing"
)

func TestParseAggregatableModifier(t *testing.T) {
	t.Parallel()
	fields, err := parseFields([]string{
		"total:decimal:aggregatable",
		"qty:int:aggregatable,filterable",
		"name:string:searchable",
	}, "invoice")
	if err != nil {
		t.Fatalf("parseFields() error = %v", err)
	}
	total, qty, name := fields[0], fields[1], fields[2]
	if !total.Aggregatable || total.Filterable || total.Sortable {
		t.Fatalf("total flags = %+v, want aggregatable only", total)
	}
	if !qty.Aggregatable || !qty.Filterable {
		t.Fatalf("qty flags = %+v, want aggregatable+filterable", qty)
	}
	if name.Aggregatable {
		t.Fatalf("name flags = %+v, want not aggregatable", name)
	}
}

func TestAggregatableTypeErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		spec string
	}{
		{"string", "name:string:aggregatable"},
		{"text", "body:text:aggregatable"},
		{"bool", "paid:bool:aggregatable"},
		{"time", "starts_at:time:aggregatable"},
		{"enum", "status:enum(a,b):aggregatable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseFields([]string{tt.spec}, "widget")
			if err == nil || !strings.Contains(err.Error(), "cannot be aggregatable") {
				t.Fatalf("parseFields(%q) error = %v, want 'cannot be aggregatable'", tt.spec, err)
			}
		})
	}
}

func TestAggregatableNumericTypesAllowed(t *testing.T) {
	t.Parallel()
	for _, spec := range []string{
		"a:int:aggregatable",
		"b:int64:aggregatable",
		"c:uint:aggregatable",
		"d:decimal:aggregatable",
	} {
		if _, err := parseFields([]string{spec}, "widget"); err != nil {
			t.Fatalf("parseFields(%q) error = %v, want nil", spec, err)
		}
	}
}

func TestRenderHandlerAggregates(t *testing.T) {
	t.Parallel()
	fields, err := parseFields([]string{
		"total:decimal:aggregatable",
		"qty:int:aggregatable,sortable",
		"customer:belongs_to:Customer",
	}, "invoice")
	if err != nil {
		t.Fatalf("parseFields() error = %v", err)
	}
	name, err := parseResourceName("Invoice")
	if err != nil {
		t.Fatalf("parseResourceName() error = %v", err)
	}
	src := string(mustFormatGo(renderHandler(newRenderContext("example.com/demo", name, fields, "/api/v1", "minimal", false, false))))
	if strings.Contains(src, "format error") {
		t.Fatalf("generated handler does not format:\n%s", src)
	}
	wantContains := []string{
		`contract.DataMeta[[]invoiceData, contract.ListMeta]`,
		`query:"aggregate"`,
		`database.ParseAggregates(ctx, input.Aggregate, map[string]database.AggregateColumn{`,
		`{Column: "total"}`,
		`{Column: "qty"}`,
		`database.Aggregate(ctx, q.Session(&gorm.Session{}), aggs)`,
		`Aggregates: aggregates`,
	}
	for _, want := range wantContains {
		if !strings.Contains(src, want) {
			t.Fatalf("generated handler missing %q\n%s", want, src)
		}
	}
	// The belongs_to FK is not aggregatable — it must not appear in the map.
	if strings.Contains(src, `"customer_id": {Column:`) {
		t.Fatalf("belongs_to FK must not be aggregatable:\n%s", src)
	}
}
