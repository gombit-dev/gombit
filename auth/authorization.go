package auth

import (
	"context"
	"errors"
	"strings"

	"github.com/gombit-dev/gombit/database"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// HasPermission reports whether user has key directly, through a group, or
// through the Django-style superuser bypass. A disabled account has none.
func HasPermission(ctx context.Context, db *gorm.DB, user User, key string) (bool, error) {
	grants, err := HasPermissions(ctx, db, user, key)
	if err != nil {
		return false, err
	}
	return grants[key], nil
}

// HasPermissions checks several keys while loading the user's associations
// once. Missing users and unassigned keys are reported as false, and so is
// every key of a disabled account (User.Disabled), a superuser's too. For a
// stored user (an ID), the row is reloaded and decides, the superuser flag
// and the account state included: a snapshot loaded before the account was
// disabled or demoted grants nothing it no longer has. A user with no ID
// (never stored) is judged as given.
func HasPermissions(ctx context.Context, db *gorm.DB, user User, keys ...string) (map[string]bool, error) {
	grants := make(map[string]bool, len(keys))
	for _, key := range keys {
		grants[key] = false
	}
	if user.ID == 0 {
		if user.IsSuperuser && !user.Disabled() {
			for key := range grants {
				grants[key] = true
			}
		}
		return grants, nil
	}
	if db == nil {
		return nil, errors.New("auth: nil database")
	}

	// The bare row first: it decides a disabled account and a superuser
	// alone, without the associations.
	var loaded User
	err := db.WithContext(ctx).First(&loaded, user.ID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return grants, nil
	}
	if err != nil {
		return nil, err
	}
	if loaded.Disabled() {
		return grants, nil
	}
	if loaded.IsSuperuser {
		for key := range grants {
			grants[key] = true
		}
		return grants, nil
	}
	err = db.WithContext(ctx).
		Preload("Permissions").
		Preload("Groups.Permissions").
		First(&loaded, user.ID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return grants, nil
	}
	if err != nil {
		return nil, err
	}
	// Reloaded with its grants: decided on this read, should the state have
	// changed since the first.
	if loaded.Disabled() {
		return grants, nil
	}
	if loaded.IsSuperuser {
		for key := range grants {
			grants[key] = true
		}
		return grants, nil
	}

	for _, permission := range loaded.Permissions {
		if _, requested := grants[permission.Key]; requested {
			grants[permission.Key] = true
		}
	}
	for _, group := range loaded.Groups {
		for _, permission := range group.Permissions {
			if _, requested := grants[permission.Key]; requested {
				grants[permission.Key] = true
			}
		}
	}
	return grants, nil
}

// EnsurePermission returns the permission for key, creating it when needed.
// Concurrent first calls for one key (replicas seeding at boot) all get the
// one row, not a unique violation (#463), outside a transaction and inside
// one at READ COMMITTED or on MySQL; a lost race does not abort the caller's
// transaction. Inside a PostgreSQL REPEATABLE READ or SERIALIZABLE
// transaction the winner's row is not visible, so a lost race returns the
// unique violation (or a serialization failure) and the caller retries the
// transaction.
func EnsurePermission(ctx context.Context, db *gorm.DB, key, description string) (Permission, error) {
	if db == nil {
		return Permission{}, errors.New("auth: nil database")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return Permission{}, errors.New("auth: permission key is required")
	}
	return ensureRow(ctx, db, "permission_key = ?", key, &Permission{Key: key, Description: strings.TrimSpace(description)})
}

// EnsureGroup returns the group with name, creating it when needed, with
// EnsurePermission's guarantee for concurrent first calls (#463).
func EnsureGroup(ctx context.Context, db *gorm.DB, name string) (Group, error) {
	if db == nil {
		return Group{}, errors.New("auth: nil database")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Group{}, errors.New("auth: group name is required")
	}
	return ensureRow(ctx, db, "name = ?", name, &Group{Name: name})
}

// ensureRow returns the row of T where query matches value, inserting fresh
// when there is none. GORM's FirstOrCreate is a read then an insert, so two
// first callers both find nothing and the second insert is a unique
// violation (#463). Here the insert runs under insertSafely's savepoint, so
// losing that race inside a caller's transaction does not abort it, and the
// loser reads the winner's row instead. That read locks (FOR SHARE; SQLite's
// dialect drops it) because on MySQL a locking read sees the latest
// committed row, where a plain read inside a REPEATABLE READ transaction
// sees the snapshot from before the winner committed. A PostgreSQL snapshot
// (REPEATABLE READ, SERIALIZABLE) cannot see it whatever the lock, and the
// violation may be another constraint's (the primary key, its sequence
// behind the table): when the re-read finds nothing, the insert's own error
// is the answer, not "record not found".
func ensureRow[T any](ctx context.Context, db *gorm.DB, query string, value string, fresh *T) (T, error) {
	var row T
	found := db.WithContext(ctx).Where(query, value).Limit(1).Find(&row)
	if found.Error != nil || found.RowsAffected > 0 {
		return row, found.Error
	}
	err := insertSafely(ctx, db, "gombit_auth_ensure", func(tx *gorm.DB) error {
		return tx.Create(fresh).Error
	})
	if err == nil {
		return *fresh, nil
	}
	if !database.IsUniqueViolation(err) {
		return row, err
	}
	insertErr := err
	err = db.WithContext(ctx).Clauses(clause.Locking{Strength: clause.LockingStrengthShare}).
		Where(query, value).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return row, insertErr
	}
	return row, err
}

