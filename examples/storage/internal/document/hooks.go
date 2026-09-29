package document

import "context"

// Hooks implements DocumentHooks. This file is generated once and
// is yours to edit: set server-managed columns (tenant, owner, timestamps not
// handled by GORM, …) on row in BeforeCreate. Regeneration does not overwrite it.
type Hooks struct{}

// BeforeCreate runs after the request is mapped onto row and before it is
// persisted. The default is a no-op; add server-derived values here.
func (Hooks) BeforeCreate(ctx context.Context, row *Document, body documentCreateBody) error {
	return nil
}

// BeforeUpload runs before an upload grant for a file field (its JSON
// name). Decide here who may upload; an error refuses the grant. The
// default lets anyone who may call the endpoint, as create does.
func (Hooks) BeforeUpload(ctx context.Context, field string) error {
	return nil
}
