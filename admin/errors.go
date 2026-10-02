package admin

import (
	"fmt"
)

func errMissingSlug() error {
	return fmt.Errorf("admin: missing slug")
}

func errDuplicateSlug(slug string) error {
	return fmt.Errorf("admin: duplicate slug %q", slug)
}

func errInvalidSlug(slug string) error {
	return fmt.Errorf("admin: invalid slug %q", slug)
}

// errCompositePrimaryKey mirrors the wording resourcegen already uses for the
// same model shape, so a model that is rejected at generation time and again at
// registration time reports the same reason.
func errCompositePrimaryKey(typeName string) error {
	return fmt.Errorf("admin: %s has a composite primary key, which is not supported", typeName)
}
