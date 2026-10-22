package resourcegen_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/framework"
	"github.com/gombit-dev/gombit/resourcegen"
	"github.com/gombit-dev/gombit/scaffold"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
)

// TestReservesEveryFrameworkOperationID keeps reservedOperationIDs in step with
// what the framework mounts: auth in both modes (email verification and session
// management included) and, in cookie mode, the admin data plane. Huma panics at
// boot on a duplicate operation ID, so every ID the framework registers has to be
// one `make resource` refuses to generate (#579).
func TestReservesEveryFrameworkOperationID(t *testing.T) {
	for _, mode := range []config.AuthMode{config.AuthModeJWT, config.AuthModeCookie} {
		t.Run(string(mode), func(t *testing.T) {
			api := newMountedAPI(t, mode)
			ids := operationIDs(api)
			if len(ids) == 0 {
				t.Fatal("the mounted app registered no operations")
			}
			for _, id := range ids {
				if !resourcegen.IsReservedOperationID(id) {
					t.Errorf("the framework registers operation %q but resourcegen does not reserve it; add it to reservedOperationIDs in resourcegen/reserved.go", id)
				}
			}
		})
	}
}

// TestPlanRejectsReservedOperationID pins the check to the planning phase: a name
// that would mount `get-me` on top of auth's is refused before the feature tree,
// the registry, or the AutoMigrate wiring is touched, instead of producing an app
// that builds and then panics at boot.
func TestPlanRejectsReservedOperationID(t *testing.T) {
	root := t.TempDir()
	if err := scaffold.Generate(context.Background(), scaffold.Options{
		Name:     "demo",
		Database: "sqlite",
		WorkDir:  root,
		Stdout:   io.Discard,
	}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	appDir := filepath.Join(root, "demo")

	_, err := resourcegen.Plan(context.Background(), resourcegen.Options{
		WorkDir:        appDir,
		Name:           "Me",
		Fields:         []string{"title:string:required"},
		SkipMigrations: true,
		Stdout:         io.Discard,
	})
	if err == nil {
		t.Fatal("Plan(Me) error = nil, want an operation-ID collision error")
	}
	if !strings.Contains(err.Error(), `"get-me"`) {
		t.Fatalf("Plan(Me) error = %q, want it to name the colliding operation ID \"get-me\"", err)
	}
	// Nothing was written: the refused plan leaves no feature package behind.
	if _, statErr := os.Stat(filepath.Join(appDir, "internal", "me")); !os.IsNotExist(statErr) {
		t.Fatalf("internal/me must not exist after a refused plan; stat err = %v", statErr)
	}
}

// TestPlanStillScaffoldsNonCollidingNames is the control: an ordinary name maps
// to `get-<pkg>`/`create-<pkg>` IDs the framework does not register, so the new
// check must not turn `make resource` into a refusal for every name.
func TestPlanStillScaffoldsNonCollidingNames(t *testing.T) {
	root := t.TempDir()
	if err := scaffold.Generate(context.Background(), scaffold.Options{
		Name:     "demo",
		Database: "sqlite",
		WorkDir:  root,
		Stdout:   io.Discard,
	}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	appDir := filepath.Join(root, "demo")

	if _, err := resourcegen.Plan(context.Background(), resourcegen.Options{
		WorkDir:        appDir,
		Name:           "Book",
		Fields:         []string{"title:string:required"},
		SkipMigrations: true,
		DryRun:         true,
		Stdout:         io.Discard,
	}); err != nil {
		t.Fatalf("Plan(Book) error = %v, want nil", err)
	}
}

// newMountedAPI builds a real app in the given auth mode so the check reads the
// operation IDs the framework actually registers, not a second hand-written list.
func newMountedAPI(t *testing.T, mode config.AuthMode) huma.API {
	t.Helper()
	db, err := database.Open(config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    "file:" + filepath.Join(t.TempDir(), "operations.db") + "?cache=shared&_fk=1",
	})
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatalf("auth.Migrate: %v", err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Auth.JWTSecret = "test-jwt-secret-32-bytes-minimum!"
	cfg.Auth.BcryptCost = bcrypt.MinCost
	cfg.Auth.Mode = mode
	app, err := framework.New(
		framework.WithConfig(cfg),
		framework.WithDatabase(db),
		framework.WithLogger(zap.NewNop()),
	)
	if err != nil {
		t.Fatalf("framework.New: %v", err)
	}
	return app.API()
}

// operationIDs lists every operation ID registered on api, sorted so a failure
// names one deterministically.
func operationIDs(api huma.API) []string {
	var ids []string
	for _, item := range api.OpenAPI().Paths {
		for _, op := range []*huma.Operation{item.Get, item.Put, item.Post, item.Delete, item.Options, item.Head, item.Patch, item.Trace} {
			if op != nil && op.OperationID != "" {
				ids = append(ids, op.OperationID)
			}
		}
	}
	sort.Strings(ids)
	return ids
}
