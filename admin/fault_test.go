package admin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/framework"
	"github.com/gombit-dev/gombit/internal/faulttest"
)

// Fault tests for admin writes of a model with many-to-many fields: the
// base row and its join-table sync commit together or not at all.

// faultDBs are SQLite, plus PostgreSQL and MySQL under the integration tag.
var faultDBs = []faulttest.TestDB{faulttest.SQLiteDB()}

// m2mFixture is an admin app over a faulted database with the engines /
// warehouses resources, a logged-in superuser, and two warehouses.
type m2mFixture struct {
	db         *database.DB
	app        *framework.App
	jar        *cookieJar
	warehouses [2]int64
}

func newM2MFixture(t *testing.T, kind database.Driver, dsn string, faults *faulttest.DBFaults) m2mFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	faults.Disarm()
	db, err := faulttest.OpenDB(kind, dsn, faults)
	if err != nil {
		t.Fatal(err)
	}
	drop := func() {
		// Bounded: a transaction a failed test left open may hold the only
		// SQLite connection.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m := db.WithContext(ctx).Migrator()
		_ = m.DropTable("engine_warehouses", &relEngine{}, &relWarehouse{}, &Widget{})
		_ = m.DropTable("auth_user_permissions", "auth_user_groups", "auth_group_permissions")
		_ = m.DropTable(auth.Models()...)
	}
	t.Cleanup(func() {
		faults.Disarm()
		drop()
		_ = db.Close()
	})
	drop()
	app := newCookieAppWithDB(t, db)
	if err := db.AutoMigrate(&relWarehouse{}, &relEngine{}); err != nil {
		t.Fatal(err)
	}
	if err := admin.Register(app, relWarehouse{}, admin.Options{
		Slug: "warehouses",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "name", Type: admin.TypeString, Required: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := admin.Register(app, relEngine{}, admin.Options{
		Slug: "engines",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "name", Type: admin.TypeString, Required: true},
			{Name: "warehouses", Type: admin.TypeRelation, Related: &admin.Relation{
				Kind: admin.RelManyToMany, Slug: "warehouses", LabelField: "name",
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	jar := loginSuperuser(t, app)
	f := m2mFixture{db: db, app: app, jar: jar}
	f.warehouses[0] = createWarehouse(t, app, jar, "North")
	f.warehouses[1] = createWarehouse(t, app, jar, "South")
	faults.Arm()
	return f
}

func (f m2mFixture) createEngine() *httptest.ResponseRecorder {
	return doRequest(f.app, f.jar, http.MethodPost, "/api/v1/admin/resources/engines",
		fmt.Sprintf(`{"name":"V8","warehouses":[%d,%d]}`, f.warehouses[0], f.warehouses[1]))
}

// assertNoEngine fails t unless no engine and no join row persisted.
func (f m2mFixture) assertNoEngine(t *testing.T) {
	t.Helper()
	faulttest.Idle(t, f.db)
	var engines, links int64
	if err := f.app.DB().Model(&relEngine{}).Count(&engines).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.app.DB().Table("engine_warehouses").Count(&links).Error; err != nil {
		t.Fatal(err)
	}
	if engines != 0 || links != 0 {
		t.Fatalf("partial write after a failed create: %d engines, %d join rows; want none", engines, links)
	}
}

// TestFault_Database_AdminManyToManyRollback: the join-table insert fails
// after the engine row was inserted; the create fails and leaves neither.
func TestFault_Database_AdminManyToManyRollback(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		f := newM2MFixture(t, kind, dsn, &faulttest.DBFaults{
			Statement: faulttest.FailOnce(faulttest.ErrInjected),
			Match:     faulttest.Inserts("engine_warehouses"),
		})
		// An error response in the D10 envelope, not a recovered panic.
		assertError(t, f.createEngine(), http.StatusInternalServerError, "internal")
		f.assertNoEngine(t)
		if again := f.createEngine(); again.Code != http.StatusOK {
			t.Fatalf("the next create = %d, want 200; body: %s", again.Code, again.Body.String())
		}
	})
}

// TestFault_Database_AdminCommitFailure: a create whose commit fails is an
// error response, and nothing persists.
func TestFault_Database_AdminCommitFailure(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		f := newM2MFixture(t, kind, dsn, &faulttest.DBFaults{Commit: faulttest.FailOnce(faulttest.ErrInjected)})
		assertError(t, f.createEngine(), http.StatusInternalServerError, "internal")
		f.assertNoEngine(t)
	})
}

// selectsWidgets matches a SELECT from widgets (a load, a read-back).
func selectsWidgets(query string) bool {
	return strings.HasPrefix(strings.TrimSpace(strings.ToUpper(query)), "SELECT") && strings.Contains(query, "widgets")
}

// TestFault_Database_AdminAuditAfterCommit: an update commits, then the
// read-back for the response fails. The request fails, but the write is
// durable, and so is its audit event: the event is recorded where the write
// committed, not from the handler's final answer.
func TestFault_Database_AdminAuditAfterCommit(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		gin.SetMode(gin.TestMode)
		// Call 1 is the update's load; call 2 its read-back after the commit.
		faults := &faulttest.DBFaults{Statement: faulttest.FailOnCall(2, faulttest.ErrInjected), Match: selectsWidgets}
		faults.Disarm()
		db, err := faulttest.OpenDB(kind, dsn, faults)
		if err != nil {
			t.Fatal(err)
		}
		drop := func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			m := db.WithContext(ctx).Migrator()
			_ = m.DropTable(&Widget{})
			_ = m.DropTable("auth_user_permissions", "auth_user_groups", "auth_group_permissions")
			_ = m.DropTable(auth.Models()...)
		}
		t.Cleanup(func() {
			faults.Disarm()
			drop()
			_ = db.Close()
		})
		drop()
		app := newCookieAppWithDB(t, db)
		registerWidgets(t, app)
		jar := loginSuperuser(t, app)
		create := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/widgets", `{"name":"Committed","sku":"c-1","price":1}`)
		if create.Code != http.StatusOK {
			t.Fatalf("create = %d %s", create.Code, create.Body.String())
		}
		var created rowEnvelope
		if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprint(asInt(created.Data["id"]))

		faults.Arm()
		patch := doRequest(app, jar, http.MethodPatch, "/api/v1/admin/resources/widgets/"+id, `{"note":"after commit"}`)
		faults.Disarm()
		if patch.Code == http.StatusOK {
			t.Fatal("the PATCH succeeded: the read-back fault did not hit")
		}
		faulttest.Idle(t, db)
		var w Widget
		if err := db.First(&w, id).Error; err != nil || w.Note != "after commit" {
			t.Fatalf("the committed write: %+v, %v", w, err)
		}
		var events []auth.AuditEvent
		if err := db.Where("action = ? AND target_id = ?", auth.AuditAdminUpdated, id).Find(&events).Error; err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 {
			t.Fatalf("%d admin.record.updated events for a committed update whose response failed, want 1", len(events))
		}
	})
}
