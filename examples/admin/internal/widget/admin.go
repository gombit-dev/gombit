package widget

import (
	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/framework"
)

// RegisterAdmin registers Widget on the runtime admin. Feature packages own
// this call — the framework never discovers GORM models by itself (ADR-013).
//
// Fields is left empty so admin.FieldsFrom derives the schema from the GORM
// model, including the three relations (#223): warehouse_id auto-derives to a
// belongs_to picker, warehouses to a many_to_many multi-select, and parts to a
// read-only has_many view — no per-field wiring, no per-model React file.
func RegisterAdmin(app *framework.App) error {
	return admin.Register(app, Widget{}, admin.Options{
		Slug:     "widgets",
		Singular: "Widget",
		Plural:   "Widgets",
		List:     []string{"name", "sku", "warehouse_id"},
		Search:   []string{"name", "sku"},
		Ordering: []string{"name", "created_at"},
		Actions: admin.Actions{
			List: true, Detail: true, Create: true, Update: true, Delete: true,
		},
	})
}
