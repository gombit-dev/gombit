package main

import (
	"context"
	"log"
	"os"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/auth/oidc"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/framework"
)

func main() {
	cfg := config.Default()
	cfg.HTTP.Addr = "127.0.0.1:8080"
	cfg.Auth.JWTSecret = "dev-only-example-jwt-secret-not-for-prod"

	db, err := database.Open(config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    "file:auth-example?mode=memory&cache=shared&_fk=1",
	})
	if err != nil {
		log.Fatal(err)
	}

	opts := []framework.Option{framework.WithConfig(cfg), framework.WithDatabase(db)}
	// Optional: sign in with an OpenID Connect provider (docs/auth.md
	// § External identity providers). Register
	// http://127.0.0.1:8080/api/v1/auth/external/oidc/callback with it.
	if issuer := os.Getenv("OIDC_ISSUER"); issuer != "" {
		provider, err := oidc.New(context.Background(), oidc.Config{
			Name:         "oidc",
			Issuer:       issuer,
			ClientID:     os.Getenv("OIDC_CLIENT_ID"),
			ClientSecret: os.Getenv("OIDC_CLIENT_SECRET"),
			RedirectURL:  "http://" + cfg.HTTP.Addr + "/api/v1/auth/external/oidc/callback",
		})
		if err != nil {
			_ = db.Close()
			log.Fatal(err)
		}
		opts = append(opts, framework.WithAuthIdentityProviders(provider))
	}

	app, err := framework.New(opts...)
	if err != nil {
		_ = db.Close()
		log.Fatal(err)
	}
	app.OnStart(func(context.Context) error {
		return auth.Migrate(db.DB)
	})
	app.OnStop(func(context.Context) error {
		return db.Close()
	})

	log.Printf("auth example listening on http://%s (docs http://127.0.0.1:8080/docs)", cfg.HTTP.Addr)
	if err := framework.Run(app); err != nil {
		log.Fatal(err)
	}
}
