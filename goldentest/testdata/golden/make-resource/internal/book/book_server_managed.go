package book

// bookServerManagedColumns lists NOT NULL columns the create handler fills
// server-side (e.g. from the auth context or a hook) rather than from the
// request body. Add a column here when you set it in code; the drift test in
// book_drift_test.go then treats it as a known source.
//
// This file is yours to edit — `gombit make resource` writes it once and never
// regenerates it (even with --force).
var bookServerManagedColumns = []string{}
