package admin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/framework"
	"github.com/google/uuid"
)

type rowEnvelope struct {
	Data map[string]any `json:"data"`
}

type listEnvelope struct {
	Data []map[string]any   `json:"data"`
	Meta *contract.PageMeta `json:"meta"`
}

func TestResourceCRUDAndAuthz(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	registerWidgets(t, app)
	jar := loginSuperuser(t, app)

	create := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/widgets", `{"name":"Alpha","sku":"a-1","price":10}`)
	if create.Code != http.StatusOK {
		t.Fatalf("create status = %d; body: %s", create.Code, create.Body.String())
	}
	var created rowEnvelope
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.Data["name"] != "Alpha" {
		t.Fatalf("created name = %#v", created.Data["name"])
	}
	id := fmt.Sprint(asInt(created.Data["id"]))

	got := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/widgets/"+id, "")
	if got.Code != http.StatusOK {
		t.Fatalf("detail status = %d; body: %s", got.Code, got.Body.String())
	}

	patch := doRequest(app, jar, http.MethodPatch, "/api/v1/admin/resources/widgets/"+id, `{"note":"updated"}`)
	if patch.Code != http.StatusOK {
		t.Fatalf("patch status = %d; body: %s", patch.Code, patch.Body.String())
	}
	var updated rowEnvelope
	if err := json.Unmarshal(patch.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if updated.Data["note"] != "updated" {
		t.Fatalf("note = %#v", updated.Data["note"])
	}

	list := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/widgets", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d; body: %s", list.Code, list.Body.String())
	}
	var listed listEnvelope
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if listed.Meta == nil || listed.Meta.Total != 1 {
		t.Fatalf("list meta = %+v", listed.Meta)
	}

	del := doRequest(app, jar, http.MethodDelete, "/api/v1/admin/resources/widgets/"+id, "")
	if del.Code != http.StatusOK {
		t.Fatalf("delete status = %d; body: %s", del.Code, del.Body.String())
	}
	missing := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/widgets/"+id, "")
	assertError(t, missing, http.StatusNotFound, "not_found")
}

func TestResourcePatchClearsOptionalFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	registerWidgets(t, app)
	jar := loginSuperuser(t, app)

	create := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/widgets",
		`{"name":"Alpha","sku":"keep-me","price":10,"note":"hello"}`)
	if create.Code != http.StatusOK {
		t.Fatalf("create status = %d; body: %s", create.Code, create.Body.String())
	}
	var created rowEnvelope
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	id := fmt.Sprint(asInt(created.Data["id"]))
	path := "/api/v1/admin/resources/widgets/" + id

	omit := doRequest(app, jar, http.MethodPatch, path, `{"price":99}`)
	if omit.Code != http.StatusOK {
		t.Fatalf("omit-key patch status = %d; body: %s", omit.Code, omit.Body.String())
	}
	assertStoredWidget(t, app, created.Data["id"], Widget{Name: "Alpha", SKU: "keep-me", Price: 99, Note: "hello"})

	clearNull := doRequest(app, jar, http.MethodPatch, path, `{"note":null}`)
	if clearNull.Code != http.StatusOK {
		t.Fatalf("null patch status = %d; body: %s", clearNull.Code, clearNull.Body.String())
	}
	assertStoredWidget(t, app, created.Data["id"], Widget{Name: "Alpha", SKU: "keep-me", Price: 99, Note: ""})

	setNote := doRequest(app, jar, http.MethodPatch, path, `{"note":"again"}`)
	if setNote.Code != http.StatusOK {
		t.Fatalf("restore patch status = %d; body: %s", setNote.Code, setNote.Body.String())
	}
	clearEmpty := doRequest(app, jar, http.MethodPatch, path, `{"note":""}`)
	if clearEmpty.Code != http.StatusOK {
		t.Fatalf("empty-string patch status = %d; body: %s", clearEmpty.Code, clearEmpty.Body.String())
	}
	assertStoredWidget(t, app, created.Data["id"], Widget{Name: "Alpha", SKU: "keep-me", Price: 99, Note: ""})

	required := doRequest(app, jar, http.MethodPatch, path, `{"name":null}`)
	assertError(t, required, http.StatusUnprocessableEntity, contract.CodeValidationError)
	env := decodeError(t, required)
	if len(env.Fields["name"]) == 0 {
		t.Fatalf("fields.name missing; %#v", env.Fields)
	}
	assertStoredWidget(t, app, created.Data["id"], Widget{Name: "Alpha", SKU: "keep-me", Price: 99, Note: ""})
}

func TestResourcePatchClearsNullablePointerAndJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	type Doc struct {
		ID      uint            `gorm:"primaryKey" json:"id"`
		Title   string          `json:"title"`
		Note    *string         `json:"note"`
		Payload json.RawMessage `json:"payload"`
		Due     *time.Time      `json:"due"`
	}
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&Doc{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := admin.Register(app, Doc{}, admin.Options{
		Slug: "docs",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "title", Type: admin.TypeString, Required: true},
			{Name: "note", Type: admin.TypeText},
			{Name: "payload", Type: admin.TypeJSON},
			{Name: "due", Type: admin.TypeDateTime},
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	note := "hello"
	due := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	doc := Doc{Title: "Memo", Note: &note, Payload: json.RawMessage(`{"k":1}`), Due: &due}
	if err := app.DB().Create(&doc).Error; err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	jar := loginSuperuser(t, app)
	path := fmt.Sprintf("/api/v1/admin/resources/docs/%d", doc.ID)

	clear := doRequest(app, jar, http.MethodPatch, path, `{"note":null,"payload":null,"due":null}`)
	if clear.Code != http.StatusOK {
		t.Fatalf("clear patch status = %d; body: %s", clear.Code, clear.Body.String())
	}

	var stored Doc
	if err := app.DB().First(&stored, doc.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if stored.Title != "Memo" {
		t.Fatalf("title = %q, want Memo (omitted key must stay unchanged)", stored.Title)
	}
	if stored.Note != nil {
		t.Fatalf("note = %#v, want nil", stored.Note)
	}
	if len(stored.Payload) != 0 {
		t.Fatalf("payload = %#v, want nil/empty", stored.Payload)
	}
	if stored.Due != nil {
		t.Fatalf("due = %#v, want nil", stored.Due)
	}
}

func TestResourceJSONAndUUIDWrites(t *testing.T) {
	gin.SetMode(gin.TestMode)
	type Token struct {
		ID      uint            `gorm:"primaryKey" json:"id"`
		Token   uuid.UUID       `gorm:"type:uuid;not null" json:"token"`
		Payload json.RawMessage `json:"payload"`
		Title   string          `json:"title"`
	}
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&Token{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := admin.Register(app, Token{}, admin.Options{
		Slug: "tokens",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "token", Type: admin.TypeUUID, Required: true},
			{Name: "payload", Type: admin.TypeJSON},
			{Name: "title", Type: admin.TypeString, Required: true},
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	jar := loginSuperuser(t, app)

	exampleUUID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	create := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/tokens",
		fmt.Sprintf(`{"title":"k","token":"%s","payload":{"a":1}}`, exampleUUID))
	if create.Code != http.StatusOK {
		t.Fatalf("create status = %d; body: %s", create.Code, create.Body.String())
	}
	var created rowEnvelope
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	id := fmt.Sprint(asInt(created.Data["id"]))

	var stored Token
	if err := app.DB().First(&stored, created.Data["id"]).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if stored.Token != exampleUUID {
		t.Fatalf("token = %s, want %s", stored.Token, exampleUUID)
	}
	if string(stored.Payload) != `{"a":1}` {
		t.Fatalf("payload = %s, want {\"a\":1}", stored.Payload)
	}

	patch := doRequest(app, jar, http.MethodPatch, "/api/v1/admin/resources/tokens/"+id, `{"payload":{"b":2}}`)
	if patch.Code != http.StatusOK {
		t.Fatalf("patch status = %d; body: %s", patch.Code, patch.Body.String())
	}
	if err := app.DB().First(&stored, created.Data["id"]).Error; err != nil {
		t.Fatalf("reload after patch: %v", err)
	}
	if string(stored.Payload) != `{"b":2}` {
		t.Fatalf("patched payload = %s, want {\"b\":2}", stored.Payload)
	}
}

func TestResourceAnonymousUnauthorized(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	registerWidgets(t, app)
	rec := doRequest(app, nil, http.MethodGet, "/api/v1/admin/resources/widgets", "")
	assertError(t, rec, http.StatusUnauthorized, "authentication")
}

func TestResourceNonSuperuserForbidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	registerWidgets(t, app)
	jar := loginUser(t, app, "staff@example.com", testPassword)
	rec := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/widgets", "")
	assertError(t, rec, http.StatusForbidden, "authorization")
}

func TestResourceViewOnlyPermission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	registerWidgets(t, app)
	widget := Widget{Name: "Read only", SKU: "ro-1"}
	if err := app.DB().Create(&widget).Error; err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	email := "resource-viewer@example.com"
	jar := loginUser(t, app, email, testPassword)
	grantGroupPermission(t, app, email, "admin.widgets.view")

	list := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/widgets", "")
	if list.Code != http.StatusOK {
		t.Fatalf("view-only list status = %d; body: %s", list.Code, list.Body.String())
	}
	detail := doRequest(app, jar, http.MethodGet, fmt.Sprintf("/api/v1/admin/resources/widgets/%d", widget.ID), "")
	if detail.Code != http.StatusOK {
		t.Fatalf("view-only detail status = %d; body: %s", detail.Code, detail.Body.String())
	}

	create := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/widgets", `{"name":"Denied"}`)
	assertError(t, create, http.StatusForbidden, "authorization")
	update := doRequest(
		app,
		jar,
		http.MethodPatch,
		fmt.Sprintf("/api/v1/admin/resources/widgets/%d", widget.ID),
		`{"name":"Denied"}`,
	)
	assertError(t, update, http.StatusForbidden, "authorization")
	del := doRequest(app, jar, http.MethodDelete, fmt.Sprintf("/api/v1/admin/resources/widgets/%d", widget.ID), "")
	assertError(t, del, http.StatusForbidden, "authorization")
}

func TestResourceCustomPermissionKeyIsEnforced(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	const customView = "inventory.widgets.read"
	registerWidgets(t, app, func(opts *admin.Options) {
		opts.Permissions.View = customView
	})
	email := "custom-viewer@example.com"
	jar := loginUser(t, app, email, testPassword)

	defaultGrant, err := auth.EnsurePermission(
		context.Background(),
		app.DB(),
		"admin.widgets.view",
		"",
	)
	if err != nil {
		t.Fatalf("EnsurePermission(default): %v", err)
	}
	var user auth.User
	if err := app.DB().Where("email = ?", email).First(&user).Error; err != nil {
		t.Fatalf("load user: %v", err)
	}
	if err := auth.GrantPermissionToUser(context.Background(), app.DB(), &user, &defaultGrant); err != nil {
		t.Fatalf("GrantPermissionToUser(default): %v", err)
	}
	denied := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/widgets", "")
	assertError(t, denied, http.StatusForbidden, "authorization")

	customGrant, err := auth.EnsurePermission(context.Background(), app.DB(), customView, "")
	if err != nil {
		t.Fatalf("EnsurePermission(custom): %v", err)
	}
	if err := auth.GrantPermissionToUser(context.Background(), app.DB(), &user, &customGrant); err != nil {
		t.Fatalf("GrantPermissionToUser(custom): %v", err)
	}
	allowed := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/widgets", "")
	if allowed.Code != http.StatusOK {
		t.Fatalf("custom permission list status = %d; body: %s", allowed.Code, allowed.Body.String())
	}
}

func TestResourceUnknownSlugNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	registerWidgets(t, app)
	jar := loginSuperuser(t, app)
	rec := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/missing", "")
	assertError(t, rec, http.StatusNotFound, "not_found")
}

func TestResourceUnknownIDNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	registerWidgets(t, app)
	jar := loginSuperuser(t, app)
	rec := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/widgets/9999", "")
	assertError(t, rec, http.StatusNotFound, "not_found")
}

func TestResourceDuplicateUniqueReturnsConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	registerWidgets(t, app)
	if err := app.DB().Exec("CREATE UNIQUE INDEX idx_widgets_sku ON widgets(sku)").Error; err != nil {
		t.Fatalf("CREATE UNIQUE INDEX error = %v", err)
	}
	jar := loginSuperuser(t, app)
	first := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/widgets", `{"name":"Alpha","sku":"dup","price":10}`)
	if first.Code != http.StatusOK {
		t.Fatalf("create status = %d; body: %s", first.Code, first.Body.String())
	}
	dup := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/widgets", `{"name":"Beta","sku":"dup","price":20}`)
	assertError(t, dup, http.StatusConflict, "conflict")
}

func TestResourceDisabledActionForbidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	registerWidgets(t, app, func(o *admin.Options) {
		o.Actions.Delete = false
	})
	jar := loginSuperuser(t, app)
	create := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/widgets", `{"name":"Keep"}`)
	if create.Code != http.StatusOK {
		t.Fatalf("create status = %d; body: %s", create.Code, create.Body.String())
	}
	var created rowEnvelope
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id := fmt.Sprint(asInt(created.Data["id"]))
	rec := doRequest(app, jar, http.MethodDelete, "/api/v1/admin/resources/widgets/"+id, "")
	assertError(t, rec, http.StatusForbidden, "authorization")
}

func TestResourceValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	registerWidgets(t, app)
	jar := loginSuperuser(t, app)

	missing := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/widgets", `{}`)
	assertError(t, missing, http.StatusUnprocessableEntity, contract.CodeValidationError)
	env := decodeError(t, missing)
	if len(env.Fields["name"]) == 0 {
		t.Fatalf("fields.name missing; %#v", env.Fields)
	}

	readonly := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/widgets", `{"id":9,"name":"X"}`)
	assertError(t, readonly, http.StatusUnprocessableEntity, contract.CodeValidationError)

	unknown := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/widgets", `{"name":"X","nope":1}`)
	assertError(t, unknown, http.StatusUnprocessableEntity, contract.CodeValidationError)
}

func TestResourceCreateInvalidRequiredFieldReportsCoercionError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	registerWidgets(t, app, func(o *admin.Options) {
		for i := range o.Fields {
			if o.Fields[i].Name == "price" {
				o.Fields[i].Required = true
			}
		}
	})
	jar := loginSuperuser(t, app)

	rec := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/widgets",
		`{"name":"X","price":"not-a-number"}`)
	assertError(t, rec, http.StatusUnprocessableEntity, contract.CodeValidationError)

	env := decodeError(t, rec)
	got := env.Fields["price"]
	if len(got) != 1 || got[0] != "must be an integer" {
		t.Fatalf("fields.price = %#v, want [\"must be an integer\"]", got)
	}
}

func TestResourceCreateMixedMissingAndInvalidRequiredFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	registerWidgets(t, app, func(o *admin.Options) {
		for i := range o.Fields {
			if o.Fields[i].Name == "price" {
				o.Fields[i].Required = true
			}
		}
	})
	jar := loginSuperuser(t, app)

	// "name" is required and absent; "price" is required and present but
	// invalid. Each must keep its own distinct message.
	rec := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/widgets",
		`{"price":"not-a-number"}`)
	assertError(t, rec, http.StatusUnprocessableEntity, contract.CodeValidationError)

	env := decodeError(t, rec)
	if got := env.Fields["name"]; len(got) != 1 || got[0] != "is required" {
		t.Fatalf("fields.name = %#v, want [\"is required\"]", got)
	}
	if got := env.Fields["price"]; len(got) != 1 || got[0] != "must be an integer" {
		t.Fatalf("fields.price = %#v, want [\"must be an integer\"]", got)
	}
}

