//go:build integration

package admin_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"testing"

	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/framework"
	"github.com/gin-gonic/gin"
)

var (
	postgresDSN = flag.String("admin.postgres-dsn", "", "PostgreSQL DSN for admin integration tests")
	mysqlDSN    = flag.String("admin.mysql-dsn", "", "MySQL DSN for admin integration tests")
)

func TestResourcePostgres(t *testing.T) {
	if *postgresDSN == "" {
		t.Skip("set -admin.postgres-dsn to run Postgres admin integration tests")
	}
	runResourceDriver(t, config.DatabaseDriverPostgres, *postgresDSN)
}

func TestResourceMySQL(t *testing.T) {
	if *mysqlDSN == "" {
		t.Skip("set -admin.mysql-dsn to run MySQL admin integration tests")
	}
	runResourceDriver(t, config.DatabaseDriverMySQL, *mysqlDSN)
}

func runResourceDriver(t *testing.T, driver config.DatabaseDriver, dsn string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := openAdminDriver(t, driver, dsn)
	app := newCookieAppWithDB(t, db)
	registerWidgets(t, app)
	jar := loginSuperuser(t, app)

	create := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/widgets", `{"name":"Bolt","sku":"b-1","price":5}`)
	if create.Code != http.StatusOK {
		t.Fatalf("create status = %d; body: %s", create.Code, create.Body.String())
	}
	var created rowEnvelope
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	id := fmt.Sprint(asInt(created.Data["id"]))

	list := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/widgets?search=Bolt&ordering=name", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d; body: %s", list.Code, list.Body.String())
	}
	del := doRequest(app, jar, http.MethodDelete, "/api/v1/admin/resources/widgets/"+id, "")
	if del.Code != http.StatusOK {
		t.Fatalf("delete status = %d; body: %s", del.Code, del.Body.String())
	}

	email := "integration-viewer@example.com"
	viewerJar := loginUser(t, app, email, testPassword)
	grantGroupPermission(t, app, email, "admin.widgets.view")
	viewOnly := doRequest(app, viewerJar, http.MethodGet, "/api/v1/admin/resources/widgets", "")
	if viewOnly.Code != http.StatusOK {
		t.Fatalf("view-only list status = %d; body: %s", viewOnly.Code, viewOnly.Body.String())
	}
	denied := doRequest(app, viewerJar, http.MethodPost, "/api/v1/admin/resources/widgets", `{"name":"Denied"}`)
	assertError(t, denied, http.StatusForbidden, "authorization")
}

func openAdminDriver(t *testing.T, driver config.DatabaseDriver, dsn string) *database.DB {
	t.Helper()
	db, err := database.Open(config.DatabaseConfig{Driver: driver, DSN: dsn})
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() {
		_ = db.Migrator().DropTable(&Widget{})
		_ = db.Migrator().DropTable("auth_user_permissions", "auth_user_groups", "auth_group_permissions")
		_ = db.Migrator().DropTable(auth.Models()...)
		_ = db.Close()
	})
	return db
}

// TestRelationsPostgres / TestRelationsMySQL run the belongs_to / has_many /
// many_to_many admin round-trips (join-table sync, FK write, has_many preload)
// against a real Postgres / MySQL, complementing the SQLite coverage in
// relations_test.go. These paths emit engine-specific SQL, so the matrix is the
// point (working agreement §5.1).
func TestRelationsPostgres(t *testing.T) {
	if *postgresDSN == "" {
		t.Skip("set -admin.postgres-dsn to run Postgres admin integration tests")
	}
	runRelationsDriver(t, config.DatabaseDriverPostgres, *postgresDSN)
}

func TestRelationsMySQL(t *testing.T) {
	if *mysqlDSN == "" {
		t.Skip("set -admin.mysql-dsn to run MySQL admin integration tests")
	}
	runRelationsDriver(t, config.DatabaseDriverMySQL, *mysqlDSN)
}

