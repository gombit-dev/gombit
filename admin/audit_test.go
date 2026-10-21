package admin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/framework"
)

func TestAdminAudit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runAdminAudit(t, openSQLite(t))
}

// runAdminAudit: every write through the admin data plane leaves one audit
// event naming the admin, the model's slug and the record's key; a write
// that fails leaves none.
func runAdminAudit(t *testing.T, db *database.DB) {
	t.Helper()
	app := newCookieAppWithDB(t, db)
	registerWidgets(t, app)
	jar := loginSuperuser(t, app)

	create := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/widgets", `{"name":"Audited","sku":"aud-1","price":10}`)
	if create.Code != http.StatusOK {
		t.Fatalf("create = %d %s", create.Code, create.Body.String())
	}
	var created rowEnvelope
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprint(asInt(created.Data["id"]))
	if rec := doRequest(app, jar, http.MethodPatch, "/api/v1/admin/resources/widgets/"+id, `{"note":"audited"}`); rec.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body.String())
	}
	// Rejected: no write, no event.
	if rec := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/widgets", `{"name":"Bad","price":"not a number"}`); rec.Code == http.StatusOK {
		t.Fatal("an invalid create succeeded")
	}
	if rec := doRequest(app, jar, http.MethodDelete, "/api/v1/admin/resources/widgets/"+id, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}

	var admin auth.User
	if err := db.Where("is_superuser = ?", true).Order("id DESC").First(&admin).Error; err != nil {
		t.Fatal(err)
	}
	var events []auth.AuditEvent
	if err := db.Where("action LIKE ?", "admin.%").Order("id").Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	want := []auth.AuditAction{auth.AuditAdminCreated, auth.AuditAdminUpdated, auth.AuditAdminDeleted}
	if len(events) != len(want) {
		t.Fatalf("admin events = %+v, want %v", events, want)
	}
	for i, e := range events {
		if e.Action != want[i] || e.TargetType != "widgets" || e.TargetID != id {
			t.Errorf("event %d = %s %s/%s, want %s widgets/%s", i, e.Action, e.TargetType, e.TargetID, want[i], id)
		}
		if e.ActorUserID == nil || *e.ActorUserID != admin.ID || e.RequestID == "" || e.IP == "" {
			t.Errorf("event %d: actor %v, request %q, ip %q", i, e.ActorUserID, e.RequestID, e.IP)
		}
	}
}

type adminRecordingAuditor struct {
	mu     sync.Mutex
	events []auth.AuditEvent
}

func (a *adminRecordingAuditor) Record(_ context.Context, e auth.AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, e)
	return nil
}

// TestAdminAuditUsesAppAuditor: the admin records through the Auditor the
// app passed to framework.WithAuthAuditor, like the auth routes.
func TestAdminAuditUsesAppAuditor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := openSQLite(t)
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Widget{}); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.BcryptCost = bcrypt.MinCost
	cfg.Auth.Mode = config.AuthModeCookie
	rec := &adminRecordingAuditor{}
	app, err := framework.New(framework.WithConfig(cfg), framework.WithDatabase(db), framework.WithLogger(zap.NewNop()), framework.WithAuthAuditor(rec))
	if err != nil {
		t.Fatal(err)
	}
	registerWidgets(t, app)
	jar := loginSuperuser(t, app)
	if r := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/widgets", `{"name":"Sink","sku":"sink-1","price":1}`); r.Code != http.StatusOK {
		t.Fatalf("create = %d %s", r.Code, r.Body.String())
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var actions []auth.AuditAction
	for _, e := range rec.events {
		actions = append(actions, e.Action)
	}
	if len(actions) == 0 || actions[len(actions)-1] != auth.AuditAdminCreated {
		t.Fatalf("the app's auditor got %v, want the admin create last", actions)
	}
}