func TestResourceListPaginationSearchOrderFilter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	registerWidgets(t, app)
	jar := loginSuperuser(t, app)

	fixtures := []string{
		`{"name":"Alpha","sku":"s-a","price":30}`,
		`{"name":"Beta","sku":"s-b","price":10}`,
		`{"name":"Gamma","sku":"s-a","price":20}`,
	}
	for _, body := range fixtures {
		rec := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/widgets", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("create %s status = %d; body: %s", body, rec.Code, rec.Body.String())
		}
	}

	page := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/widgets?page=1&per_page=2", "")
	if page.Code != http.StatusOK {
		t.Fatalf("page status = %d; body: %s", page.Code, page.Body.String())
	}
	var paged listEnvelope
	if err := json.Unmarshal(page.Body.Bytes(), &paged); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if paged.Meta == nil || paged.Meta.Page != 1 || paged.Meta.PerPage != 2 || paged.Meta.Total != 3 {
		t.Fatalf("page meta = %+v", paged.Meta)
	}
	if len(paged.Data) != 2 {
		t.Fatalf("page data len = %d, want 2", len(paged.Data))
	}

	search := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/widgets?search=Beta", "")
	var found listEnvelope
	if err := json.Unmarshal(search.Body.Bytes(), &found); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	if found.Meta == nil || found.Meta.Total != 1 || len(found.Data) != 1 || found.Data[0]["name"] != "Beta" {
		t.Fatalf("search = %+v", found)
	}

	// Search is ASCII case-insensitive regardless of driver collation — see
	// issue #200. (Not a full Django icontains equivalent: SQLite's LOWER()
	// only folds ASCII, so Unicode case-folding parity across drivers isn't
	// guaranteed for accented/non-Latin terms.)
	caseInsensitive := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/widgets?search=beta", "")
	var foundLower listEnvelope
	if err := json.Unmarshal(caseInsensitive.Body.Bytes(), &foundLower); err != nil {
		t.Fatalf("decode case-insensitive search: %v", err)
	}
	if foundLower.Meta == nil || foundLower.Meta.Total != 1 || len(foundLower.Data) != 1 || foundLower.Data[0]["name"] != "Beta" {
		t.Fatalf("case-insensitive search = %+v", foundLower)
	}

	ordered := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/widgets?ordering=-price", "")
	var ranked listEnvelope
	if err := json.Unmarshal(ordered.Body.Bytes(), &ranked); err != nil {
		t.Fatalf("decode order: %v", err)
	}
	if len(ranked.Data) != 3 {
		t.Fatalf("order len = %d", len(ranked.Data))
	}
	if ranked.Data[0]["name"] != "Alpha" {
		t.Fatalf("first ordered = %#v, want Alpha", ranked.Data[0]["name"])
	}

	createdOrder := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/widgets?ordering=-created_at", "")
	if createdOrder.Code != http.StatusOK {
		t.Fatalf("ordering created_at status = %d; body: %s", createdOrder.Code, createdOrder.Body.String())
	}

	filtered := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/widgets?sku=s-a", "")
	var matches listEnvelope
	if err := json.Unmarshal(filtered.Body.Bytes(), &matches); err != nil {
		t.Fatalf("decode filter: %v", err)
	}
	if matches.Meta == nil || matches.Meta.Total != 2 {
		t.Fatalf("filter meta = %+v data=%+v", matches.Meta, matches.Data)
	}

	badOrder := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/widgets?ordering=note", "")
	assertError(t, badOrder, http.StatusUnprocessableEntity, contract.CodeValidationError)
}

