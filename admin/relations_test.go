package admin_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/framework"
)

type relWarehouse struct {
	ID   uint   `gorm:"primaryKey" json:"id"`
	Name string `json:"name"`
}

func (relWarehouse) TableName() string { return "warehouses" }

type relEngine struct {
	ID         uint           `gorm:"primaryKey" json:"id"`
	Name       string         `json:"name"`
	Warehouses []relWarehouse `gorm:"many2many:engine_warehouses;" json:"warehouses"`
}

func (relEngine) TableName() string { return "engines" }

// idsOf pulls a []int64 out of a JSON relation field for order-independent
// comparison.
func idsOf(t *testing.T, v any) []int64 {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("field = %#v, want an array of ids", v)
	}
	out := make([]int64, 0, len(arr))
	for _, e := range arr {
		out = append(out, asInt(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func TestResourceManyToMany(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&relWarehouse{}, &relEngine{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := admin.Register(app, relWarehouse{}, admin.Options{
		Slug: "warehouses",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "name", Type: admin.TypeString, Required: true},
		},
	}); err != nil {
		t.Fatalf("Register warehouse: %v", err)
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
		t.Fatalf("Register engine: %v", err)
	}
	jar := loginSuperuser(t, app)

	w1 := createWarehouse(t, app, jar, "North")
	w2 := createWarehouse(t, app, jar, "South")

	// Create an engine with two warehouses.
	create := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/engines",
		fmt.Sprintf(`{"name":"V8","warehouses":[%d,%d]}`, w1, w2))
	if create.Code != http.StatusOK {
		t.Fatalf("create engine status = %d; body: %s", create.Code, create.Body.String())
	}
	var created rowEnvelope
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if got := idsOf(t, created.Data["warehouses"]); len(got) != 2 || got[0] != w1 || got[1] != w2 {
		t.Fatalf("created warehouses = %v, want [%d %d]", got, w1, w2)
	}
	engineID := asInt(created.Data["id"])
	path := fmt.Sprintf("/api/v1/admin/resources/engines/%d", engineID)

	// Read it back — the join table round-trips.
	get := doRequest(app, jar, http.MethodGet, path, "")
	var got rowEnvelope
	if err := json.Unmarshal(get.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if ids := idsOf(t, got.Data["warehouses"]); len(ids) != 2 {
		t.Fatalf("get warehouses = %v, want 2", ids)
	}

	// Shrink to one warehouse.
	patch := doRequest(app, jar, http.MethodPatch, path, fmt.Sprintf(`{"warehouses":[%d]}`, w1))
	if patch.Code != http.StatusOK {
		t.Fatalf("patch status = %d; body: %s", patch.Code, patch.Body.String())
	}
	var patched rowEnvelope
	_ = json.Unmarshal(patch.Body.Bytes(), &patched)
	if ids := idsOf(t, patched.Data["warehouses"]); len(ids) != 1 || ids[0] != w1 {
		t.Fatalf("patched warehouses = %v, want [%d]", ids, w1)
	}

	// A non-existent related id is a 422, and the scalar in the same PATCH must
	// NOT commit (persist + sync share a transaction).
	bad := doRequest(app, jar, http.MethodPatch, path, `{"name":"should-not-stick","warehouses":[999999]}`)
	assertError(t, bad, http.StatusUnprocessableEntity, "validation_error")
	var afterBad relEngine
	if err := app.DB().First(&afterBad, engineID).Error; err != nil {
		t.Fatalf("reload after bad patch: %v", err)
	}
	if afterBad.Name != "V8" {
		t.Fatalf("name = %q after failed patch, want unchanged V8 (scalar must roll back)", afterBad.Name)
	}

	// Omitting the relation on PATCH leaves it unchanged (partial update).
	rename := doRequest(app, jar, http.MethodPatch, path, `{"name":"V8-b"}`)
	if rename.Code != http.StatusOK {
		t.Fatalf("rename status = %d; body: %s", rename.Code, rename.Body.String())
	}
	var renamed rowEnvelope
	_ = json.Unmarshal(rename.Body.Bytes(), &renamed)
	if ids := idsOf(t, renamed.Data["warehouses"]); len(ids) != 1 || ids[0] != w1 {
		t.Fatalf("after rename warehouses = %v, want unchanged [%d]", ids, w1)
	}

	// Clearing to an empty list removes all join rows.
	clear := doRequest(app, jar, http.MethodPatch, path, `{"warehouses":[]}`)
	if clear.Code != http.StatusOK {
		t.Fatalf("clear status = %d; body: %s", clear.Code, clear.Body.String())
	}
	var cleared rowEnvelope
	_ = json.Unmarshal(clear.Body.Bytes(), &cleared)
	if ids := idsOf(t, cleared.Data["warehouses"]); len(ids) != 0 {
		t.Fatalf("cleared warehouses = %v, want empty", ids)
	}
}

// TestManyToManyCreateBadIDLeavesNoOrphan verifies that a POST naming a
// non-existent related id fails with 422 and does NOT insert the parent row
// (persist + join sync share one transaction).
func TestManyToManyCreateBadIDLeavesNoOrphan(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&relWarehouse{}, &relEngine{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := admin.Register(app, relWarehouse{}, admin.Options{
		Slug:   "warehouses",
		Fields: []admin.Field{{Name: "id", Type: admin.TypeInteger, ReadOnly: true}, {Name: "name", Type: admin.TypeString, Required: true}},
	}); err != nil {
		t.Fatalf("Register warehouse: %v", err)
	}
	if err := admin.Register(app, relEngine{}, admin.Options{
		Slug: "engines",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "name", Type: admin.TypeString, Required: true},
			{Name: "warehouses", Type: admin.TypeRelation, Related: &admin.Relation{Kind: admin.RelManyToMany, Slug: "warehouses", LabelField: "name"}},
		},
	}); err != nil {
		t.Fatalf("Register engine: %v", err)
	}
	jar := loginSuperuser(t, app)

	create := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/engines", `{"name":"Orphan","warehouses":[999999]}`)
	assertError(t, create, http.StatusUnprocessableEntity, "validation_error")

	var count int64
	if err := app.DB().Model(&relEngine{}).Where("name = ?", "Orphan").Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("engine count = %d, want 0 (a bad related id must not leave an orphan parent)", count)
	}
}

