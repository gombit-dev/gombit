package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/auth/authtest"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/framework"
)

// auditRequest is a POST with the headers the audit trail correlates.
func auditRequest(app *framework.App, path, access, body, requestID string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "audit-test/1.0")
	req.Header.Set(framework.RequestIDHeader, requestID)
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	app.Router().ServeHTTP(rec, req)
	return rec
}

// eventsOf are the user's events (as actor or target), oldest first.
func eventsOf(t *testing.T, db *database.DB, userID uint) []auth.AuditEvent {
	t.Helper()
	var events []auth.AuditEvent
	if err := db.Where("actor_user_id = ? OR target_user_id = ?", userID, userID).Order("id").Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	return events
}

func TestAuditTrail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := openSQLite(t)
	runAuditTrail(t, db)
	runAuditGrantInTransaction(t, db)
}

// warnLogger is a GORM logger that records its warnings.
type warnLogger struct {
	gormlogger.Interface
	mu    sync.Mutex
	warns []string
}

func (l *warnLogger) LogMode(gormlogger.LogLevel) gormlogger.Interface { return l }

func (l *warnLogger) Warn(_ context.Context, msg string, data ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, fmt.Sprintf(msg, data...))
}

// runAuditGrantInTransaction: a grant helper given the caller's transaction
// records in a savepoint, so an audit insert that fails (the table not
// migrated yet) does not abort the transaction on PostgreSQL: the grant
// commits, whether or not GORM's nested transactions are disabled, and the
// lost event is logged as a warning through the database's logger.
func runAuditGrantInTransaction(t *testing.T, db *database.DB) {
	t.Helper()
	for _, nested := range []string{"nested transactions", "nested transactions disabled"} {
		t.Run(nested, func(t *testing.T) {
			logs := &warnLogger{Interface: gormlogger.Discard}
			session := db.Session(&gorm.Session{Logger: logs, DisableNestedTransaction: nested != "nested transactions"})
			auditGrantInTransaction(t, db, session)
			logs.mu.Lock()
			defer logs.mu.Unlock()
			if len(logs.warns) != 1 || !strings.Contains(logs.warns[0], string(auth.AuditPermissionGranted)) {
				t.Fatalf("warnings = %q, want the lost %s event", logs.warns, auth.AuditPermissionGranted)
			}
		})
	}
}

