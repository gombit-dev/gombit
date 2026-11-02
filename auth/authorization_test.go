package auth_test

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/database"
	"gorm.io/gorm"
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

// TestEnsureConcurrentFirstUse: concurrent first callers of EnsurePermission
// and EnsureGroup for one key all get the one row and no error (#463).
// runConcurrentEnsure also runs on PostgreSQL and MySQL (integration_test.go).
func TestEnsureConcurrentFirstUse(t *testing.T) {
	runConcurrentEnsure(t, openSQLite(t))
}

func runConcurrentEnsure(t *testing.T, db *database.DB) {
	t.Helper()
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	const callers = 16
	// race runs callers concurrent ensure calls, released together, and
	// checks they all got one row with the same id.
	race := func(t *testing.T, ensure func() (uint, error)) {
		t.Helper()
		ids := make([]uint, callers)
		errs := make([]error, callers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ids[i], errs[i] = ensure()
			}()
		}
		close(start)
		wg.Wait()
		for i := range callers {
			if errs[i] != nil {
				t.Fatalf("caller %d: %v, want the row", i, errs[i])
			}
			if ids[i] == 0 || ids[i] != ids[0] {
				t.Fatalf("callers got ids %v, want one row", ids)
			}
		}
	}
	tag := strconv.FormatInt(time.Now().UnixNano(), 36)
	ctx := context.Background()

	t.Run("permission", func(t *testing.T) {
		race(t, func() (uint, error) {
			p, err := auth.EnsurePermission(ctx, db.DB, "concurrent.permission."+tag, "Concurrent")
			return p.ID, err
		})
	})
	t.Run("group", func(t *testing.T) {
		race(t, func() (uint, error) {
			g, err := auth.EnsureGroup(ctx, db.DB, "concurrent-group-"+tag)
			return g.ID, err
		})
	})
	// Each caller inside its own transaction (the default isolation:
	// READ COMMITTED on PostgreSQL, REPEATABLE READ on MySQL): the loser's
	// failed insert must not abort it (PostgreSQL), and it must see the
	// winner's row although its snapshot may predate it (MySQL). Whether a
	// caller loses is left to scheduling here; runEnsureRaceOutcomes forces
	// each outcome. SQLite's single writer serializes the transactions, so
	// there is no race to lose there.
	t.Run("in a transaction", func(t *testing.T) {
		if db.Name() == "sqlite" {
			t.Skip("SQLite serializes writing transactions")
		}
		race(t, func() (uint, error) {
			var id uint
			err := db.Transaction(func(tx *gorm.DB) error {
				p, err := auth.EnsurePermission(ctx, tx, "concurrent.tx."+tag, "Concurrent in a transaction")
				id = p.ID
				return err
			})
			return id, err
		})
	})
}

// TestEnsureRaceOutcomes: the outcomes of a lost Ensure race, each forced
// in order rather than left to scheduling (#463). runEnsureRaceOutcomes
// also runs on PostgreSQL and MySQL (integration_test.go).
func TestEnsureRaceOutcomes(t *testing.T) {
	runEnsureRaceOutcomes(t, openSQLite(t))
}

