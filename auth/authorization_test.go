package auth_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/gombit-dev/gombit/auth"
	"gorm.io/gorm/schema"
)

func TestHasPermission(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatalf("auth.Migrate() error = %v", err)
	}

	superuser := auth.User{
		Email:        "superuser@example.com",
		PasswordHash: "unused",
		IsSuperuser:  true,
	}
	groupUser := auth.User{Email: "group@example.com", PasswordHash: "unused"}
	directUser := auth.User{Email: "direct@example.com", PasswordHash: "unused"}
	plainUser := auth.User{Email: "plain@example.com", PasswordHash: "unused"}
	for _, user := range []*auth.User{&superuser, &groupUser, &directUser, &plainUser} {
		if err := db.WithContext(ctx).Create(user).Error; err != nil {
			t.Fatalf("create user %s: %v", user.Email, err)
		}
	}

	groupPermission, err := auth.EnsurePermission(ctx, db.DB, "admin.widgets.view", "View widgets")
	if err != nil {
		t.Fatalf("EnsurePermission(group): %v", err)
	}
	directPermission, err := auth.EnsurePermission(ctx, db.DB, "admin.widgets.create", "Create widgets")
	if err != nil {
		t.Fatalf("EnsurePermission(direct): %v", err)
	}
	viewers, err := auth.EnsureGroup(ctx, db.DB, "viewers")
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if err := auth.GrantPermissionToGroup(ctx, db.DB, &viewers, &groupPermission); err != nil {
		t.Fatalf("GrantPermissionToGroup: %v", err)
	}
	if err := auth.AddUserToGroup(ctx, db.DB, &groupUser, &viewers); err != nil {
		t.Fatalf("AddUserToGroup: %v", err)
	}
	if err := auth.GrantPermissionToUser(ctx, db.DB, &directUser, &directPermission); err != nil {
		t.Fatalf("GrantPermissionToUser: %v", err)
	}

	tests := []struct {
		name string
		user auth.User
		key  string
		want bool
	}{
		{name: "superuser bypass", user: superuser, key: "anything.at.all", want: true},
		{name: "group permission", user: groupUser, key: groupPermission.Key, want: true},
		{name: "direct permission", user: directUser, key: directPermission.Key, want: true},
		{name: "no permission", user: plainUser, key: groupPermission.Key, want: false},
		{name: "missing user", user: auth.User{ID: plainUser.ID + 10_000}, key: groupPermission.Key, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, checkErr := auth.HasPermission(ctx, db.DB, tt.user, tt.key)
			if checkErr != nil {
				t.Fatalf("HasPermission() error = %v", checkErr)
			}
			if got != tt.want {
				t.Fatalf("HasPermission(%q) = %t, want %t", tt.key, got, tt.want)
			}
		})
	}
}

func TestPermissionAssociationsPersist(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatalf("auth.Migrate() error = %v", err)
	}

	user := auth.User{Email: "persisted@example.com", PasswordHash: "unused"}
	if err := db.WithContext(ctx).Create(&user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	permission, err := auth.EnsurePermission(ctx, db.DB, "admin.widgets.view", "View widgets")
	if err != nil {
		t.Fatalf("EnsurePermission: %v", err)
	}
	group, err := auth.EnsureGroup(ctx, db.DB, "persisted-viewers")
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if err := auth.GrantPermissionToGroup(ctx, db.DB, &group, &permission); err != nil {
		t.Fatalf("GrantPermissionToGroup: %v", err)
	}
	if err := auth.AddUserToGroup(ctx, db.DB, &user, &group); err != nil {
		t.Fatalf("AddUserToGroup: %v", err)
	}

	var loaded auth.User
	if err := db.WithContext(ctx).
		Preload("Groups.Permissions").
		First(&loaded, user.ID).Error; err != nil {
		t.Fatalf("reload user associations: %v", err)
	}
	if len(loaded.Groups) != 1 || len(loaded.Groups[0].Permissions) != 1 {
		t.Fatalf("persisted associations = %+v, want one group permission", loaded.Groups)
	}
}

func TestPermissionAndGroupCanBeRecreatedAfterDelete(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatalf("auth.Migrate() error = %v", err)
	}

	permission, err := auth.EnsurePermission(ctx, db.DB, "admin.widgets.recreate", "Recreate widgets")
	if err != nil {
		t.Fatalf("EnsurePermission(initial): %v", err)
	}
	group, err := auth.EnsureGroup(ctx, db.DB, "recreated-viewers")
	if err != nil {
		t.Fatalf("EnsureGroup(initial): %v", err)
	}
	if err := db.WithContext(ctx).Delete(&permission).Error; err != nil {
		t.Fatalf("delete permission: %v", err)
	}
	if err := db.WithContext(ctx).Delete(&group).Error; err != nil {
		t.Fatalf("delete group: %v", err)
	}

	recreatedPermission, err := auth.EnsurePermission(
		ctx,
		db.DB,
		permission.Key,
		permission.Description,
	)
	if err != nil {
		t.Fatalf("EnsurePermission(recreate): %v", err)
	}
	recreatedGroup, err := auth.EnsureGroup(ctx, db.DB, group.Name)
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

	var loaded auth.Group
	if err := db.WithContext(ctx).
		Preload("Permissions").
		First(&loaded, recreatedGroup.ID).Error; err != nil {
		t.Fatalf("load recreated group: %v", err)
	}
	if len(loaded.Permissions) != 1 || loaded.Permissions[0].Key != permission.Key {
		t.Fatalf("recreated group permissions = %+v, want %q", loaded.Permissions, permission.Key)
	}
}

func TestPermissionKeyColumnAvoidsMySQLReservedWord(t *testing.T) {
	parsed, err := schema.Parse(&auth.Permission{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("schema.Parse() error = %v", err)
	}
	field := parsed.LookUpField("Key")
	if field == nil {
		t.Fatal("Permission.Key field missing")
	}
	if strings.EqualFold(field.DBName, "key") {
		t.Fatalf("Permission.Key column %q is a reserved MySQL identifier", field.DBName)
	}
}
