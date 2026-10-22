package framework

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
)

// waitingMailer sends nothing: it returns only when its context is done.
type waitingMailer struct {
	once     sync.Once
	entered  chan struct{}
	returned chan struct{}
}

func (m *waitingMailer) Send(ctx context.Context, _ auth.Message) error {
	m.once.Do(func() { close(m.entered) })
	<-ctx.Done()
	select {
	case <-m.returned:
	default:
		close(m.returned)
	}
	return ctx.Err()
}

// TestRunContextClosesAuthBeforeStopHooks: a password reset message is
// still being sent, by a Mailer that waits on its context, when the App
// stops. The App owns the auth Service: it closes it first, so the send is
// cancelled (not abandoned) before any stop hook (the app's database close,
// say) runs. Afterwards, the forgot route refuses work it could no longer
// do, the same way for any address.
func TestRunContextClosesAuthBeforeStopHooks(t *testing.T) {
	db, err := database.Open(config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: "file:auth-lifecycle?mode=memory&cache=shared&_fk=1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Auth.JWTSecret = "framework-auth-lifecycle-secret-32b"
	cfg.Auth.BcryptCost = bcrypt.MinCost
	signup, err := auth.NewService(db.DB, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signup.Register(context.Background(), "ada@example.com", "correct-horse"); err != nil {
		t.Fatal(err)
	}

	mailer := &waitingMailer{entered: make(chan struct{}), returned: make(chan struct{})}
	app, err := New(WithConfig(cfg), WithDatabase(db), WithLogger(zap.NewNop()), WithAuthMailer(mailer), WithShutdownTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	hookSawSendDone := make(chan bool, 1)
	app.OnStop(func(context.Context) error {
		select {
		case <-mailer.returned:
			hookSawSendDone <- true
		default:
			hookSawSendDone <- false
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunContext(ctx, app) }()
	waitForHTTP(t, app, "/livez")

	resp, err := http.Post("http://"+app.Addr()+"/api/v1/auth/password/forgot", "application/json", strings.NewReader(`{"email":"ada@example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("forgot = %d, want 202", resp.StatusCode)
	}
	select {
	case <-mailer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the reset message was never sent")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("RunContext = %v, want the auth close to report its cut-short drain", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunContext did not return with a send waiting on its context")
	}
	if !<-hookSawSendDone {
		t.Fatal("a stop hook ran while the reset message was still being sent")
	}

	// Closed: the route refuses, and the same way for an account and for an
	// address that is none.
	forgot := func(email string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/password/forgot", strings.NewReader(`{"email":"`+email+`"}`))
		req.Header.Set("Content-Type", "application/json")
		app.Router().ServeHTTP(rec, req)
		return rec
	}
	code := func(rec *httptest.ResponseRecorder) string {
		var body struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return body.Error.Code + ": " + body.Error.Message
	}
	known, unknown := forgot("ada@example.com"), forgot("nobody@example.com")
	if known.Code != http.StatusServiceUnavailable || unknown.Code != known.Code || code(known) != code(unknown) ||
		known.Header().Get("Retry-After") == "" || known.Header().Get("Retry-After") != unknown.Header().Get("Retry-After") {
		t.Fatalf("after stop: account %d %q (Retry-After %q), no account %d %q", known.Code, code(known), known.Header().Get("Retry-After"), unknown.Code, code(unknown))
	}
}