func auditGrantInTransaction(t *testing.T, db *database.DB, session *gorm.DB) {
	t.Helper()
	ctx := context.Background()
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	user := auth.User{Email: fmt.Sprintf("tx-grant-%d@example.com", time.Now().UnixNano()), PasswordHash: "x"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	perm, err := auth.EnsurePermission(ctx, db.DB, fmt.Sprintf("tx.grant.%d", time.Now().UnixNano()), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrator().DropTable(&auth.AuditEvent{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = auth.Migrate(db.DB) })
	err = session.Transaction(func(tx *gorm.DB) error {
		if err := auth.GrantPermissionToUser(ctx, tx, &user, &perm); err != nil {
			return err
		}
		// A statement after the failed audit insert, in the same transaction.
		return tx.Model(&auth.User{}).Where("id = ?", user.ID).Update("is_superuser", false).Error
	})
	if err != nil {
		t.Fatalf("a grant in a transaction without the audit table = %v; the failed audit aborted it", err)
	}
	var granted int64
	db.Table("auth_user_permissions").Where("user_id = ? AND permission_id = ?", user.ID, perm.ID).Count(&granted)
	if granted != 1 {
		t.Fatal("the grant did not commit")
	}
}

// runAuditTrail: the security-sensitive operations of a Bearer app each
// leave one event with the actor, the target, the time and the request
// (request id, client IP, user agent), and no password or token anywhere.
func runAuditTrail(t *testing.T, db *database.DB) {
	t.Helper()
	mail := &authtest.Mailer{}
	app := newVerificationApp(t, db, config.AuthModeJWT, mail, config.EnvironmentTest)
	email := fmt.Sprintf("audit-%d@example.com", time.Now().UnixNano())
	const password = "audit-secret-password"
	cred := func(pw string) string { return `{"email":"` + email + `","password":"` + pw + `"}` }

	if rec := auditRequest(app, "/api/v1/auth/register", "", cred(password), "req-register"); rec.Code != http.StatusOK {
		t.Fatalf("register = %d %s", rec.Code, rec.Body.String())
	}
	if rec := auditRequest(app, "/api/v1/auth/login", "", cred("wrong-password-x"), "req-bad-login"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad login = %d", rec.Code)
	}
	login := auditRequest(app, "/api/v1/auth/login", "", cred(password), "req-login")
	var pair dataBody[tokenPair]
	if err := json.Unmarshal(login.Body.Bytes(), &pair); err != nil || login.Code != http.StatusOK {
		t.Fatalf("login = %d %s", login.Code, login.Body.String())
	}
	msg, _ := mail.Last(email)
	if rec := auditRequest(app, "/api/v1/auth/verify-email", "", `{"token":"`+msg.Token+`"}`, "req-verify"); rec.Code != http.StatusOK {
		t.Fatalf("verify = %d", rec.Code)
	}
	rotated := refreshUser(t, app, pair.Data.RefreshToken)
	// The rotated-away token again: reuse, every session revoked.
	if rec := auditRequest(app, "/api/v1/auth/refresh", "", `{"refresh_token":"`+pair.Data.RefreshToken+`"}`, "req-reuse"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("reuse = %d", rec.Code)
	}
	second := loginUser(t, app, email, password)
	if rec := auditRequest(app, "/api/v1/auth/logout", "", `{"refresh_token":"`+second.RefreshToken+`"}`, "req-logout"); rec.Code != http.StatusOK {
		t.Fatalf("logout = %d", rec.Code)
	}
	third := loginUser(t, app, email, password)
	revoke := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/sessions?scope=all", nil)
	req.Header.Set("Authorization", "Bearer "+third.AccessToken)
	req.Header.Set(framework.RequestIDHeader, "req-revoke-all")
	app.Router().ServeHTTP(revoke, req)
	if revoke.Code != http.StatusOK {
		t.Fatalf("revoke all = %d %s", revoke.Code, revoke.Body.String())
	}

	var user auth.User
	if err := db.Where("email = ?", email).First(&user).Error; err != nil {
		t.Fatal(err)
	}
	events := eventsOf(t, db, user.ID)
	var got []string
	for _, e := range events {
		got = append(got, string(e.Action))
	}
	want := []string{
		"auth.user.registered", "auth.login.failed", "auth.login.succeeded", "auth.email.verified",
		"auth.refresh.reuse_detected", "auth.login.succeeded", "auth.logout", "auth.login.succeeded", "auth.session.revoked",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("events = %v\nwant %v", got, want)
	}
	byRequest := map[string]auth.AuditEvent{}
	for _, e := range events {
		if e.RequestID != "" {
			byRequest[e.RequestID] = e
		}
		if e.TargetUserID == nil || *e.TargetUserID != user.ID || e.OccurredAt.IsZero() {
			t.Errorf("%s: target %v, occurred %v", e.Action, e.TargetUserID, e.OccurredAt)
		}
	}
	bad := byRequest["req-bad-login"]
	if bad.Action != auth.AuditLoginFailed || bad.ActorUserID != nil || bad.UserAgent != "audit-test/1.0" || bad.IP == "" || !strings.Contains(bad.Details, "wrong_password") {
		t.Errorf("failed login = %+v", bad)
	}
	ok := byRequest["req-login"]
	if ok.ActorUserID == nil || *ok.ActorUserID != user.ID || ok.IP == "" {
		t.Errorf("login = %+v", ok)
	}
	revoked := byRequest["req-revoke-all"]
	if revoked.Action != auth.AuditSessionRevoked || revoked.ActorUserID == nil || *revoked.ActorUserID != user.ID || !strings.Contains(revoked.Details, `"scope":"all"`) {
		t.Errorf("revoke all = %+v", revoked)
	}

	// Never a password or a token, in any column of any event.
	var all []auth.AuditEvent
	if err := db.Find(&all).Error; err != nil {
		t.Fatal(err)
	}
	secrets := []string{password, "wrong-password-x", pair.Data.RefreshToken, pair.Data.AccessToken, rotated.RefreshToken, second.RefreshToken, third.AccessToken, msg.Token}
	for _, e := range all {
		row := fmt.Sprintf("%+v", e)
		for _, secret := range secrets {
			if secret != "" && strings.Contains(row, secret) {
				t.Fatalf("an audit event holds a secret: %s", row)
			}
		}
	}

	// An unknown address: a failed login with no target, and no address.
	unknown := fmt.Sprintf("nobody-%d@example.com", time.Now().UnixNano())
	if rec := auditRequest(app, "/api/v1/auth/login", "", `{"email":"`+unknown+`","password":"whatever-pass"}`, "req-unknown"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown login = %d", rec.Code)
	}
	var anon auth.AuditEvent
	if err := db.Where("request_id = ?", "req-unknown").First(&anon).Error; err != nil {
		t.Fatal(err)
	}
	if anon.TargetUserID != nil || anon.ActorUserID != nil || strings.Contains(fmt.Sprintf("%+v", anon), unknown) {
		t.Fatalf("unknown-account login = %+v", anon)
	}

	// An inbound request id too long to keep whole is replaced at the
	// boundary: the event carries exactly the id the response and the logs
	// do, never a cut prefix of the inbound one.
	longID := strings.Repeat("x", 200)
	longReq := auditRequest(app, "/api/v1/auth/login", "", cred("wrong-password-y"), longID)
	answered := longReq.Header().Get(framework.RequestIDHeader)
	if answered == "" || strings.HasPrefix(longID, answered) {
		t.Fatalf("the response's request id = %q, want a generated one", answered)
	}
	var correlated auth.AuditEvent
	if err := db.Where("request_id = ?", answered).First(&correlated).Error; err != nil || correlated.Action != auth.AuditLoginFailed {
		t.Fatalf("the event for request %q = %+v, %v", answered, correlated, err)
	}

	listed, err := auth.ListAuditEvents(context.Background(), db.DB, auth.AuditQuery{TargetUserID: user.ID, Action: auth.AuditLoginSucceeded})
	if err != nil || len(listed) != 3 || listed[0].ID < listed[2].ID {
		t.Fatalf("ListAuditEvents = %d events (%v), want the 3 logins, newest first", len(listed), err)
	}

	// Newest first means by when the operation ran, not by insertion: the
	// newer event is inserted first, and the older one in another zone.
	base := time.Now().UTC().Truncate(time.Second)
	east := time.FixedZone("UTC+5", 5*60*60)
	for _, at := range []time.Time{base.Add(time.Hour), base.Add(-time.Hour).In(east)} {
		if err := auth.RecordAuditEvent(context.Background(), db.DB, auth.AuditEvent{Action: "app.ordered", TargetUserID: &user.ID, OccurredAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	ordered, err := auth.ListAuditEvents(context.Background(), db.DB, auth.AuditQuery{TargetUserID: user.ID, Action: "app.ordered"})
	if err != nil || len(ordered) != 2 || !ordered[0].OccurredAt.After(ordered[1].OccurredAt) || ordered[0].ID > ordered[1].ID {
		t.Fatalf("ListAuditEvents over inverted inserts = %+v, %v; want the later occurrence first", ordered, err)
	}
}

type recordingAuditor struct {
	mu     sync.Mutex
	events []auth.AuditEvent
	err    error
}

func (a *recordingAuditor) Record(_ context.Context, e auth.AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, e)
	return a.err
}

// TestAuditorIsReplaceable: framework.WithAuthAuditor receives the events,
// completed with the request; an Auditor that fails does not fail the
// operation it records.
func TestAuditorIsReplaceable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := openSQLite(t)
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	rec := &recordingAuditor{err: errors.New("siem down")}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.BcryptCost = bcrypt.MinCost
	app, err := framework.New(framework.WithConfig(cfg), framework.WithDatabase(db), framework.WithLogger(zap.NewNop()), framework.WithAuthAuditor(rec))
	if err != nil {
		t.Fatal(err)
	}
	if r := auditRequest(app, "/api/v1/auth/register", "", `{"email":"siem@example.com","password":"correct-horse"}`, "req-siem"); r.Code != http.StatusOK {
		t.Fatalf("register with a failing auditor = %d", r.Code)
	}
	if len(rec.events) != 1 || rec.events[0].Action != auth.AuditRegistered || rec.events[0].RequestID != "req-siem" {
		t.Fatalf("auditor got %+v", rec.events)
	}
	var stored int64
	db.Model(&auth.AuditEvent{}).Count(&stored)
	if stored != 0 {
		t.Fatalf("%d events in the table; the replaced auditor was the only sink", stored)
	}
	if _, err := framework.New(framework.WithAuthAuditor(nil)); err == nil {
		t.Fatal("WithAuthAuditor(nil) was accepted")
	}
}

// TestAuditGrants: the group and permission helpers record what they
// granted, with the actor the context carries.
func TestAuditGrants(t *testing.T) {
	runAuditGrants(t, openSQLite(t))
}

// runAuditGrants: each grant helper records its event when it changed
// something, and only then: granting what is already granted is a no-op
// (nil, no event), however many times.
func runAuditGrants(t *testing.T, db *database.DB) {
	t.Helper()
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	n := time.Now().UnixNano()
	ctx := context.Background()
	admin := auth.User{Email: fmt.Sprintf("admin-grants-%d@example.com", n), PasswordHash: "x"}
	user := auth.User{Email: fmt.Sprintf("grants-%d@example.com", n), PasswordHash: "x"}
	db.Create(&admin)
	db.Create(&user)
	ctx = auth.ContextWithSession(ctx, admin, 1)
	group, _ := auth.EnsureGroup(ctx, db.DB, fmt.Sprintf("editors-%d", n))
	perm, _ := auth.EnsurePermission(ctx, db.DB, fmt.Sprintf("posts.publish.%d", n), "Publish posts")
	for range 2 {
		if err := auth.AddUserToGroup(ctx, db.DB, &user, &group); err != nil {
			t.Fatal(err)
		}
		if err := auth.GrantPermissionToGroup(ctx, db.DB, &group, &perm); err != nil {
			t.Fatal(err)
		}
		if err := auth.GrantPermissionToUser(ctx, db.DB, &user, &perm); err != nil {
			t.Fatal(err)
		}
	}
	var events []auth.AuditEvent
	db.Where("actor_user_id = ?", admin.ID).Order("id").Find(&events)
	if len(events) != 3 {
		t.Fatalf("%d events for three grants each made twice, want 3 (the repeats change nothing): %+v", len(events), events)
	}
	for _, e := range events {
		if e.ActorUserID == nil || *e.ActorUserID != admin.ID {
			t.Errorf("%s: actor %v, want the admin", e.Action, e.ActorUserID)
		}
	}
	if events[0].Action != auth.AuditGroupMemberAdded || *events[0].TargetUserID != user.ID || !strings.Contains(events[0].Details, group.Name) {
		t.Errorf("member added = %+v", events[0])
	}
	if events[1].Action != auth.AuditPermissionGranted || events[1].TargetType != "group" || !strings.Contains(events[1].Details, perm.Key) {
		t.Errorf("group grant = %+v", events[1])
	}
	if events[2].Action != auth.AuditPermissionGranted || *events[2].TargetUserID != user.ID {
		t.Errorf("user grant = %+v", events[2])
	}
	if ok, err := auth.HasPermission(ctx, db.DB, user, perm.Key); err != nil || !ok {
		t.Fatalf("HasPermission after the grants = %v, %v", ok, err)
	}
	// The public entry point refuses a row that says nothing.
	if err := auth.RecordAuditEvent(ctx, db.DB, auth.AuditEvent{}); err == nil {
		t.Fatal("RecordAuditEvent(AuditEvent{}) was accepted")
	}
	if err := auth.RecordAuditEvent(ctx, db.DB, auth.AuditEvent{Action: "app.exported", Details: "not JSON"}); err == nil {
		t.Fatal("RecordAuditEvent with non-JSON details was accepted")
	}
}