// TestManyToManyRejectsRequired verifies Register refuses a Required m2m field,
// which applyWrite's required check can never see (the id list is split out).
func TestManyToManyRejectsRequired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&relWarehouse{}, &relEngine{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	err := admin.Register(app, relEngine{}, admin.Options{
		Slug: "engines",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "name", Type: admin.TypeString, Required: true},
			{Name: "warehouses", Type: admin.TypeRelation, Required: true, Related: &admin.Relation{Kind: admin.RelManyToMany, Slug: "warehouses"}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be Required") {
		t.Fatalf("Register error = %v, want a many_to_many-cannot-be-Required rejection", err)
	}
}

type relVersionedEngine struct {
	ID         uint           `gorm:"primaryKey" json:"id"`
	Name       string         `json:"name"`
	Version    int            `json:"version"`
	Warehouses []relWarehouse `gorm:"many2many:versioned_engine_warehouses;" json:"warehouses"`
}

func (relVersionedEngine) TableName() string { return "versioned_engines" }

// TestManyToManyWithVersionRejectedAtRegister guards the merge regression the
// review caught: a model with both a version column and m2m fields would route
// to the version path and silently drop the m2m write. Registration must refuse
// the combination instead of accepting a write it discards.
func TestManyToManyWithVersionRejectedAtRegister(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&relWarehouse{}, &relVersionedEngine{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	err := admin.Register(app, relVersionedEngine{}, admin.Options{
		Slug: "versioned-engines",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "name", Type: admin.TypeString, Required: true},
			{Name: "version", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "warehouses", Type: admin.TypeRelation, Related: &admin.Relation{Kind: admin.RelManyToMany, Slug: "warehouses"}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "version column and many_to_many") {
		t.Fatalf("Register error = %v, want rejection of version + m2m combination", err)
	}
}

// TestManyToManyReadOnlyRejectsWrite guards that a read-only m2m field cannot be
// written: the id list is split out before applyWrite, so the read-only 422 must
// be enforced in splitM2M (meta and the SPA already honor readonly).
func TestManyToManyReadOnlyRejectsWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&relWarehouse{}, &relEngine{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := admin.Register(app, relWarehouse{}, admin.Options{
		Slug:   "warehouses",
		Fields: []admin.Field{{Name: "id", Type: admin.TypeInteger, ReadOnly: true}, {Name: "name", Type: admin.TypeString, Required: true}},
	}); err != nil {
		t.Fatalf("Register warehouse: %v", err)
	}
	if err := admin.Register(app, relEngine{}, admin.Options{
		Slug: "engines",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "name", Type: admin.TypeString, Required: true},
			{Name: "warehouses", Type: admin.TypeRelation, ReadOnly: true, Related: &admin.Relation{Kind: admin.RelManyToMany, Slug: "warehouses"}},
		},
	}); err != nil {
		t.Fatalf("Register engine: %v", err)
	}
	jar := loginSuperuser(t, app)
	w1 := createWarehouse(t, app, jar, "North")

	create := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/engines", `{"name":"V8"}`)
	if create.Code != http.StatusOK {
		t.Fatalf("create engine status = %d; body: %s", create.Code, create.Body.String())
	}
	var created rowEnvelope
	_ = json.Unmarshal(create.Body.Bytes(), &created)
	engineID := asInt(created.Data["id"])
	path := fmt.Sprintf("/api/v1/admin/resources/engines/%d", engineID)

	patch := doRequest(app, jar, http.MethodPatch, path, fmt.Sprintf(`{"warehouses":[%d]}`, w1))
	assertError(t, patch, http.StatusUnprocessableEntity, "validation_error")

	// The read-only write must not have applied.
	var count int64
	if err := app.DB().Table("engine_warehouses").Count(&count).Error; err != nil {
		t.Fatalf("count join rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("join rows = %d, want 0 (read-only m2m must not write)", count)
	}
}

// TestAutoDerivedModelIsSearchable verifies a purely auto-registered model gets
// a default Search over its text columns, so the list endpoint (which the
// relation picker uses for server-side search) can filter by name.
func TestAutoDerivedModelIsSearchable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&relWarehouse{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := admin.Register(app, relWarehouse{}, admin.Options{Slug: "warehouses"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	jar := loginSuperuser(t, app)
	createWarehouse(t, app, jar, "North")
	createWarehouse(t, app, jar, "South")

	// Meta advertises the default search field.
	meta := doRequest(app, jar, http.MethodGet, "/api/v1/admin/meta/warehouses", "")
	if !strings.Contains(meta.Body.String(), `"search":["name"]`) {
		t.Fatalf("warehouses meta missing default search on name: %s", meta.Body.String())
	}

	list := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/warehouses?search=North", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d; body: %s", list.Code, list.Body.String())
	}
	var env listEnvelope
	if err := json.Unmarshal(list.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(env.Data) != 1 || fmt.Sprint(env.Data[0]["name"]) != "North" {
		t.Fatalf("search=North returned %v, want exactly [North]", env.Data)
	}
}

// TestExplicitFieldsModelIsSearchable verifies the documented registration path
// (explicit Fields, no Search) also gets the default Search — the picker's
// server-side search reaches past the first page there too, not only for
// auto-derived models.
func TestExplicitFieldsModelIsSearchable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&relWarehouse{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := admin.Register(app, relWarehouse{}, admin.Options{
		Slug: "warehouses",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "name", Type: admin.TypeString, Required: true},
		},
		// No Search set.
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	jar := loginSuperuser(t, app)
	createWarehouse(t, app, jar, "North")
	createWarehouse(t, app, jar, "South")

	list := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/warehouses?search=South", "")
	var env listEnvelope
	if err := json.Unmarshal(list.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(env.Data) != 1 || fmt.Sprint(env.Data[0]["name"]) != "South" {
		t.Fatalf("search=South returned %v, want exactly [South] (explicit-Fields model must be searchable)", env.Data)
	}
}

// TestEmptySearchOptsOut verifies an explicit empty (non-nil) Search disables
// search — the picker then filters client-side only.
func TestEmptySearchOptsOut(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&relWarehouse{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := admin.Register(app, relWarehouse{}, admin.Options{
		Slug:   "warehouses",
		Search: []string{}, // explicit opt-out
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	jar := loginSuperuser(t, app)
	createWarehouse(t, app, jar, "North")
	createWarehouse(t, app, jar, "South")

	list := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/warehouses?search=South", "")
	var env listEnvelope
	if err := json.Unmarshal(list.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(env.Data) != 2 {
		t.Fatalf("with empty Search, search=South returned %d rows, want 2 (search opted out)", len(env.Data))
	}
}

type hmPart struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	Name      string `json:"name"`
	MachineID uint   `json:"machine_id"`
}

func (hmPart) TableName() string { return "hm_parts" }

type hmMachine struct {
	ID    uint     `gorm:"primaryKey" json:"id"`
	Name  string   `json:"name"`
	Parts []hmPart `gorm:"foreignKey:MachineID" json:"parts"`
}

func (hmMachine) TableName() string { return "hm_machines" }

// TestHasManyReadOnlyView verifies a has_many association auto-derives to a
// read-only relation whose data-plane read returns the related children's ids
// (preloaded), and that a write to it is rejected.
func TestHasManyReadOnlyView(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// Auto-derivation emits a read-only has_many relation.
	derived, err := admin.FieldsFrom(hmMachine{})
	if err != nil {
		t.Fatalf("FieldsFrom: %v", err)
	}
	var parts *admin.Field
	for i := range derived {
		if derived[i].Name == "parts" {
			parts = &derived[i]
		}
	}
	if parts == nil || parts.Type != admin.TypeRelation || parts.Related == nil ||
		parts.Related.Kind != admin.RelHasMany || !parts.ReadOnly {
		t.Fatalf("parts field = %+v, want a read-only has_many relation", parts)
	}

	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&hmPart{}, &hmMachine{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := admin.Register(app, hmMachine{}, admin.Options{Slug: "machines"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	jar := loginSuperuser(t, app)

	machine := hmMachine{Name: "Lathe"}
	if err := app.DB().Create(&machine).Error; err != nil {
		t.Fatalf("create machine: %v", err)
	}
	children := []hmPart{
		{Name: "Chuck", MachineID: machine.ID},
		{Name: "Bed", MachineID: machine.ID},
	}
	if err := app.DB().Create(&children).Error; err != nil {
		t.Fatalf("create parts: %v", err)
	}
	want := []int64{asInt(children[0].ID), asInt(children[1].ID)}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })

	path := fmt.Sprintf("/api/v1/admin/resources/machines/%d", machine.ID)
	get := doRequest(app, jar, http.MethodGet, path, "")
	if get.Code != http.StatusOK {
		t.Fatalf("get status = %d; body: %s", get.Code, get.Body.String())
	}
	var got rowEnvelope
	if err := json.Unmarshal(get.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if ids := idsOf(t, got.Data["parts"]); !reflect.DeepEqual(ids, want) {
		t.Fatalf("detail parts = %v, want the created child PKs %v", ids, want)
	}

	// The list endpoint returns the same child ids.
	list := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/machines", "")
	var listed listEnvelope
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed.Data) != 1 {
		t.Fatalf("list returned %d machines, want 1", len(listed.Data))
	}
	if ids := idsOf(t, listed.Data[0]["parts"]); !reflect.DeepEqual(ids, want) {
		t.Fatalf("list parts = %v, want %v", ids, want)
	}

	// A write to the read-only has_many field is rejected with a field error.
	patch := doRequest(app, jar, http.MethodPatch, path, fmt.Sprintf(`{"parts":[%d]}`, want[0]))
	assertError(t, patch, http.StatusUnprocessableEntity, "validation_error")
	if fields := decodeError(t, patch).Fields; len(fields["parts"]) == 0 {
		t.Fatalf("error fields = %#v, want a parts error", fields)
	}

	// The rejected write must not have mutated the relation.
	get2 := doRequest(app, jar, http.MethodGet, path, "")
	var after rowEnvelope
	if err := json.Unmarshal(get2.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode re-get: %v", err)
	}
	if ids := idsOf(t, after.Data["parts"]); !reflect.DeepEqual(ids, want) {
		t.Fatalf("parts after rejected write = %v, want unchanged %v", ids, want)
	}
}

func createWarehouse(t *testing.T, app *framework.App, jar *cookieJar, name string) int64 {
	t.Helper()
	rec := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/warehouses",
		fmt.Sprintf(`{"name":%q}`, name))
	if rec.Code != http.StatusOK {
		t.Fatalf("create warehouse status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var env rowEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode warehouse: %v", err)
	}
	return asInt(env.Data["id"])
}

type belongsEngine struct {
	ID          uint         `gorm:"primaryKey" json:"id"`
	Name        string       `json:"name"`
	WarehouseID uint         `json:"warehouse_id"`
	Warehouse   relWarehouse `json:"-"`
}

func (belongsEngine) TableName() string { return "belongs_engines" }

// TestBelongsToAutoDerivation verifies FieldsFrom renders a belongs_to foreign
// key as a relation (a picker), not a bare integer, with the target slug and a
// label field (#223).
func TestBelongsToAutoDerivation(t *testing.T) {
	fields, err := admin.FieldsFrom(belongsEngine{})
	if err != nil {
		t.Fatalf("FieldsFrom: %v", err)
	}
	var fk *admin.Field
	for i := range fields {
		if fields[i].Name == "warehouse_id" {
			fk = &fields[i]
		}
	}
	if fk == nil {
		t.Fatalf("derived fields %v missing warehouse_id", fields)
	}
	if fk.Type != admin.TypeRelation || fk.Related == nil || fk.Related.Kind != admin.RelBelongsTo {
		t.Fatalf("warehouse_id = %+v, want belongs_to relation", fk)
	}
	if fk.Related.Slug != "warehouses" {
		t.Fatalf("warehouse_id slug = %q, want warehouses", fk.Related.Slug)
	}
	if fk.Related.LabelField != "name" {
		t.Fatalf("warehouse_id label = %q, want name", fk.Related.LabelField)
	}
}

type labelWarehouse struct {
	ID   uint   `gorm:"primaryKey" json:"id"`
	Name string `json:"title"` // JSON key deliberately differs from the column
}

func (labelWarehouse) TableName() string { return "label_warehouses" }

type labelEngine struct {
	ID          uint           `gorm:"primaryKey" json:"id"`
	WarehouseID uint           `json:"warehouse_id"`
	Warehouse   labelWarehouse `json:"-"`
}

func (labelEngine) TableName() string { return "label_engines" }

// TestBelongsToLabelIsFieldName verifies label_field is the related model's JSON
// field name (what the SPA indexes on the list row), not the SQL column name.
func TestBelongsToLabelIsFieldName(t *testing.T) {
	fields, err := admin.FieldsFrom(labelEngine{})
	if err != nil {
		t.Fatalf("FieldsFrom: %v", err)
	}
	for i := range fields {
		if fields[i].Name == "warehouse_id" {
			got := ""
			if fields[i].Related != nil {
				got = fields[i].Related.LabelField
			}
			if got != "title" {
				t.Fatalf("label_field = %q, want the json field name \"title\", not the column \"name\"", got)
			}
			return
		}
	}
	t.Fatalf("derived fields %v missing warehouse_id", fields)
}

type optBelongsEngine struct {
	ID          uint         `gorm:"primaryKey" json:"id"`
	Name        string       `json:"name"`
	WarehouseID *uint        `gorm:"index" json:"warehouse_id"`
	Warehouse   relWarehouse `json:"-"`
}

func (optBelongsEngine) TableName() string { return "opt_belongs_engines" }

// TestBelongsToOptionalNullable verifies that a belongs_to backed by a nullable
// (*uint) foreign key is genuinely optional: FieldsFrom marks it not-required,
// a create omitting it succeeds and reads back null (not rejected by the FK),
// and a create naming a real target sets it. This is the contract the admin
// advertises (belongs_to required=false); a non-nullable uint FK would reject
// "no parent" under foreign key enforcement. See #223 / the #238 review.
func TestBelongsToOptionalNullable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&relWarehouse{}, &optBelongsEngine{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := admin.Register(app, relWarehouse{}, admin.Options{Slug: "warehouses"}); err != nil {
		t.Fatalf("Register warehouse: %v", err)
	}
	if err := admin.Register(app, optBelongsEngine{}, admin.Options{Slug: "engines"}); err != nil {
		t.Fatalf("Register engine: %v", err)
	}

	// Derivation marks the nullable FK optional.
	fields, err := admin.FieldsFrom(optBelongsEngine{})
	if err != nil {
		t.Fatalf("FieldsFrom: %v", err)
	}
	var fk *admin.Field
	for i := range fields {
		if fields[i].Name == "warehouse_id" {
			fk = &fields[i]
		}
	}
	if fk == nil || fk.Related == nil || fk.Related.Kind != admin.RelBelongsTo {
		t.Fatalf("warehouse_id = %+v, want belongs_to relation", fk)
	}
	if fk.Required {
		t.Fatalf("nullable belongs_to FK must not be Required")
	}

	jar := loginSuperuser(t, app)

	// Create with no warehouse: must succeed and read back null.
	create := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/engines", `{"name":"Loose"}`)
	if create.Code != http.StatusOK {
		t.Fatalf("create without FK status = %d; body: %s (optional belongs_to must accept none)", create.Code, create.Body.String())
	}
	var created rowEnvelope
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if v, ok := created.Data["warehouse_id"]; !ok || v != nil {
		t.Fatalf("warehouse_id = %#v, want null on a create that omits it", created.Data["warehouse_id"])
	}

	// Create naming a real warehouse: FK round-trips.
	w := createWarehouse(t, app, jar, "North")
	set := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/engines",
		fmt.Sprintf(`{"name":"Bolt","warehouse_id":%d}`, w))
	if set.Code != http.StatusOK {
		t.Fatalf("create with FK status = %d; body: %s", set.Code, set.Body.String())
	}
	var withFK rowEnvelope
	if err := json.Unmarshal(set.Body.Bytes(), &withFK); err != nil {
		t.Fatalf("decode create-with-fk: %v", err)
	}
	if got := asInt(withFK.Data["warehouse_id"]); got != w {
		t.Fatalf("warehouse_id = %d, want %d", got, w)
	}
}

// TestBelongsToAutoDerivationRoundTrip exercises the full picker contract: both
// models are registered with auto-derived fields (empty Fields), the belongs_to
// FK is written through the derived relation field, and it reads back — plus the
// meta advertises the relation with its target slug and label field.
func TestBelongsToAutoDerivationRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&relWarehouse{}, &belongsEngine{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	// Empty Fields → FieldsFrom auto-derivation.
	if err := admin.Register(app, relWarehouse{}, admin.Options{Slug: "warehouses"}); err != nil {
		t.Fatalf("Register warehouse: %v", err)
	}
	if err := admin.Register(app, belongsEngine{}, admin.Options{Slug: "engines"}); err != nil {
		t.Fatalf("Register engine: %v", err)
	}
	jar := loginSuperuser(t, app)

	// Meta advertises the belongs_to relation.
	meta := doRequest(app, jar, http.MethodGet, "/api/v1/admin/meta/engines", "")
	if meta.Code != http.StatusOK {
		t.Fatalf("meta status = %d; body: %s", meta.Code, meta.Body.String())
	}
	if body := meta.Body.String(); !strings.Contains(body, `"kind":"belongs_to"`) ||
		!strings.Contains(body, `"slug":"warehouses"`) || !strings.Contains(body, `"label_field":"name"`) {
		t.Fatalf("engines meta missing belongs_to relation with slug+label: %s", body)
	}

	w1 := createWarehouse(t, app, jar, "North")

	// Write the FK through the derived relation field, then read it back.
	create := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/engines",
		fmt.Sprintf(`{"name":"V8","warehouse_id":%d}`, w1))
	if create.Code != http.StatusOK {
		t.Fatalf("create engine status = %d; body: %s", create.Code, create.Body.String())
	}
	var created rowEnvelope
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if got := asInt(created.Data["warehouse_id"]); got != w1 {
		t.Fatalf("created warehouse_id = %d, want %d", got, w1)
	}
	engineID := asInt(created.Data["id"])

	get := doRequest(app, jar, http.MethodGet, fmt.Sprintf("/api/v1/admin/resources/engines/%d", engineID), "")
	var got rowEnvelope
	if err := json.Unmarshal(get.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if id := asInt(got.Data["warehouse_id"]); id != w1 {
		t.Fatalf("get warehouse_id = %d, want %d (FK must round-trip)", id, w1)
	}
}

// TestManyToManyAutoDerivation verifies FieldsFrom emits a relation field for a
// many-to-many association instead of dropping it (#221/#223).
func TestManyToManyAutoDerivation(t *testing.T) {
	fields, err := admin.FieldsFrom(relEngine{})
	if err != nil {
		t.Fatalf("FieldsFrom: %v", err)
	}
	var found *admin.Field
	for i := range fields {
		if fields[i].Name == "warehouses" {
			found = &fields[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("derived fields %v missing warehouses relation", fields)
	}
	if found.Type != admin.TypeRelation || found.Related == nil || found.Related.Kind != admin.RelManyToMany {
		t.Fatalf("warehouses field = %+v, want many_to_many relation", found)
	}
	if found.Related.Slug != "warehouses" {
		t.Fatalf("warehouses slug = %q, want warehouses (related table)", found.Related.Slug)
	}
	// The multi-select needs a human label, like belongs_to and has_many — not a
	// blank that renders raw ids.
	if found.Related.LabelField != "name" {
		t.Fatalf("warehouses label_field = %q, want name (auto-derived, consistent with belongs_to/has_many)", found.Related.LabelField)
	}
}
