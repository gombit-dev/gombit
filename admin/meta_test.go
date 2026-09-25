package admin_test

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"time"

	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/framework"
	"github.com/pb33f/libopenapi"
	openapivalidator "github.com/pb33f/libopenapi-validator"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
)

type catalogEnvelope struct {
	Data struct {
		Models []admin.ModelMeta `json:"models"`
	} `json:"data"`
	Meta *admin.CatalogAux `json:"meta"`
}

type modelEnvelope struct {
	Data admin.ModelMeta `json:"data"`
}

func TestMetaEmptyCatalog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	jar := loginSuperuser(t, app)

	rec := doRequest(app, jar, http.MethodGet, "/api/v1/admin/meta", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var env catalogEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v; body: %s", err, rec.Body.String())
	}
	if env.Data.Models == nil {
		t.Fatal("data.models is nil, want empty array")
	}
	if len(env.Data.Models) != 0 {
		t.Fatalf("data.models len = %d, want 0", len(env.Data.Models))
	}
	if env.Meta == nil || env.Meta.Auth == nil {
		t.Fatal("meta.auth is nil")
	}
	if env.Meta.Auth.Mode != "cookie" || env.Meta.Auth.Bootstrap != "permissions" {
		t.Fatalf("meta.auth = %+v", env.Meta.Auth)
	}
}

func TestMetaListsRegisteredModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	registerWidgets(t, app)
	jar := loginSuperuser(t, app)

	rec := doRequest(app, jar, http.MethodGet, "/api/v1/admin/meta", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var env catalogEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v; body: %s", err, rec.Body.String())
	}
	if len(env.Data.Models) != 1 {
		t.Fatalf("data.models len = %d, want 1; body: %s", len(env.Data.Models), rec.Body.String())
	}
	got := env.Data.Models[0]
	if got.Slug != "widgets" || got.PK != "id" {
		t.Fatalf("model = %+v, want slug=widgets pk=id", got)
	}
	if len(got.Fields) == 0 {
		t.Fatal("fields is empty")
	}
	if got.Permissions.View != "admin.widgets.view" {
		t.Fatalf("permissions.view = %q", got.Permissions.View)
	}
	if !got.Actions.List || !got.Actions.Create {
		t.Fatalf("actions = %+v", got.Actions)
	}
	if !got.Can.View || !got.Can.Create || !got.Can.Update || !got.Can.Delete {
		t.Fatalf("superuser can = %+v, want every enabled capability", got.Can)
	}

	one := doRequest(app, jar, http.MethodGet, "/api/v1/admin/meta/widgets", "")
	if one.Code != http.StatusOK {
		t.Fatalf("GET meta/widgets status = %d; body: %s", one.Code, one.Body.String())
	}
	var single modelEnvelope
	if err := json.Unmarshal(one.Body.Bytes(), &single); err != nil {
		t.Fatalf("decode one: %v", err)
	}
	if single.Data.Slug != "widgets" {
		t.Fatalf("slug = %q", single.Data.Slug)
	}
	if !single.Data.Can.View || !single.Data.Can.Create || !single.Data.Can.Update || !single.Data.Can.Delete {
		t.Fatalf("single superuser can = %+v, want every enabled capability", single.Data.Can)
	}
}

func TestMetaRelationHasManyIsReadOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	type Category struct {
		ID   uint   `gorm:"primaryKey" json:"id"`
		Name string `json:"name"`
	}
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&Category{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := admin.Register(app, Category{}, admin.Options{
		Slug: "categories",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "name", Type: admin.TypeString, Required: true},
			{
				Name: "widgets",
				Type: admin.TypeRelation,
				Related: &admin.Relation{
					Slug:       "widgets",
					Kind:       admin.RelHasMany,
					LabelField: "name",
				},
			},
		},
		List: []string{"name"},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	jar := loginSuperuser(t, app)
	rec := doRequest(app, jar, http.MethodGet, "/api/v1/admin/meta/categories", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var env modelEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var rel *admin.FieldMeta
	for i := range env.Data.Fields {
		if env.Data.Fields[i].Name == "widgets" {
			rel = &env.Data.Fields[i]
			break
		}
	}
	if rel == nil || rel.Related == nil || rel.Related.Kind != admin.RelHasMany {
		t.Fatalf("widgets relation = %+v", rel)
	}
	if !rel.ReadOnly {
		t.Fatal("has_many field should be readonly in meta")
	}
}

func TestMetaReadsValidateConstraints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	type Person struct {
		ID     uint   `gorm:"primaryKey" json:"id"`
		Age    int    `json:"age" validate:"min=0;max=150"`
		Status string `json:"status" validate:"default=draft"`
		Code   string `json:"code" validate:"max_length=8;pattern=^[a-z]+$"`
	}
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&Person{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := admin.Register(app, Person{}, admin.Options{
		Slug: "people",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "age", Type: admin.TypeInteger, Required: true},
			{Name: "status", Type: admin.TypeString},
			{Name: "code", Type: admin.TypeString},
		},
		List: []string{"status"},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	jar := loginSuperuser(t, app)
	rec := doRequest(app, jar, http.MethodGet, "/api/v1/admin/meta/people", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var env modelEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byName := map[string]admin.FieldMeta{}
	for _, f := range env.Data.Fields {
		byName[f.Name] = f
	}
	if byName["age"].Minimum != "0" || byName["age"].Maximum != "150" {
		t.Fatalf("age = %+v", byName["age"])
	}
	if byName["status"].Default != "draft" {
		t.Fatalf("status = %+v", byName["status"])
	}
	if byName["code"].MaxLength != 8 || byName["code"].Pattern != "^[a-z]+$" {
		t.Fatalf("code = %+v", byName["code"])
	}
	if strings.Contains(rec.Body.String(), `"minimum":""`) {
		t.Fatalf("empty constraints must be omitted: %s", rec.Body.String())
	}
}

func TestMetaUnknownSlugNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	registerWidgets(t, app)
	jar := loginSuperuser(t, app)
	rec := doRequest(app, jar, http.MethodGet, "/api/v1/admin/meta/missing", "")
	assertError(t, rec, http.StatusNotFound, "not_found")
}

func TestMetaAnonymousUnauthorized(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	registerWidgets(t, app)
	rec := doRequest(app, nil, http.MethodGet, "/api/v1/admin/meta", "")
	assertError(t, rec, http.StatusUnauthorized, "authentication")
}

func TestMetaNonSuperuserForbidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	registerWidgets(t, app)
	jar := loginUser(t, app, "user@example.com", testPassword)
	rec := doRequest(app, jar, http.MethodGet, "/api/v1/admin/meta", "")
	assertError(t, rec, http.StatusForbidden, "authorization")
}

