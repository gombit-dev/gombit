package widget

import (
	"time"

	"github.com/gombit-dev/gombit/examples/admin/internal/part"
	"github.com/gombit-dev/gombit/examples/admin/internal/warehouse"
)

// Widget is the model registered with the admin in this example. It carries all
// three relation kinds so the admin renders their widgets from the registry
// alone (ADR-013 / #223): a belongs_to picker, a many_to_many multi-select, and
// a read-only has_many view.
type Widget struct {
	ID   uint   `gorm:"primaryKey" json:"id"`
	Name string `json:"name"`
	SKU  string `json:"sku"`

	// belongs_to: the widget's primary warehouse (FK + association). The FK is a
	// *uint so it is genuinely optional: a widget with no warehouse stores NULL
	// rather than 0, which a non-nullable uint could not represent under foreign
	// key enforcement (the admin advertises belongs_to as optional, so the create
	// must actually accept "none").
	WarehouseID *uint               `gorm:"index" json:"warehouse_id"`
	Warehouse   warehouse.Warehouse `json:"-"`

	// many_to_many: every warehouse this widget is stocked at (join table). The
	// JSON name is the admin field name the multi-select binds to.
	Warehouses []warehouse.Warehouse `gorm:"many2many:widget_warehouses;" json:"warehouses"`

	// has_many: the widget's parts (read-only in the admin; Part carries the FK).
	Parts []part.Part `json:"parts"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
