package task

import "context"

// Hooks implements TaskHooks. This file is generated once and
// is yours to edit: set server-managed columns (tenant, owner, timestamps not
// handled by GORM, …) on row in BeforeCreate. Regeneration does not overwrite it.
type Hooks struct{}

// BeforeCreate runs after the request is mapped onto row and before it is
// persisted. The default is a no-op; add server-derived values here.
func (Hooks) BeforeCreate(ctx context.Context, row *Task, body taskCreateBody) error {
	return nil
}
