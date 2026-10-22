package auth

import (
	"context"
	"errors"
	"strings"

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
	if err := db.WithContext(ctx).Model(user).Association("Groups").Append(group); err != nil {
		return err
	}
	recordHelperEvent(ctx, db, AuditEvent{Action: AuditGroupMemberAdded, TargetUserID: uintPtr(user.ID), Details: details("group", group.Name)})
	return nil
}

// GrantPermissionToGroup grants a persisted permission to a persisted group.
func GrantPermissionToGroup(ctx context.Context, db *gorm.DB, group *Group, permission *Permission) error {
	if err := validateAssociation(db, group != nil && group.ID != 0, permission != nil && permission.ID != 0); err != nil {
		return err
	}
	if err := db.WithContext(ctx).Model(group).Association("Permissions").Append(permission); err != nil {
		return err
	}
	recordHelperEvent(ctx, db, AuditEvent{Action: AuditPermissionGranted, TargetType: "group", TargetID: group.Name, Details: details("group", group.Name, "permission", permission.Key)})
	return nil
}

// GrantPermissionToUser grants a persisted permission directly to a user.
func GrantPermissionToUser(ctx context.Context, db *gorm.DB, user *User, permission *Permission) error {
	if err := validateAssociation(db, user != nil && user.ID != 0, permission != nil && permission.ID != 0); err != nil {
		return err
	}
	if err := db.WithContext(ctx).Model(user).Association("Permissions").Append(permission); err != nil {
		return err
	}
	recordHelperEvent(ctx, db, AuditEvent{Action: AuditPermissionGranted, TargetUserID: uintPtr(user.ID), Details: details("permission", permission.Key)})
	return nil
}

// recordHelperEvent records a grant made through the helpers above. It does
// not fail the grant, which has already happened (an application that has
// not migrated auth_audit_events keeps granting); an event it could not
// write is logged as a warning through db's logger, the application's
// database logger.
func recordHelperEvent(ctx context.Context, db *gorm.DB, event AuditEvent) {
	if err := RecordAuditEvent(ctx, db, event); err != nil && db.Logger != nil {
		db.Logger.Warn(ctx, "auth: record an audit event (%s): %v", event.Action, err)
	}
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
