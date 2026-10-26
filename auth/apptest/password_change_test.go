// Package apptest_test uses package auth as an application does: through
// its production exports only (no export_test.go helpers reach it), so a
// public contract that needs something unexported fails to compile here.
package apptest_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humagin"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/auth/authtest"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
)

type changeInput struct {
	Body struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
}

type changeOutput struct {
	Body struct {
		Revoked bool `json:"revoked"`
	}
}

// appChangePassword is an application's own password-change operation,
// behind the framework's Bearer gate, classifying every failure the way an
// application would.
func appChangePassword(svc *auth.Service) func(context.Context, *changeInput) (*changeOutput, error) {
	return func(ctx context.Context, input *changeInput) (*changeOutput, error) {
		user, ok := auth.UserFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("not signed in")
		}
		revoked, err := svc.ChangePassword(ctx, user.ID, input.Body.Current, input.Body.New, auth.PasswordChange{})
		var policy auth.PasswordPolicyError
		switch {
		case errors.Is(err, auth.ErrWrongCurrentPassword):
			return nil, huma.Error422UnprocessableEntity("current: wrong")
		case errors.As(err, &policy):
			return nil, huma.Error422UnprocessableEntity(policy.Field + ": " + policy.Err.Error())
		case errors.Is(err, auth.ErrPasswordChanged):
			return nil, huma.Error409Conflict("changed meanwhile")
		case errors.Is(err, auth.ErrSessionEnded), errors.Is(err, auth.ErrUserNotFound):
			return nil, huma.Error401Unauthorized("session ended")
		case err != nil:
			return nil, huma.Error500InternalServerError("database")
		}
		out := &changeOutput{}
		out.Body.Revoked = revoked
		return out, nil
	}
}

func TestApplicationPasswordChange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.Open(config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: fmt.Sprintf("file:apptest-%d?mode=memory&cache=shared&_fk=1", time.Now().UnixNano())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.Auth.JWTSecret = "apptest-jwt-secret-at-least-32-bytes"
	cfg.Auth.BcryptCost = bcrypt.MinCost
	router := gin.New()
	api := humagin.New(router, huma.DefaultConfig("app", "1.0.0"))
	svc, err := auth.Mount(api, db.DB, cfg, auth.WithMailer(&authtest.Mailer{}))
	if err != nil {
		t.Fatal(err)
	}
	huma.Register(api, huma.Operation{
		OperationID: "app-change-password", Method: http.MethodPost, Path: "/app/password",
		Middlewares: huma.Middlewares{svc.RequireBearer()},
	}, appChangePassword(svc))

	ctx := context.Background()
	user, err := svc.Register(ctx, "app@example.com", "correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	mine, err := svc.IssueTokens(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.IssueTokens(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	post := func(access, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/app/password", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+access)
		router.ServeHTTP(rec, req)
		return rec
	}

	// The application tells the failures apart with the exported errors.
	if rec := post(mine.AccessToken, `{"current":"not-mine","new":"brand-new-horse"}`); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "current: wrong") {
		t.Fatalf("a wrong current password = %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(mine.AccessToken, `{"current":"correct-horse","new":"short"}`); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "new_password: ") {
		t.Fatalf("a short new password = %d %s", rec.Code, rec.Body.String())
	}

	// The default keeps the requesting session, and only it.
	rec := post(mine.AccessToken, `{"current":"correct-horse","new":"brand-new-horse"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"revoked":true`) {
		t.Fatalf("the change = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := svc.RotateRefresh(ctx, mine.RefreshToken); err != nil {
		t.Fatalf("the requesting session after the change: %v", err)
	}
	if _, err := svc.RotateRefresh(ctx, other.RefreshToken); err == nil {
		t.Fatal("another session survived the change")
	}

	// Called outside a request, it has no session to keep, and says why it
	// refuses in terms an application can test.
	if _, err := svc.ChangePassword(ctx, user.ID, "not-mine", "another-horse", auth.PasswordChange{}); !errors.Is(err, auth.ErrWrongCurrentPassword) {
		t.Fatalf("a wrong current password from Go = %v", err)
	}
	if _, err := svc.ChangePassword(ctx, user.ID+1000, "x", "another-horse", auth.PasswordChange{}); !errors.Is(err, auth.ErrUserNotFound) {
		t.Fatalf("an unknown user = %v", err)
	}
	if _, err := svc.ChangePassword(ctx, user.ID, "brand-new-horse", strings.Repeat("é", 40), auth.PasswordChange{}); !errors.Is(err, auth.ErrPasswordTooLong) {
		t.Fatalf("an 80-byte password = %v, want ErrPasswordTooLong", err)
	}
}
