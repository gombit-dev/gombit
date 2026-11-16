package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gombit-dev/gombit/contract"
)

// authMFAStatus is named for its schema, like AuthSession.
type authMFAStatus struct {
	Enabled           bool `json:"enabled" doc:"True when a login needs a multi-factor code"`
	PendingEnrollment bool `json:"pending_enrollment" doc:"True while an enrollment awaits its confirming code"`
	RecoveryCodesLeft int  `json:"recovery_codes_remaining" doc:"Unused recovery codes, while enabled"`
}

func toMFAStatus(st MFAStatus) authMFAStatus {
	return authMFAStatus(st)
}

type mfaStatusOutput struct {
	Body contract.Data[authMFAStatus]
}

type authTOTPEnrollBody struct {
	CurrentPassword string `json:"current_password" minLength:"1" maxLength:"72" doc:"The password the user has now"`
}

type totpEnrollInput struct {
	Body authTOTPEnrollBody
}

type authTOTPEnrollment struct {
	Secret     string `json:"secret" example:"JBSWY3DPEHPK3PXP" doc:"The TOTP secret, base32, to type into an authenticator app"`
	OTPAuthURI string `json:"otpauth_uri" example:"otpauth://totp/Gombit:ada@example.com?..." doc:"The otpauth:// URI, to show as a QR code"`
}

type totpEnrollOutput struct {
	Body contract.Data[authTOTPEnrollment]
}

type authTOTPConfirmBody struct {
	Code string `json:"code" minLength:"1" maxLength:"32" example:"123456" doc:"A code from the authenticator just set up"`
}

type totpConfirmInput struct {
	Body authTOTPConfirmBody
}

type authRecoveryCodes struct {
	RecoveryCodes []string `json:"recovery_codes" doc:"Single-use recovery codes, shown this once: each signs in once in place of an authenticator code"`
}

type recoveryCodesOutput struct {
	Body contract.Data[authRecoveryCodes]
}

// authMFAProofBody is both factors, for the operations that change the
// second one (disable, new recovery codes).
type authMFAProofBody struct {
	CurrentPassword string `json:"current_password" minLength:"1" maxLength:"72" doc:"The password the user has now"`
	MFACode         string `json:"mfa_code" minLength:"1" maxLength:"32" doc:"A code from the authenticator, or a recovery code"`
}

type mfaProofInput struct {
	Body authMFAProofBody
}

func registerMFARoutes(api huma.API, svc *Service, prefix, security string, gate func(huma.Context, func(huma.Context))) {
	signedIn := func(op huma.Operation) huma.Operation {
		op.Tags = []string{"Auth"}
		op.Security = []map[string][]string{{security: {}}}
		op.Middlewares = huma.Middlewares{gate}
		return withRequestInfo(op)
	}
	throttled := func(op huma.Operation) huma.Operation {
		op.Errors = []int{http.StatusTooManyRequests}
		return signedIn(op)
	}
	huma.Register(api, signedIn(huma.Operation{
		OperationID: "auth-mfa-status",
		Method:      http.MethodGet,
		Path:        prefix + "/auth/mfa",
		Summary:     "The signed-in user's multi-factor authentication",
	}), svc.mfaStatus)

	enrollPath := prefix + "/auth/mfa/totp"
	huma.Register(api, throttled(huma.Operation{
		OperationID: "auth-mfa-totp-enroll",
		Method:      http.MethodPost,
		Path:        enrollPath,
		Summary:     "Begin enrolling a TOTP authenticator",
		Description: "Requires the current password (422 on current_password). Returns the secret and otpauth:// URI for an authenticator app; nothing changes for the login until POST /auth/mfa/totp/confirm gets a code from it. A new enrollment replaces one pending; 409 while MFA is enabled.",
	}), svc.enrollTOTP)
	declareRetryAfter(api, enrollPath, http.StatusTooManyRequests, throttleRetryAfterDoc)

	confirmPath := prefix + "/auth/mfa/totp/confirm"
	huma.Register(api, throttled(huma.Operation{
		OperationID: "auth-mfa-totp-confirm",
		Method:      http.MethodPost,
		Path:        confirmPath,
		Summary:     "Confirm a TOTP enrollment and enable MFA",
		Description: "A code from the authenticator just set up enables multi-factor authentication: from then on a login needs mfa_code. Returns the recovery codes, shown this once. A wrong code is a 422 on mfa_code; 409 with no enrollment pending.",
	}), svc.confirmTOTP)
	declareRetryAfter(api, confirmPath, http.StatusTooManyRequests, throttleRetryAfterDoc)

	codesPath := prefix + "/auth/mfa/recovery-codes"
	huma.Register(api, throttled(huma.Operation{
		OperationID: "auth-mfa-recovery-codes",
		Method:      http.MethodPost,
		Path:        codesPath,
		Summary:     "Replace the recovery codes",
		Description: "Requires the current password and a code from the authenticator (or a recovery code): new codes are a second factor, so a stolen session and password alone do not mint them. The old recovery codes stop working; the new ones are shown this once. 409 while MFA is not enabled.",
	}), svc.regenerateRecoveryCodes)
	declareRetryAfter(api, codesPath, http.StatusTooManyRequests, throttleRetryAfterDoc)

	disablePath := prefix + "/auth/mfa/disable"
	huma.Register(api, throttled(huma.Operation{
		OperationID: "auth-mfa-disable",
		Method:      http.MethodPost,
		Path:        disablePath,
		Summary:     "Turn multi-factor authentication off",
		Description: "Requires the current password and a code from the authenticator (or a recovery code): a stolen session and password alone do not remove the second factor. 409 while MFA is not enabled.",
	}), svc.disableMFA)
	declareRetryAfter(api, disablePath, http.StatusTooManyRequests, throttleRetryAfterDoc)

	huma.Register(api, signedIn(huma.Operation{
		OperationID: "auth-mfa-reset",
		Method:      http.MethodPost,
		Path:        prefix + "/auth/users/{id}/mfa/reset",
		Summary:     "Remove a user's multi-factor authentication",
		Description: "For a user who lost their authenticator and recovery codes; every session of the user ends with it (the lost device may hold one). Requires the " + PermissionManageUsers + " permission (a superuser has it), and a superuser to reset a superuser; an account turns its own off with POST /auth/mfa/disable (409). Idempotent.",
	}), svc.resetUserMFA)
}

