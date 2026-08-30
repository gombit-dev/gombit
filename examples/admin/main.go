package main

import (
	"context"
	"errors"
	"log"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/examples/admin/internal/part"
	"github.com/gombit-dev/gombit/examples/admin/internal/warehouse"
	"github.com/gombit-dev/gombit/examples/admin/internal/widget"
	"github.com/gombit-dev/gombit/framework"
)

func main() {
	cfg := config.Default()
	cfg.HTTP.Addr = "127.0.0.1:8082"
	cfg.Auth.JWTSecret = "dev-only-example-jwt-secret-not-for-prod"
	cfg.Auth.Mode = config.AuthModeCookie
	// Secure=false only because this example serves plain HTTP on
	// 127.0.0.1. config.Validate requires CookieSecure=true in production.
	cfg.Auth.CookieSecure = false

	db, err := database.Open(config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    "file:admin-example?mode=memory&cache=shared&_fk=1",
	})
	if err != nil {
		log.Fatal(err)
	}

	app, err := framework.New(framework.WithConfig(cfg), framework.WithDatabase(db))
	if err != nil {
		_ = db.Close()
		log.Fatal(err)
	}
	// Register every model. Order is not significant: the belongs_to /
	// many_to_many pickers resolve against the target slug at request time (the
	// SPA calls the target's list endpoint), not at Register time.
	for _, register := range []func(*framework.App) error{
		warehouse.RegisterAdmin,
		part.RegisterAdmin,
		widget.RegisterAdmin,
	} {
		if err := register(app); err != nil {
			_ = db.Close()
			log.Fatal(err)
		}
	}

	app.OnStart(func(ctx context.Context) error {
		if err := auth.Migrate(db.DB); err != nil {
			return err
		}
		if err := db.AutoMigrate(&warehouse.Warehouse{}, &part.Part{}, &widget.Widget{}); err != nil {
			return err
		}
		svc, err := auth.NewService(db.DB, cfg)
		if err != nil {
			return err
		}
		return seedAdminUsers(ctx, db, svc)
	})
	app.OnStop(func(context.Context) error {
		return db.Close()
	})

	log.Printf("admin example listening on http://%s (admin http://127.0.0.1:8082/admin/ docs http://127.0.0.1:8082/docs)", cfg.HTTP.Addr)
	log.Printf("seeded superuser admin@example.com / correct-horse-battery-staple")
	log.Printf("seeded view-only user viewer@example.com / correct-horse-battery-staple")
	if err := framework.Run(app); err != nil {
		log.Fatal(err)
	}
}

func seedAdminUsers(ctx context.Context, db *database.DB, svc *auth.Service) error {
	if _, err := svc.CreateSuperuser(ctx, "admin@example.com", "correct-horse-battery-staple"); err != nil &&
		!errors.Is(err, auth.ErrEmailTaken) {
		return err
	}

	viewer, err := svc.Register(ctx, "viewer@example.com", "correct-horse-battery-staple")
	if errors.Is(err, auth.ErrEmailTaken) {
		err = db.WithContext(ctx).Where("email = ?", "viewer@example.com").First(&viewer).Error
	}
	if err != nil {
		return err
	}
	permission, err := auth.EnsurePermission(ctx, db.DB, "admin.widgets.view", "View widgets in the admin")
	if err != nil {
		return err
	}
	group, err := auth.EnsureGroup(ctx, db.DB, "viewers")
	if err != nil {
		return err
	}
	if err := auth.GrantPermissionToGroup(ctx, db.DB, &group, &permission); err != nil {
		return err
	}
	return auth.AddUserToGroup(ctx, db.DB, &viewer, &group)
}
