package warehouse

import (
	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/framework"
)

// RegisterAdmin registers Warehouse on the runtime admin. It is the target of
// Widget's belongs_to and many_to_many relations, so its slug ("warehouses")
// must match the relation metadata the pickers resolve against.
func RegisterAdmin(app *framework.App) error {
	return admin.Register(app, Warehouse{}, admin.Options{
		Slug:     "warehouses",
		Singular: "Warehouse",
		Plural:   "Warehouses",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "name", Type: admin.TypeString, Required: true},
		},
		List:     []string{"name"},
		Search:   []string{"name"},
		Ordering: []string{"name", "created_at"},
		Actions: admin.Actions{
			List: true, Detail: true, Create: true, Update: true, Delete: true,
		},
	})
}
