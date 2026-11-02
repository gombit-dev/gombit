package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gombit-dev/gombit/contract"
	"gorm.io/gorm"
)

// Account state (AUTH-6, #336): an account is active or disabled
// (User.DisabledAt). Disabling it revokes every session and voids every
// one-time link of the user in one transaction, under the user's row lock,
// and from then on every path refuses it: a login (after the password, so
// the state is told only to whoever knows it), a session start, a token
// refresh, an access token, a one-time link, a message. Enabling it lets
// the user sign in again; the sessions and links it had stay revoked.

// PermissionManageUsers is the permission POST {prefix}/auth/users/{id}/disable
// and /enable require (a superuser has every permission). Create it with
// EnsurePermission and grant it to the users or groups that may.
const PermissionManageUsers = "auth.users.manage"

// DisableUser disables userID's account: its sessions are revoked and its
// one-time links voided, in one transaction under the user's row lock, so a
// login, a refresh or a link that races it either finished before it or is
// refused after. Disabling a disabled account changes nothing (its
// DisabledAt stays). It returns the user, and ErrUserNotFound for an
// unknown userID. It checks no permission: it is for an app's own code and
// commands; the HTTP route checks PermissionManageUsers.
func (s *Service) DisableUser(ctx context.Context, userID uint) (User, error) {
	return s.setUserState(ctx, userID, true, nil)
}

// EnableUser makes userID's disabled account active again: the user may
// sign in. The sessions and links revoked when it was disabled stay
// revoked. Enabling an active account changes nothing. It returns the user,
// and ErrUserNotFound for an unknown userID. Like DisableUser, it checks no
// permission.
func (s *Service) EnableUser(ctx context.Context, userID uint) (User, error) {
	return s.setUserState(ctx, userID, false, nil)
}

// setUserState disables or enables userID's account under its row lock,
// after check (when given) accepted the locked row: a rule on the account
// (who may change it) is decided on the row the change is made to, never on
// an earlier read.
func (s *Service) setUserState(ctx context.Context, userID uint, disable bool, check func(locked User) error) (User, error) {
	var user User
	changed := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, err := lockUserTx(tx, userID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errUserNotFound
		}
		if err != nil {
			return err
		}
		if check != nil {
			if err := check(locked); err != nil {
				return err
			}
		}
		user = locked
		if locked.Disabled() == disable {
			return nil
		}
		if !disable {
			if err := tx.Model(&User{}).Where("id = ?", userID).Update("disabled_at", nil).Error; err != nil {
				return err
			}
			user.DisabledAt = nil
			changed = true
			return nil
		}
		now := s.now()
		if err := tx.Model(&User{}).Where("id = ?", userID).Update("disabled_at", now).Error; err != nil {
			return err
		}
		if err := revokeAllTx(tx, userID, now); err != nil {
			return err
		}
		if err := voidTokensTx(tx, userID, 0, now); err != nil {
			return err
		}
		user.DisabledAt = &now
		changed = true
		return nil
	})
	if err != nil {
		return User{}, err
	}
	if changed {
		action := AuditUserEnabled
		if disable {
			action = AuditUserDisabled
		}
		s.Audit(ctx, AuditEvent{Action: action, TargetUserID: uintPtr(user.ID)})
	}
	return user, nil
}

var (
	errSuperuserOnly     = errors.New("auth: only a superuser may change a superuser's account")
	errCannotDisableSelf = errors.New("auth: an account cannot disable itself")
)

type userStateInput struct {
	ID uint `path:"id" minimum:"1" doc:"The user's id"`
}

// authUserState is named for its schema, like AuthSession.
type authUserState struct {
	ID       uint `json:"id" example:"42" doc:"The user's id"`
	Disabled bool `json:"disabled" example:"true" doc:"True when the account is disabled"`
}

type userStateOutput struct {
	Body contract.Data[authUserState]
}

func registerAccountStateRoutes(api huma.API, svc *Service, prefix, security string, gate func(huma.Context, func(huma.Context))) {
	for _, route := range []struct {
		op, path, summary, description string
		apply                          func(context.Context, *userStateInput) (*userStateOutput, error)
	}{
		{
			op: "auth-disable-user", path: "/auth/users/{id}/disable",
			summary:     "Disable a user's account",
			description: "Requires the " + PermissionManageUsers + " permission (a superuser has it), and a superuser to disable a superuser; no one disables their own account (409). Revokes every session of the user and voids their links, in one transaction; from then on the account cannot sign in, and no token issued to it works. Idempotent.",
			apply:       svc.disableUserRoute,
		},
		{
			op: "auth-enable-user", path: "/auth/users/{id}/enable",
			summary:     "Enable a disabled user's account",
			description: "Requires the " + PermissionManageUsers + " permission (a superuser has it), and a superuser to enable a superuser. The user may sign in again; the sessions revoked when the account was disabled stay revoked. Idempotent.",
			apply:       svc.enableUserRoute,
		},
	} {
		huma.Register(api, withRequestInfo(huma.Operation{
			OperationID: route.op,
			Method:      http.MethodPost,
			Path:        prefix + route.path,
			Summary:     route.summary,
			Description: route.description,
			Tags:        []string{"Auth"},
			Security:    []map[string][]string{{security: {}}},
			Middlewares: huma.Middlewares{gate},
		}), route.apply)
	}
}

func (s *Service) disableUserRoute(ctx context.Context, input *userStateInput) (*userStateOutput, error) {
	return s.changeUserState(ctx, input.ID, true)
}

func (s *Service) enableUserRoute(ctx context.Context, input *userStateInput) (*userStateOutput, error) {
	return s.changeUserState(ctx, input.ID, false)
}

// changeUserState checks the signed-in user may change target's state, then
// changes it. The permission is the actor's, checked first; the rules on the
// target (a superuser's account is a superuser's to change, no one disables
// themselves) are decided under the target's row lock.
func (s *Service) changeUserState(ctx context.Context, targetID uint, disable bool) (*userStateOutput, error) {
	actor, _, err := requestSession(ctx)
	if err != nil {
		return nil, err
	}
	allowed, err := HasPermission(ctx, s.db, actor, PermissionManageUsers)
	if err != nil {
		return nil, contract.WithContext(ctx, contract.Internal("check permission"))
	}
	if !allowed {
		return nil, contract.WithContext(ctx, contract.Authorization("permission denied: "+PermissionManageUsers))
	}
	user, err := s.setUserState(ctx, targetID, disable, func(locked User) error {
		// The permission must not reach above the one who holds it.
		if locked.IsSuperuser && !actor.IsSuperuser {
			return errSuperuserOnly
		}
		if disable && locked.ID == actor.ID {
			return errCannotDisableSelf
		}
		return nil
	})
	switch {
	case errors.Is(err, errUserNotFound):
		return nil, contract.WithContext(ctx, contract.NotFound("user not found"))
	case errors.Is(err, errSuperuserOnly):
		return nil, contract.WithContext(ctx, contract.Authorization("only a superuser may change a superuser's account"))
	case errors.Is(err, errCannotDisableSelf):
		return nil, contract.WithContext(ctx, contract.Conflict("an account cannot disable itself"))
	case err != nil:
		return nil, mapServiceError(ctx, err)
	}
	return &userStateOutput{Body: contract.Data[authUserState]{Data: authUserState{ID: user.ID, Disabled: user.Disabled()}}}, nil
}
