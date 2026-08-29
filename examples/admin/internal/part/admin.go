package part

import (
	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/framework"
)

// RegisterAdmin registers Part on the runtime admin. Widget reads Parts as a
// read-only has_many; here the back-reference (widget_id) is a plain integer,
// because Part cannot hold a Widget association without an import cycle.
func RegisterAdmin(app *framework.App) error {
	return admin.Register(app, Part{}, admin.Options{
		Slug:     "parts",
		Singular: "Part",
		Plural:   "Parts",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "name", Type: admin.TypeString, Required: true},
			{Name: "widget_id", Type: admin.TypeInteger},
		},
		List:     []string{"name", "widget_id"},
		Search:   []string{"name"},
		Ordering: []string{"name", "created_at"},
		Actions: admin.Actions{
			List: true, Detail: true, Create: true, Update: true, Delete: true,
		},
	})
}
