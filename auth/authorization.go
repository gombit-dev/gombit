package auth

import (
	"context"
	"errors"
	"strings"

	"github.com/gombit-dev/gombit/database"
	"gorm.io/gorm"
)

// HasPermission reports whether user has key directly, through a group, or
// through the Django-style superuser bypass.
func HasPermission(ctx context.Context, db *gorm.DB, user User, key string) (bool, error) {
	grants, err := HasPermissions(ctx, db, user, key)
	if err != nil {
		return false, err
	}
	return grants[key], nil
}

// HasPermissions checks several keys while loading the user's associations
// once. Missing users and unassigned keys are reported as false.
func HasPermissions(ctx context.Context, db *gorm.DB, user User, keys ...string) (map[string]bool, error) {
	grants := make(map[string]bool, len(keys))
	for _, key := range keys {
		grants[key] = false
	}
	if user.IsSuperuser {
		for key := range grants {
			grants[key] = true
		}
		return grants, nil
	}
	if user.ID == 0 {
		return grants, nil
	}
	if db == nil {
		return nil, errors.New("auth: nil database")
	}

	var loaded User
	err := db.WithContext(ctx).
		Preload("Permissions").
		Preload("Groups.Permissions").
		First(&loaded, user.ID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return grants, nil
	}
	if err != nil {
		return nil, err
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
func EnsurePermission(ctx context.Context, db *gorm.DB, key, description string) (Permission, error) {
	if db == nil {
		return Permission{}, errors.New("auth: nil database")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return Permission{}, errors.New("auth: permission key is required")
	}
	permission := Permission{Key: key}
	err := db.WithContext(ctx).
		Where(&Permission{Key: key}).
		Attrs(Permission{Description: strings.TrimSpace(description)}).
		FirstOrCreate(&permission).Error
	return permission, err
}

// EnsureGroup returns the group with name, creating it when needed.
func EnsureGroup(ctx context.Context, db *gorm.DB, name string) (Group, error) {
	if db == nil {
		return Group{}, errors.New("auth: nil database")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Group{}, errors.New("auth: group name is required")
	}
	group := Group{Name: name}
	err := db.WithContext(ctx).Where("name = ?", name).FirstOrCreate(&group).Error
	return group, err
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
