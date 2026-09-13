package book

// These lists are the human-owned opt-outs the Book contract drift test
// (book_drift_test.go) consumes. Columns hold DB names (e.g. "tenant_id").
//
// This file is yours to edit — `gombit make resource` writes it once and never
// regenerates it (even with --force).

// bookServerManagedColumns: NOT NULL columns the create handler fills server-side
// (from the auth context or a hook) rather than from the request body. The
// drift test then treats them as a known create-value source.
var bookServerManagedColumns = []string{}

// bookWriteOmittedColumns: content columns intentionally NOT settable through the
// create request body (the drift test will not flag them as input drift).
var bookWriteOmittedColumns = []string{}

// bookReadOmittedColumns: content columns intentionally NOT surfaced in responses
// (the drift test will not flag them as response drift).
var bookReadOmittedColumns = []string{}