func TestResourceListIncludesImplicitCreatedAt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	registerWidgets(t, app, func(o *admin.Options) {
		o.List = []string{"name", "created_at"}
	})
	jar := loginSuperuser(t, app)

	create := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/widgets", `{"name":"Dated"}`)
	if create.Code != http.StatusOK {
		t.Fatalf("create status = %d; body: %s", create.Code, create.Body.String())
	}
	var created rowEnvelope
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	assertCreatedAtPresent(t, created.Data)

	list := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/widgets", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d; body: %s", list.Code, list.Body.String())
	}
	var listed listEnvelope
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed.Data) != 1 {
		t.Fatalf("list len = %d, want 1; body: %s", len(listed.Data), list.Body.String())
	}
	assertCreatedAtPresent(t, listed.Data[0])

	id := fmt.Sprint(asInt(created.Data["id"]))
	detail := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/widgets/"+id, "")
	if detail.Code != http.StatusOK {
		t.Fatalf("detail status = %d; body: %s", detail.Code, detail.Body.String())
	}
	var got rowEnvelope
	if err := json.Unmarshal(detail.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	assertCreatedAtPresent(t, got.Data)
}

func TestResourceCreateWithoutCSRFForbidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	registerWidgets(t, app)
	jar := loginSuperuser(t, app)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/resources/widgets", strings.NewReader(`{"name":"X"}`))
	req.Header.Set("Content-Type", "application/json")
	jar.attach(req)
	app.Router().ServeHTTP(rec, req)
	assertError(t, rec, http.StatusForbidden, "authorization")
}

func TestResourceBelongsToStoresFK(t *testing.T) {
	gin.SetMode(gin.TestMode)
	type Category struct {
		ID   uint   `gorm:"primaryKey" json:"id"`
		Name string `json:"name"`
	}
	type Item struct {
		ID         uint   `gorm:"primaryKey" json:"id"`
		Name       string `json:"name"`
		CategoryID uint   `json:"category_id"`
	}
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&Category{}, &Item{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := admin.Register(app, Item{}, admin.Options{
		Slug: "items",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "name", Type: admin.TypeString, Required: true},
			{
				Name:    "category_id",
				Type:    admin.TypeRelation,
				Related: &admin.Relation{Slug: "categories", Kind: admin.RelBelongsTo, LabelField: "name"},
			},
		},
		List:   []string{"name", "category_id"},
		Filter: []string{"category_id"},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	jar := loginSuperuser(t, app)
	rec := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/items", `{"name":"Nail","category_id":7}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var created rowEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if asInt(created.Data["category_id"]) != 7 {
		t.Fatalf("category_id = %#v, want 7", created.Data["category_id"])
	}
}