func runRelationsDriver(t *testing.T, driver config.DatabaseDriver, dsn string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := openAdminDriver(t, driver, dsn)
	// Drop the relation tables (join and children before parents) after the run;
	// registered after openAdminDriver's cleanup, so it runs first (LIFO).
	t.Cleanup(func() {
		_ = db.Migrator().DropTable("engine_warehouses")
		_ = db.Migrator().DropTable(&hmPart{}, &relEngine{}, &optBelongsEngine{}, &hmMachine{}, &relWarehouse{})
	})
	if err := db.AutoMigrate(&relWarehouse{}, &relEngine{}, &optBelongsEngine{}, &hmMachine{}, &hmPart{}); err != nil {
		t.Fatalf("AutoMigrate relations: %v", err)
	}
	app := newCookieAppWithDB(t, db)
	mustRegister := func(model any, opts admin.Options) {
		if err := admin.Register(app, model, opts); err != nil {
			t.Fatalf("Register %s: %v", opts.Slug, err)
		}
	}
	mustRegister(relWarehouse{}, admin.Options{Slug: "warehouses"})
	mustRegister(relEngine{}, admin.Options{Slug: "engines", Fields: []admin.Field{
		{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
		{Name: "name", Type: admin.TypeString, Required: true},
		{Name: "warehouses", Type: admin.TypeRelation, Related: &admin.Relation{
			Kind: admin.RelManyToMany, Slug: "warehouses", LabelField: "name",
		}},
	}})
	mustRegister(optBelongsEngine{}, admin.Options{Slug: "belongs-engines"})
	mustRegister(hmMachine{}, admin.Options{Slug: "machines"})
	jar := loginSuperuser(t, app)

	assertManyToManyRoundTrip(t, app, jar)
	assertBelongsToRoundTrip(t, app, jar)
	assertHasManyRead(t, app, jar)
}

// assertManyToManyRoundTrip drives create-with-ids, read-back, shrink, the
// bad-id 422 that must roll back the whole write (persist + join sync share one
// transaction), and clear-to-empty — the DB-touching join-table path.
func assertManyToManyRoundTrip(t *testing.T, app *framework.App, jar *cookieJar) {
	t.Helper()
	w1 := createWarehouse(t, app, jar, "North")
	w2 := createWarehouse(t, app, jar, "South")

	create := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/engines",
		fmt.Sprintf(`{"name":"V8","warehouses":[%d,%d]}`, w1, w2))
	if create.Code != http.StatusOK {
		t.Fatalf("m2m create status = %d; body: %s", create.Code, create.Body.String())
	}
	var created rowEnvelope
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode m2m create: %v", err)
	}
	engineID := asInt(created.Data["id"])
	path := fmt.Sprintf("/api/v1/admin/resources/engines/%d", engineID)
	if ids := idsOf(t, created.Data["warehouses"]); len(ids) != 2 {
		t.Fatalf("created warehouses = %v, want 2", ids)
	}

	get := doRequest(app, jar, http.MethodGet, path, "")
	var got rowEnvelope
	if err := json.Unmarshal(get.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode m2m get: %v", err)
	}
	if ids := idsOf(t, got.Data["warehouses"]); len(ids) != 2 {
		t.Fatalf("get warehouses = %v, want 2 (join round-trip)", ids)
	}

	patch := doRequest(app, jar, http.MethodPatch, path, fmt.Sprintf(`{"warehouses":[%d]}`, w1))
	var patched rowEnvelope
	_ = json.Unmarshal(patch.Body.Bytes(), &patched)
	if ids := idsOf(t, patched.Data["warehouses"]); len(ids) != 1 || ids[0] != w1 {
		t.Fatalf("patched warehouses = %v, want [%d]", ids, w1)
	}

	bad := doRequest(app, jar, http.MethodPatch, path, `{"name":"should-not-stick","warehouses":[999999]}`)
	assertError(t, bad, http.StatusUnprocessableEntity, "validation_error")
	var afterBad relEngine
	if err := app.DB().First(&afterBad, engineID).Error; err != nil {
		t.Fatalf("reload after bad patch: %v", err)
	}
	if afterBad.Name != "V8" {
		t.Fatalf("name = %q after failed patch, want unchanged V8 (scalar must roll back with the join sync)", afterBad.Name)
	}

	clear := doRequest(app, jar, http.MethodPatch, path, `{"warehouses":[]}`)
	var cleared rowEnvelope
	_ = json.Unmarshal(clear.Body.Bytes(), &cleared)
	if ids := idsOf(t, cleared.Data["warehouses"]); len(ids) != 0 {
		t.Fatalf("cleared warehouses = %v, want empty", ids)
	}
}

