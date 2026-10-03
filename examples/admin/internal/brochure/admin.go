package brochure

import (
	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/framework"
)

// RegisterAdmin registers Brochure on the runtime admin with fields derived
// from the model: pdf is a file field and cover an image field, with their
// upload policies from the storage tags.
func RegisterAdmin(app *framework.App) error {
	return admin.Register(app, Brochure{}, admin.Options{
		Slug:     "brochures",
		Singular: "Brochure",
		Plural:   "Brochures",
		List:     []string{"title", "pdf", "cover"},
		Search:   []string{"title"},
	})
}