func TestResourceCreateForeignKeyViolationIsValidationError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	type FKCategory struct {
		ID uint `gorm:"primaryKey" json:"id"`
	}
	type FKItem struct {
		ID         uint       `gorm:"primaryKey" json:"id"`
		Name       string     `json:"name"`
		CategoryID uint       `json:"category_id"`
		Category   FKCategory `gorm:"constraint:OnUpdate:CASCADE,OnDelete:RESTRICT;" json:"-"`
	}
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&FKCategory{}, &FKItem{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := admin.Register(app, FKItem{}, admin.Options{
		Slug: "fk-items",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "name", Type: admin.TypeString},
			{
				Name:    "category_id",
				Type:    admin.TypeRelation,
				Related: &admin.Relation{Slug: "fk-categories", Kind: admin.RelBelongsTo, LabelField: "id"},
			},
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	jar := loginSuperuser(t, app)

	// Unlike TestResourceBelongsToStoresFK, FKItem.Category is a real GORM
	// association with a DB-level constraint, so a nonexistent category_id
	// reaches the database as an actual foreign-key violation, not just
	// stored as an opaque integer.
	rec := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/fk-items", `{"name":"Nail","category_id":999}`)
	assertError(t, rec, http.StatusUnprocessableEntity, contract.CodeValidationError)
}

func TestResourceCreateNotNullViolationIsValidationError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	type StrictWidget struct {
		ID   uint    `gorm:"primaryKey" json:"id"`
		Name *string `gorm:"not null" json:"name"`
	}
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&StrictWidget{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	// "name" is intentionally not marked Required in the admin metadata, to
	// reproduce the issue's actual scenario: the DB schema enforces NOT
	// NULL even where the admin Field options do not.
	if err := admin.Register(app, StrictWidget{}, admin.Options{
		Slug: "strict-widgets",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "name", Type: admin.TypeString},
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	jar := loginSuperuser(t, app)

	rec := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/strict-widgets", `{}`)
	assertError(t, rec, http.StatusUnprocessableEntity, contract.CodeValidationError)
}

func TestResourceDeleteForeignKeyViolationIsConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	type DelCategory struct {
		ID uint `gorm:"primaryKey" json:"id"`
	}
	type DelItem struct {
		ID         uint        `gorm:"primaryKey" json:"id"`
		CategoryID uint        `json:"category_id"`
		Category   DelCategory `gorm:"constraint:OnUpdate:CASCADE,OnDelete:RESTRICT;" json:"-"`
	}
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&DelCategory{}, &DelItem{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	category := DelCategory{}
	if err := app.DB().Create(&category).Error; err != nil {
		t.Fatalf("create fixture category: %v", err)
	}
	if err := app.DB().Create(&DelItem{CategoryID: category.ID}).Error; err != nil {
		t.Fatalf("create fixture item: %v", err)
	}
	if err := admin.Register(app, DelCategory{}, admin.Options{
		Slug: "del-categories",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	jar := loginSuperuser(t, app)

	// The category is still referenced by DelItem (ON DELETE RESTRICT), so
	// this must be a 409 conflict, not the 422 validation error a bad
	// foreign key on create/update would produce, and not a 500.
	rec := doRequest(app, jar, http.MethodDelete, fmt.Sprintf("/api/v1/admin/resources/del-categories/%d", category.ID), "")
	assertError(t, rec, http.StatusConflict, "conflict")
}

func TestJWTModeDoesNotMountAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newJWTApp(t)
	rec := doRequest(app, nil, http.MethodGet, "/api/v1/admin/meta", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("JWT admin meta status = %d, want 404; body: %s", rec.Code, rec.Body.String())
	}
	spec := doRequest(app, nil, http.MethodGet, "/openapi.json", "")
	if spec.Code != http.StatusOK {
		t.Fatalf("openapi status = %d", spec.Code)
	}
	if strings.Contains(spec.Body.String(), "/api/v1/admin/meta") {
		t.Fatal("JWT OpenAPI includes admin routes")
	}
}

func TestHandlersDoNotImportReflect(t *testing.T) {
	t.Parallel()
	// Request handlers must not walk arbitrary Go types. Registration-time
	// reflect lives in fields.go (and register.go constructors).
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	forbidden := []string{
		"mount.go",
		"meta.go",
		"resources.go",
		"convert.go",
		"options.go",
		"errors.go",
		"registry.go",
	}
	fset := token.NewFileSet()
	for _, name := range forbidden {
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, spec := range file.Imports {
			if spec.Path != nil && spec.Path.Value == `"reflect"` {
				t.Errorf("%s imports reflect; request-time / handler files must not", name)
			}
		}
	}
}

func asInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case int:
		return int64(n)
	case int64:
		return n
	case uint:
		if n > math.MaxInt64 {
			return math.MaxInt64
		}
		return int64(n)
	default:
		return 0
	}
}

func assertStoredWidget(t *testing.T, app *framework.App, id any, want Widget) {
	t.Helper()
	var stored Widget
	if err := app.DB().First(&stored, asInt(id)).Error; err != nil {
		t.Fatalf("reload widget: %v", err)
	}
	if stored.Name != want.Name || stored.SKU != want.SKU || stored.Price != want.Price || stored.Note != want.Note {
		t.Fatalf("stored widget = {name:%q sku:%q price:%d note:%q}, want {name:%q sku:%q price:%d note:%q}",
			stored.Name, stored.SKU, stored.Price, stored.Note,
			want.Name, want.SKU, want.Price, want.Note)
	}
}

func assertCreatedAtPresent(t *testing.T, row map[string]any) {
	t.Helper()
	v, ok := row["created_at"]
	if !ok || v == nil {
		t.Fatalf("row missing created_at: %#v", row)
	}
	s := fmt.Sprint(v)
	if s == "" || strings.HasPrefix(s, "0001-01-01") {
		t.Fatalf("created_at is empty/zero: %#v", v)
	}
}
