package resourcegen

import (
	"fmt"
	"strings"
)

// reservedOperationIDs are the Huma operation IDs the framework registers on
// the app's API before any generated resource mounts: auth in both modes, email
// verification, session management and password reset, and the admin data plane
// (cookie mode).
//
// Huma keys operations by ID and panics at boot on a duplicate, so a resource
// that emits one of these produces an app that builds cleanly and then dies on
// start — the collision `make resource` could not see at generation time (#579).
// reserved_test.go mounts the real surfaces on a Huma API and asserts this list
// covers every ID they register, so it cannot fall behind them.
var reservedOperationIDs = map[string]struct{}{
	// Auth, both bearer and cookie modes.
	"register": {}, "login": {}, "refresh": {}, "logout": {}, "get-me": {},
	// Auth, cookie mode only.
	"csrf": {},
	// Auth: email verification (AUTH-1) and session management (AUTH-5).
	"auth-verify-email": {}, "auth-resend-email-verification": {},
	"auth-list-sessions": {}, "auth-revoke-session": {}, "auth-revoke-sessions": {},
	// Auth: password reset (AUTH-2).
	"auth-request-password-reset": {}, "auth-reset-password": {},
	// Admin data plane (cookie mode).
	"admin-meta-list": {}, "admin-meta-get": {},
	"admin-resource-list": {}, "admin-resource-create": {}, "admin-resource-get": {},
	"admin-resource-update": {}, "admin-resource-delete": {}, "admin-resource-upload": {},
}

// IsReservedOperationID reports whether the framework registers id itself, so a
// generated resource must not.
func IsReservedOperationID(id string) bool {
	_, reserved := reservedOperationIDs[id]
	return reserved
}

// The operation IDs a generated resource registers, in one place so the handler
// generator and the collision check cannot drift apart. Their shapes match the
// legacy path: list is plural (list-books), get/create singular (get-book,
// create-book).
func listOperationID(name ResourceName) string   { return "list-" + name.Kebab }
func getOperationID(name ResourceName) string    { return "get-" + name.Package }
func createOperationID(name ResourceName) string { return "create-" + name.Package }

func uploadOperationID(name ResourceName, jsonName string) string {
	return "upload-" + name.Package + "-" + strings.ReplaceAll(jsonName, "_", "-")
}

// resourceOperationIDs returns every Huma operation ID a resource registers for
// the given fields, in mount order.
//
// Both storage kinds count: `file` and `image` columns are equally
// storage-backed, so the handler registers `upload-<package>-<field>` for each
// one the create request accepts (modelhandler.go iterates modelResource's
// File-backed fields).
//
// A file or image field is included even when the create request does not accept
// it: `InRequest` is resolved later in planning, and the framework registers no
// `upload-*` ID today. Over-reporting a future collision is the safer error for a
// check that fails closed.
func resourceOperationIDs(name ResourceName, fields []Field) []string {
	ids := []string{listOperationID(name), getOperationID(name), createOperationID(name)}
	for _, f := range fields {
		if f.Type == FieldFile || f.Type == FieldImage {
			ids = append(ids, uploadOperationID(name, f.JSONName))
		}
	}
	return ids
}

// checkReservedOperationIDs fails when a resource would register an operation ID
// the framework already owns. make resource calls it while planning, before
// anything is written: the collision is otherwise invisible until the generated
// app boots and Huma panics on the duplicate ID.
func checkReservedOperationIDs(name ResourceName, fields []Field) error {
	for _, id := range resourceOperationIDs(name, fields) {
		if IsReservedOperationID(id) {
			return fmt.Errorf("resourcegen: resource name %q generates operation ID %q, which the framework registers itself; choose another name", name.Input, id)
		}
	}
	return nil
}
