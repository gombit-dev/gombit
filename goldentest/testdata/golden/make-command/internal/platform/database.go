package platform

import (
	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/storage/claims"

	"github.com/example/demo/internal/product"
)

// OpenDatabase opens the SQL database from typed config.
func OpenDatabase(cfg config.DatabaseConfig) (*database.DB, error) {
	return database.Open(cfg)
}

// AutoMigrate runs GORM AutoMigrate for the framework's tables (auth, and
// the storage claims that file uploads are owned through) and
// feature-package models so the example API can serve before Atlas
// migrations. Framework models must stay in this call: gombit make resource
// collects every AutoMigrate argument as the entire desired Atlas schema.
// gombit db makemigrations builds its desired schema from the persisted
// migration registry instead, and refuses --forget-model for a model still
// listed here, so the two sources never disagree.
func AutoMigrate(db *database.DB) error {
	return db.AutoMigrate(
		&auth.User{},
		&auth.RefreshToken{},
		&auth.Group{},
		&auth.Permission{},
		&claims.Claim{},
		&product.Product{},
	)
}