func TestMetaViewPermissionFiltersCatalogAndCapabilities(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	registerWidgets(t, app)
	email := "viewer@example.com"
	jar := loginUser(t, app, email, testPassword)
	grantGroupPermission(t, app, email, "admin.widgets.view")

	rec := doRequest(app, jar, http.MethodGet, "/api/v1/admin/meta", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var env catalogEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if len(env.Data.Models) != 1 {
		t.Fatalf("catalog models = %d, want 1; body: %s", len(env.Data.Models), rec.Body.String())
	}
	can := env.Data.Models[0].Can
	if !can.View || can.Create || can.Update || can.Delete {
		t.Fatalf("view-only can = %+v", can)
	}

	one := doRequest(app, jar, http.MethodGet, "/api/v1/admin/meta/widgets", "")
	if one.Code != http.StatusOK {
		t.Fatalf("model meta status = %d; body: %s", one.Code, one.Body.String())
	}
	var single modelEnvelope
	if err := json.Unmarshal(one.Body.Bytes(), &single); err != nil {
		t.Fatalf("decode model meta: %v", err)
	}
	if single.Data.Can != can {
		t.Fatalf("model can = %+v, want %+v", single.Data.Can, can)
	}
}

func TestMetaSuperuserCapabilitiesHonorDisabledActions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	registerWidgets(t, app, func(opts *admin.Options) {
		opts.Actions.Create = false
	})
	jar := loginSuperuser(t, app)

	rec := doRequest(app, jar, http.MethodGet, "/api/v1/admin/meta", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var env catalogEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if len(env.Data.Models) != 1 {
		t.Fatalf("catalog models = %d, want 1", len(env.Data.Models))
	}
	if env.Data.Models[0].Can.Create {
		t.Fatalf("superuser can.create = true when create action disabled: %+v", env.Data.Models[0].Can)
	}
}

func TestMetaHonorsAPIPrefix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := openSQLite(t)
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatalf("auth.Migrate: %v", err)
	}
	if err := db.AutoMigrate(&Widget{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.BcryptCost = bcrypt.MinCost
	cfg.Auth.AccessTokenTTL = time.Minute
	cfg.Auth.RefreshTokenTTL = time.Hour
	cfg.Auth.Mode = config.AuthModeCookie
	cfg.API.Prefix = "/svc/v2"
	app, err := framework.New(
		framework.WithConfig(cfg),
		framework.WithDatabase(db),
		framework.WithLogger(zap.NewNop()),
	)
	if err != nil {
		t.Fatalf("framework.New: %v", err)
	}
	registerWidgets(t, app)
	jar := loginSuperuser(t, app)
	rec := doRequest(app, jar, http.MethodGet, "/svc/v2/admin/meta", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestAdminRoutesAppearInOpenAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	rec := doRequest(app, nil, http.MethodGet, "/openapi.json", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("openapi status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, path := range []string{
		"/api/v1/admin/meta",
		"/api/v1/admin/meta/{slug}",
		"/api/v1/admin/resources/{slug}",
		"/api/v1/admin/resources/{slug}/{id}",
	} {
		if !strings.Contains(body, path) {
			t.Fatalf("openapi.json missing %s", path)
		}
	}
}

// TestAdminOpenAPISchemaNamesAreValid is the regression test for #100: the
// admin data plane's generic responses used map[string]any directly as a
// generic type parameter (contract.Data[map[string]any], contract.DataMeta
// [[]map[string]any, contract.PageMeta]). map[string]any has no
// reflect.Type.Name(), so Huma's DefaultSchemaNamer fell back to Go's
// unnamed-type string ("map[string]interface {}"), and that literal space
// and braces survived into the OpenAPI component name — e.g.
// "DataMetaListMapStringInterface {}PageMeta" — which fails OpenAPI 3.1
// validation (component names must match ^[a-zA-Z0-9._-]+$).
//
// No admin.Register call is needed to reproduce this: the admin data-plane
// routes (and their response schemas) are mounted unconditionally under
// cookie auth, per newCookieApp.
func TestAdminOpenAPISchemaNamesAreValid(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	rec := doRequest(app, nil, http.MethodGet, "/openapi.json", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("openapi status = %d", rec.Code)
	}

	var doc struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode openapi.json: %v", err)
	}
	if len(doc.Components.Schemas) == 0 {
		t.Fatal("openapi.json has no component schemas to check")
	}

	validName := regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
	for name := range doc.Components.Schemas {
		if !validName.MatchString(name) {
			t.Errorf("schema component name %q does not match OpenAPI 3.1's ^[a-zA-Z0-9._-]+$ pattern", name)
		}
	}

	document, err := libopenapi.NewDocument(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("parse OpenAPI document: %v", err)
	}
	validator, validatorErrs := openapivalidator.NewValidator(document)
	if len(validatorErrs) > 0 {
		t.Fatalf("create OpenAPI validator: %v", validatorErrs)
	}
	valid, documentErrs := validator.ValidateDocument()
	if !valid || len(documentErrs) > 0 {
		t.Fatalf("validate OpenAPI 3.1 document: valid=%v errors=%v", valid, documentErrs)
	}
}