func (s *Service) mfaStatus(ctx context.Context, _ *struct{}) (*mfaStatusOutput, error) {
	user, _, err := requestSession(ctx)
	if err != nil {
		return nil, err
	}
	st, err := s.MFAStatusOf(ctx, user.ID)
	if err != nil {
		return nil, mapServiceError(ctx, err)
	}
	return &mfaStatusOutput{Body: contract.Data[authMFAStatus]{Data: toMFAStatus(st)}}, nil
}

func (s *Service) enrollTOTP(ctx context.Context, input *totpEnrollInput) (*totpEnrollOutput, error) {
	user, _, err := requestSession(ctx)
	if err != nil {
		return nil, err
	}
	enrollment, err := s.BeginTOTPEnrollment(ctx, user.ID, input.Body.CurrentPassword)
	if err != nil {
		return nil, mapServiceError(ctx, err)
	}
	return &totpEnrollOutput{Body: contract.Data[authTOTPEnrollment]{Data: authTOTPEnrollment{Secret: enrollment.Secret, OTPAuthURI: enrollment.URI}}}, nil
}

func (s *Service) confirmTOTP(ctx context.Context, input *totpConfirmInput) (*recoveryCodesOutput, error) {
	user, _, err := requestSession(ctx)
	if err != nil {
		return nil, err
	}
	codes, err := s.ConfirmTOTPEnrollment(ctx, user.ID, input.Body.Code)
	if err != nil {
		return nil, mapServiceError(ctx, err)
	}
	return &recoveryCodesOutput{Body: contract.Data[authRecoveryCodes]{Data: authRecoveryCodes{RecoveryCodes: codes}}}, nil
}

func (s *Service) regenerateRecoveryCodes(ctx context.Context, input *mfaProofInput) (*recoveryCodesOutput, error) {
	user, _, err := requestSession(ctx)
	if err != nil {
		return nil, err
	}
	codes, err := s.RegenerateRecoveryCodes(ctx, user.ID, input.Body.CurrentPassword, input.Body.MFACode)
	if err != nil {
		return nil, mapServiceError(ctx, err)
	}
	return &recoveryCodesOutput{Body: contract.Data[authRecoveryCodes]{Data: authRecoveryCodes{RecoveryCodes: codes}}}, nil
}

func (s *Service) disableMFA(ctx context.Context, input *mfaProofInput) (*mfaStatusOutput, error) {
	user, _, err := requestSession(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.DisableMFA(ctx, user.ID, input.Body.CurrentPassword, input.Body.MFACode); err != nil {
		return nil, mapServiceError(ctx, err)
	}
	return &mfaStatusOutput{Body: contract.Data[authMFAStatus]{Data: authMFAStatus{}}}, nil
}

func (s *Service) resetUserMFA(ctx context.Context, input *userStateInput) (*mfaStatusOutput, error) {
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
	err = s.resetMFA(ctx, input.ID, func(locked User) error {
		if locked.IsSuperuser && !actor.IsSuperuser {
			return errSuperuserOnly
		}
		if locked.ID == actor.ID {
			return errCannotResetOwnMFA
		}
		return nil
	})
	switch {
	case errors.Is(err, errUserNotFound):
		return nil, contract.WithContext(ctx, contract.NotFound("user not found"))
	case errors.Is(err, errSuperuserOnly):
		return nil, contract.WithContext(ctx, contract.Authorization("only a superuser may change a superuser's account"))
	case errors.Is(err, errCannotResetOwnMFA):
		return nil, contract.WithContext(ctx, contract.Conflict("an account turns its own multi-factor authentication off with POST /auth/mfa/disable"))
	case err != nil:
		return nil, mapServiceError(ctx, err)
	}
	return &mfaStatusOutput{Body: contract.Data[authMFAStatus]{Data: authMFAStatus{}}}, nil
}