// AddUserToGroup adds a persisted user to a persisted group.
func AddUserToGroup(ctx context.Context, db *gorm.DB, user *User, group *Group) error {
	if err := validateAssociation(db, user != nil && user.ID != 0, group != nil && group.ID != 0); err != nil {
		return err
	}
	added, err := insertJoinRow(ctx, db, "auth_user_groups", map[string]any{"user_id": user.ID, "group_id": group.ID})
	if err != nil || !added {
		return err
	}
	user.Groups = append(user.Groups, *group)
	return recordHelperEvent(ctx, db, AuditEvent{Action: AuditGroupMemberAdded, TargetUserID: uintPtr(user.ID), Details: details("group", group.Name)})
}

// GrantPermissionToGroup grants a persisted permission to a persisted group.
func GrantPermissionToGroup(ctx context.Context, db *gorm.DB, group *Group, permission *Permission) error {
	if err := validateAssociation(db, group != nil && group.ID != 0, permission != nil && permission.ID != 0); err != nil {
		return err
	}
	added, err := insertJoinRow(ctx, db, "auth_group_permissions", map[string]any{"group_id": group.ID, "permission_id": permission.ID})
	if err != nil || !added {
		return err
	}
	group.Permissions = append(group.Permissions, *permission)
	return recordHelperEvent(ctx, db, AuditEvent{Action: AuditPermissionGranted, TargetType: "group", TargetID: group.Name, Details: details("group", group.Name, "permission", permission.Key)})
}

// GrantPermissionToUser grants a persisted permission directly to a user.
func GrantPermissionToUser(ctx context.Context, db *gorm.DB, user *User, permission *Permission) error {
	if err := validateAssociation(db, user != nil && user.ID != 0, permission != nil && permission.ID != 0); err != nil {
		return err
	}
	added, err := insertJoinRow(ctx, db, "auth_user_permissions", map[string]any{"user_id": user.ID, "permission_id": permission.ID})
	if err != nil || !added {
		return err
	}
	user.Permissions = append(user.Permissions, *permission)
	return recordHelperEvent(ctx, db, AuditEvent{Action: AuditPermissionGranted, TargetUserID: uintPtr(user.ID), Details: details("permission", permission.Key)})
}

// insertJoinRow inserts one many-to-many row into table and reports whether
// it did: a row already there (the join table's primary key) is no change,
// and nothing to record. One INSERT decides, atomically: no read first,
// which another grant could race. Inside a caller's transaction the
// conflict is rolled back to a savepoint (insertSafely), so the
// transaction goes on.
func insertJoinRow(ctx context.Context, db *gorm.DB, table string, row map[string]any) (bool, error) {
	err := insertSafely(ctx, db, "gombit_auth_grant", func(tx *gorm.DB) error {
		return tx.Table(table).Create(row).Error
	})
	if database.IsUniqueViolation(err) {
		return false, nil
	}
	return err == nil, err
}

// recordHelperEvent records a grant made through the helpers above. It does
// not fail the grant, which has already happened (an application that has
// not migrated auth_audit_events keeps granting); an event it could not
// write is logged as a warning through db's logger, the application's
// database logger. The one failure it returns is a caller's transaction the
// failed write left unusable (errTransactionLost): the grant in it will not
// commit, and the caller must know now.
func recordHelperEvent(ctx context.Context, db *gorm.DB, event AuditEvent) error {
	err := RecordAuditEvent(ctx, db, event)
	if err == nil {
		return nil
	}
	if db.Logger != nil {
		db.Logger.Warn(ctx, "auth: record an audit event (%s): %v", event.Action, err)
	}
	if errors.Is(err, errTransactionLost) {
		return err
	}
	return nil
}

func validateAssociation(db *gorm.DB, owners ...bool) error {
	if db == nil {
		return errors.New("auth: nil database")
	}
	for _, valid := range owners {
		if !valid {
			return errors.New("auth: association models must be persisted")
		}
	}
	return nil
}