// assertBelongsToRoundTrip writes a belongs_to FK through the derived relation
// field and reads it back, and checks that an omitted (nullable) FK is accepted
// and stored as null — the case where DB foreign-key enforcement differs and a
// non-nullable uint FK would reject "no parent".
func assertBelongsToRoundTrip(t *testing.T, app *framework.App, jar *cookieJar) {
	t.Helper()

	// Optional belongs_to: omitting the FK must succeed and read back null.
	none := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/belongs-engines", `{"name":"Loose"}`)
	if none.Code != http.StatusOK {
		t.Fatalf("create without FK status = %d; body: %s (optional belongs_to must accept none under FK enforcement)", none.Code, none.Body.String())
	}
	var noneEnv rowEnvelope
	if err := json.Unmarshal(none.Body.Bytes(), &noneEnv); err != nil {
		t.Fatalf("decode create-without-fk: %v", err)
	}
	if v, ok := noneEnv.Data["warehouse_id"]; !ok || v != nil {
		t.Fatalf("warehouse_id = %#v, want null when omitted", noneEnv.Data["warehouse_id"])
	}

	w := createWarehouse(t, app, jar, "Depot")
	create := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/belongs-engines",
		fmt.Sprintf(`{"name":"I4","warehouse_id":%d}`, w))
	if create.Code != http.StatusOK {
		t.Fatalf("belongs_to create status = %d; body: %s", create.Code, create.Body.String())
	}
	var created rowEnvelope
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode belongs_to create: %v", err)
	}
	if got := asInt(created.Data["warehouse_id"]); got != w {
		t.Fatalf("created warehouse_id = %d, want %d", got, w)
	}
	id := asInt(created.Data["id"])
	get := doRequest(app, jar, http.MethodGet, fmt.Sprintf("/api/v1/admin/resources/belongs-engines/%d", id), "")
	var got rowEnvelope
	if err := json.Unmarshal(get.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode belongs_to get: %v", err)
	}
	if id := asInt(got.Data["warehouse_id"]); id != w {
		t.Fatalf("get warehouse_id = %d, want %d (FK must round-trip)", id, w)
	}
}

// assertHasManyRead seeds a parent with children and checks the read-only
// has_many preload returns the child ids, and that a write to it is rejected.
func assertHasManyRead(t *testing.T, app *framework.App, jar *cookieJar) {
	t.Helper()
	machine := hmMachine{Name: "Lathe"}
	if err := app.DB().Create(&machine).Error; err != nil {
		t.Fatalf("create machine: %v", err)
	}
	children := []hmPart{{Name: "Chuck", MachineID: machine.ID}, {Name: "Bed", MachineID: machine.ID}}
	if err := app.DB().Create(&children).Error; err != nil {
		t.Fatalf("create parts: %v", err)
	}
	want := []int64{asInt(children[0].ID), asInt(children[1].ID)}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })

	path := fmt.Sprintf("/api/v1/admin/resources/machines/%d", machine.ID)
	get := doRequest(app, jar, http.MethodGet, path, "")
	var got rowEnvelope
	if err := json.Unmarshal(get.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode machine get: %v", err)
	}
	if ids := idsOf(t, got.Data["parts"]); !reflect.DeepEqual(ids, want) {
		t.Fatalf("detail parts = %v, want preloaded child PKs %v", ids, want)
	}
	patch := doRequest(app, jar, http.MethodPatch, path, fmt.Sprintf(`{"parts":[%d]}`, want[0]))
	assertError(t, patch, http.StatusUnprocessableEntity, "validation_error")
}
