package warehouse

import "time"

// Warehouse is a small model used as the target of Widget's belongs_to
// (primary warehouse) and many_to_many (stocked-at warehouses) relations.
type Warehouse struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
