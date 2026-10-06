//go:build integration

package auth_test

import (
	"context"
	"flag"
	"testing"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gin-gonic/gin"
)

var (
	postgresDSN = flag.String("auth.postgres-dsn", "", "PostgreSQL DSN for auth integration tests")
	mysqlDSN    = flag.String("auth.mysql-dsn", "", "MySQL DSN for auth integration tests")
)

func TestE2EPostgres(t *testing.T) {
	if *postgresDSN == "" {
		t.Skip("set -auth.postgres-dsn to run Postgres auth integration tests")
	}
	gin.SetMode(gin.TestMode)
	db := openDriver(t, config.DatabaseDriverPostgres, *postgresDSN)
	app := newAuthAppWithDB(t, db)
	runBearerE2E(t, app)
	runBearerSessions(t, app)
	runCookieSessions(t, newCookieAuthAppWithDB(t, db))
	runSessionExpiryGates(t, db)
	runSessionsService(t, db)
	runPermissionPersistence(t, db)
}

func TestE2EMySQL(t *testing.T) {
	if *mysqlDSN == "" {
		t.Skip("set -auth.mysql-dsn to run MySQL auth integration tests")
	}
	gin.SetMode(gin.TestMode)
	db := openDriver(t, config.DatabaseDriverMySQL, *mysqlDSN)
	app := newAuthAppWithDB(t, db)
	runBearerE2E(t, app)
	runBearerSessions(t, app)
	runCookieSessions(t, newCookieAuthAppWithDB(t, db))
	runSessionExpiryGates(t, db)
	runSessionsService(t, db)
	runPermissionPersistence(t, db)
}

func openDriver(t *testing.T, driver config.DatabaseDriver, dsn string) *database.DB {
	t.Helper()
	db, err := database.Open(config.DatabaseConfig{Driver: driver, DSN: dsn})
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() {
		_ = db.Migrator().DropTable("auth_user_permissions", "auth_user_groups", "auth_group_permissions")
		_ = db.Migrator().DropTable(auth.Models()...)
		_ = db.Close()
	})
	return db
}

func runPermissionPersistence(t *testing.T, db *database.DB) {
	t.Helper()
	ctx := context.Background()
	user := auth.User{Email: "integration-authz@example.com", PasswordHash: "unused"}
	if err := db.WithContext(ctx).Create(&user).Error; err != nil {
		t.Fatalf("create permission test user: %v", err)
	}
	permission, err := auth.EnsurePermission(ctx, db.DB, "admin.widgets.view", "View widgets")
	if err != nil {
		t.Fatalf("EnsurePermission: %v", err)
	}
	group, err := auth.EnsureGroup(ctx, db.DB, "integration-viewers")
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if err := auth.GrantPermissionToGroup(ctx, db.DB, &group, &permission); err != nil {
		t.Fatalf("GrantPermissionToGroup: %v", err)
	}
	if err := auth.AddUserToGroup(ctx, db.DB, &user, &group); err != nil {
		t.Fatalf("AddUserToGroup: %v", err)
	}
	granted, err := auth.HasPermission(ctx, db.DB, user, permission.Key)
	if err != nil {
		t.Fatalf("HasPermission: %v", err)
	}
	if !granted {
		t.Fatal("group permission was not persisted")
	}

	recreatedPermission, err := auth.EnsurePermission(
		ctx,
		db.DB,
		"admin.widgets.recreate",
		"Recreate widgets",
	)
	if err != nil {
		t.Fatalf("EnsurePermission(recreate initial): %v", err)
	}
	recreatedGroup, err := auth.EnsureGroup(ctx, db.DB, "integration-recreated-viewers")
	if err != nil {
		t.Fatalf("EnsureGroup(recreate initial): %v", err)
	}
	if err := db.WithContext(ctx).Delete(&recreatedPermission).Error; err != nil {
		t.Fatalf("delete permission for recreation: %v", err)
	}
	if err := db.WithContext(ctx).Delete(&recreatedGroup).Error; err != nil {
		t.Fatalf("delete group for recreation: %v", err)
	}
	recreatedPermission, err = auth.EnsurePermission(
		ctx,
		db.DB,
		recreatedPermission.Key,
		recreatedPermission.Description,
	)
	if err != nil {
		t.Fatalf("EnsurePermission(recreate): %v", err)
	}
	recreatedGroup, err = auth.EnsureGroup(ctx, db.DB, recreatedGroup.Name)
	if err != nil {
		t.Fatalf("EnsureGroup(recreate): %v", err)
	}
	if recreatedPermission.ID == 0 || recreatedGroup.ID == 0 {
		t.Fatalf(
			"recreated identities must be persisted: permission ID = %d, group ID = %d",
			recreatedPermission.ID,
			recreatedGroup.ID,
		)
	}
	if err := auth.GrantPermissionToGroup(
		ctx,
		db.DB,
		&recreatedGroup,
		&recreatedPermission,
	); err != nil {
		t.Fatalf("GrantPermissionToGroup(recreated): %v", err)
	}
	if err := auth.AddUserToGroup(ctx, db.DB, &user, &recreatedGroup); err != nil {
		t.Fatalf("AddUserToGroup(recreated): %v", err)
	}
	granted, err = auth.HasPermission(ctx, db.DB, user, recreatedPermission.Key)
	if err != nil {
		t.Fatalf("HasPermission(recreated): %v", err)
	}
	if !granted {
		t.Fatal("recreated group permission was not persisted")
	}
}
