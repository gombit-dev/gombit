package part

import "time"

// Part is a child of Widget: it carries the parent foreign key (WidgetID) as a
// plain column, which is what a has_many relation reads from. The parent
// (Widget) is not imported here — that would be an import cycle.
type Part struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Name      string    `json:"name"`
	WidgetID  uint      `gorm:"index" json:"widget_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