func runEnsureRaceOutcomes(t *testing.T, db *database.DB) {
	t.Helper()
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tag := strconv.FormatInt(time.Now().UnixNano(), 36)
	multiConn := func(t *testing.T) {
		t.Helper()
		if db.Name() == "sqlite" {
			t.Skip("needs a second writer while a transaction is open")
		}
	}

	// A unique violation on another constraint than the key's (here the
	// primary key; on PostgreSQL a sequence behind the table) is reported
	// as itself: the re-read finds no row for the key, which is no reason
	// to say "record not found".
	t.Run("another constraint", func(t *testing.T) {
		taken, err := auth.EnsurePermission(ctx, db.DB, "taken."+tag, "Taken")
		if err != nil {
			t.Fatal(err)
		}
		_, err = auth.EnsurePermissionRow(ctx, db.DB, &auth.Permission{ID: taken.ID, Key: "other." + tag})
		if !database.IsUniqueViolation(err) {
			t.Fatalf("err = %v, want the primary key's unique violation", err)
		}
	})

	// The caller's transaction took its snapshot before the winner
	// committed. At READ COMMITTED and on MySQL (whose locking re-read sees
	// the latest committed row) the loser gets the winner's row and its
	// transaction stays usable; a PostgreSQL REPEATABLE READ snapshot
	// cannot see the row, so the loser gets the unique violation.
	for _, level := range []sql.IsolationLevel{sql.LevelReadCommitted, sql.LevelRepeatableRead} {
		t.Run("snapshot before the winner/"+level.String(), func(t *testing.T) {
			multiConn(t)
			key := "snapshot." + strconv.Itoa(int(level)) + "." + tag
			tx := db.WithContext(ctx).Begin(&sql.TxOptions{Isolation: level})
			if tx.Error != nil {
				t.Fatal(tx.Error)
			}
			defer tx.Rollback()
			var n int64
			if err := tx.Model(&auth.Permission{}).Count(&n).Error; err != nil {
				t.Fatal(err)
			}
			winner, err := auth.EnsurePermission(ctx, db.DB, key, "Winner")
			if err != nil {
				t.Fatal(err)
			}
			got, err := auth.EnsurePermission(ctx, tx, key, "Loser")
			if db.Name() == "postgres" && level == sql.LevelRepeatableRead {
				if !database.IsUniqueViolation(err) {
					t.Fatalf("err = %v, want the unique violation", err)
				}
				return
			}
			if err != nil || got.ID != winner.ID {
				t.Fatalf("got %d, %v; want the winner's row %d", got.ID, err, winner.ID)
			}
			if err := tx.Model(&auth.Permission{}).Count(&n).Error; err != nil {
				t.Fatalf("transaction unusable after the lost race: %v", err)
			}
		})
	}

	// The winner rolls back while the loser's insert waits on it: the
	// loser's insert then succeeds and it returns its own row.
	t.Run("the winner rolls back", func(t *testing.T) {
		multiConn(t)
		key := "rollback." + tag
		winner := db.WithContext(ctx).Begin()
		if winner.Error != nil {
			t.Fatal(winner.Error)
		}
		defer winner.Rollback()
		if _, err := auth.EnsurePermission(ctx, winner, key, "Winner"); err != nil {
			t.Fatal(err)
		}
		type result struct {
			row auth.Permission
			err error
		}
		done := make(chan result, 1)
		go func() {
			row, err := auth.EnsurePermission(ctx, db.DB, key, "Loser")
			done <- result{row, err}
		}()
		waitForBlockedInsert(t, db, done)
		if err := winner.Rollback().Error; err != nil {
			t.Fatal(err)
		}
		r := <-done
		if r.err != nil || r.row.ID == 0 || r.row.Description != "Loser" {
			t.Fatalf("loser got %+v, %v; want its own row", r.row, r.err)
		}
	})
}

// waitForBlockedInsert waits until a session of this test's user is running
// an INSERT into permissions, which, with the winner's insert uncommitted,
// waits on it. Both servers show a user its own sessions without further
// privileges. It fails if the loser returns first.
func waitForBlockedInsert[T any](t *testing.T, db *database.DB, done <-chan T) {
	t.Helper()
	query := `SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND state = 'active'
		AND query LIKE 'INSERT INTO "permissions"%'`
	if db.Name() == "mysql" {
		query = "SELECT count(*) FROM information_schema.PROCESSLIST " +
			"WHERE DB = DATABASE() AND INFO LIKE 'INSERT INTO `permissions`%'"
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case r := <-done:
			t.Fatalf("loser returned %+v before the winner ended", r)
		default:
		}
		var n int64
		if err := db.Raw(query).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the loser's insert never waited on the winner")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
