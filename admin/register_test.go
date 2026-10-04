package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/framework"
	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/claims"
	"github.com/gombit-dev/gombit/types"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

func TestRegisterMissingSlug(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	err := admin.Register(app, Widget{}, admin.Options{})
	if err == nil || !strings.Contains(err.Error(), "missing slug") {
		t.Fatalf("Register() error = %v, want missing slug", err)
	}
}

func TestRegisterInvalidSlug(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	err := admin.Register(app, Widget{}, admin.Options{Slug: "Widgets"})
	if err == nil || !strings.Contains(err.Error(), "invalid slug") {
		t.Fatalf("Register() error = %v, want invalid slug", err)
	}
}

func TestRegisterDuplicateSlug(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	if err := admin.Register(app, Widget{}, widgetOptions()); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := admin.Register(app, Widget{}, widgetOptions())
	if err == nil || !strings.Contains(err.Error(), "duplicate slug") {
		t.Fatalf("second Register() error = %v, want duplicate slug", err)
	}
}

func TestRegisterRejectsJWTMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newJWTApp(t)
	err := admin.Register(app, Widget{}, widgetOptions())
	if err == nil || !strings.Contains(err.Error(), "cookie") {
		t.Fatalf("Register() on JWT app error = %v, want cookie-auth error", err)
	}
}

func TestRegisterDerivesPKAndEmptyFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	if err := admin.Register(app, Widget{}, admin.Options{
		Slug:     "widgets",
		List:     []string{"name", "created_at"},
		Ordering: []string{"created_at"},
	}); err != nil {
		t.Fatalf("Register empty Fields: %v", err)
	}
}

func TestRegisterUnknownListField(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	err := admin.Register(app, Widget{}, widgetOptions(func(o *admin.Options) {
		o.List = []string{"missing"}
	}))
	if err == nil || !strings.Contains(err.Error(), "list") {
		t.Fatalf("Register() error = %v, want unknown list field", err)
	}
}

func TestRegisterImplicitTimestampMissingOnModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	type Bare struct {
		ID   uint   `gorm:"primaryKey" json:"id"`
		Name string `json:"name"`
	}
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&Bare{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	err := admin.Register(app, Bare{}, admin.Options{
		Slug:     "bares",
		Fields:   []admin.Field{{Name: "id", Type: admin.TypeInteger, ReadOnly: true}, {Name: "name", Type: admin.TypeString}},
		Ordering: []string{"created_at"},
	})
	if err == nil || !strings.Contains(err.Error(), "created_at") {
		t.Fatalf("Register() error = %v, want missing timestamp", err)
	}
}

func TestRegisterPKOverrideMustBeInFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	err := admin.Register(app, Widget{}, widgetOptions(func(o *admin.Options) {
		o.PK = "missing"
	}))
	if err == nil || !strings.Contains(err.Error(), "pk") {
		t.Fatalf("Register() error = %v, want pk not in Fields", err)
	}
}

func TestRegisterRejectsUnknownFieldType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	err := admin.Register(app, Widget{}, widgetOptions(func(o *admin.Options) {
		o.Fields = append(o.Fields, admin.Field{Name: "mystery", Type: "nope"})
	}))
	if err == nil || !strings.Contains(err.Error(), "unknown type") {
		t.Fatalf("Register() error = %v, want unknown type", err)
	}
}

func TestRegisterRejectsDuplicateFieldName(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	err := admin.Register(app, Widget{}, widgetOptions(func(o *admin.Options) {
		o.Fields = append(o.Fields, admin.Field{Name: "name", Type: admin.TypeString})
	}))
	if err == nil || !strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("Register() error = %v, want duplicate field", err)
	}
}

func TestRegisterRejectsTextFilter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	err := admin.Register(app, Widget{}, widgetOptions(func(o *admin.Options) {
		o.Filter = []string{"note"}
	}))
	if err == nil || !strings.Contains(err.Error(), "filter") || !strings.Contains(err.Error(), "text") {
		t.Fatalf("Register() error = %v, want text filter rejected", err)
	}
}

func TestRegisterRejectsHasManyInQueryOptions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	type Category struct {
		ID   uint   `gorm:"primaryKey" json:"id"`
		Name string `json:"name"`
	}
	hasMany := admin.Field{
		Name: "widgets",
		Type: admin.TypeRelation,
		Related: &admin.Relation{
			Slug:       "widgets",
			Kind:       admin.RelHasMany,
			LabelField: "name",
		},
	}
	fields := []admin.Field{
		{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
		{Name: "name", Type: admin.TypeString, Required: true},
		hasMany,
	}
	cases := []struct {
		kind string
		opts admin.Options
	}{
		{"search", admin.Options{Slug: "categories", Fields: fields, Search: []string{"widgets"}}},
		{"filter", admin.Options{Slug: "cat-filter", Fields: fields, Filter: []string{"widgets"}}},
		{"ordering", admin.Options{Slug: "cat-order", Fields: fields, Ordering: []string{"widgets"}}},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			app := newCookieApp(t)
			if err := app.DB().AutoMigrate(&Category{}); err != nil {
				t.Fatalf("AutoMigrate: %v", err)
			}
			err := admin.Register(app, Category{}, tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.kind) || !strings.Contains(err.Error(), "has_many") {
				t.Fatalf("Register() error = %v, want %s has_many", err, tc.kind)
			}
		})
	}
}

func TestFieldsFromUsesJSONNames(t *testing.T) {
	fields, err := admin.FieldsFrom(Widget{})
	if err != nil {
		t.Fatalf("FieldsFrom: %v", err)
	}
	if len(fields) == 0 {
		t.Fatal("FieldsFrom returned no fields")
	}
	byName := map[string]admin.Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	id, ok := byName["id"]
	if !ok {
		t.Fatalf("FieldsFrom missing id; fields=%v", fields)
	}
	if !id.ReadOnly {
		t.Fatal("id should be readonly")
	}
	if _, ok := byName["name"]; !ok {
		t.Fatalf("FieldsFrom missing name; fields=%v", fields)
	}
	if _, ok := byName["created_at"]; !ok {
		t.Fatalf("FieldsFrom missing created_at; fields=%v", fields)
	}
}

func TestFieldsFromOneToOneIsUniqueFK(t *testing.T) {
	type User struct {
		ID        uint   `gorm:"primaryKey" json:"id"`
		ProfileID uint   `gorm:"uniqueIndex" json:"profile_id"`
		Profile   Widget `json:"-"`
		ParentID  *uint  `json:"parent_id"`
		Parent    *User  `json:"-"`
	}
	fields, err := admin.FieldsFrom(User{})
	if err != nil {
		t.Fatalf("FieldsFrom: %v", err)
	}
	byName := map[string]admin.Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	if byName["profile_id"].Related == nil || byName["profile_id"].Related.Kind != admin.RelOneToOne {
		t.Fatalf("profile_id = %+v", byName["profile_id"].Related)
	}
	if byName["parent_id"].Related == nil || byName["parent_id"].Related.Kind != admin.RelBelongsTo {
		t.Fatalf("parent_id = %+v", byName["parent_id"].Related)
	}

	type Membership struct {
		ID     uint   `gorm:"primaryKey" json:"id"`
		UserID uint   `gorm:"uniqueIndex:idx_membership" json:"user_id"`
		User   Widget `json:"-"`
		OrgID  uint   `gorm:"uniqueIndex:idx_membership" json:"org_id"`
		Org    Widget `json:"-"`
	}
	membership, err := admin.FieldsFrom(Membership{})
	if err != nil {
		t.Fatalf("FieldsFrom membership: %v", err)
	}
	for _, f := range membership {
		if f.Name != "user_id" && f.Name != "org_id" {
			continue
		}
		if f.Related == nil || f.Related.Kind != admin.RelBelongsTo {
			t.Fatalf("%s composite unique index = %+v, want belongs_to", f.Name, f.Related)
		}
	}

	type Account struct {
		ID        uint   `gorm:"primaryKey" json:"id"`
		ProfileID uint   `gorm:"index:idx_profile,unique" json:"profile_id"`
		Profile   Widget `json:"-"`
	}
	account, err := admin.FieldsFrom(Account{})
	if err != nil {
		t.Fatalf("FieldsFrom account: %v", err)
	}
	var profile admin.Field
	for _, f := range account {
		if f.Name == "profile_id" {
			profile = f
		}
	}
	if profile.Related == nil || profile.Related.Kind != admin.RelOneToOne {
		t.Fatalf("index:name,unique = %+v, want one_to_one", profile.Related)
	}
}

func TestFieldsFromInfersUUIDAndJSON(t *testing.T) {
	type Token struct {
		ID      uuid.UUID       `gorm:"type:uuid;primaryKey" json:"id"`
		Payload json.RawMessage `json:"payload"`
		Name    string          `json:"name"`
	}
	fields, err := admin.FieldsFrom(Token{})
	if err != nil {
		t.Fatalf("FieldsFrom: %v", err)
	}
	byName := map[string]admin.Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	id, ok := byName["id"]
	if !ok {
		t.Fatalf("FieldsFrom missing id; fields=%v", fields)
	}
	if id.Type != admin.TypeUUID {
		t.Fatalf("id type = %q, want %q", id.Type, admin.TypeUUID)
	}
	if id.ReadOnly || !id.Required {
		t.Fatalf("uuid primary key = %+v, want writable and required", id)
	}
	type Named struct {
		ID   string `gorm:"primaryKey" json:"id"`
		Name string `json:"name"`
	}
	named, err := admin.FieldsFrom(Named{})
	if err != nil {
		t.Fatalf("FieldsFrom string pk: %v", err)
	}
	var stringPK admin.Field
	for _, f := range named {
		if f.Name == "id" {
			stringPK = f
		}
	}
	if stringPK.Name != "id" || stringPK.ReadOnly || !stringPK.Required {
		t.Fatalf("string primary key = %+v, want writable required id", stringPK)
	}
	payload, ok := byName["payload"]
	if !ok {
		t.Fatalf("FieldsFrom missing payload; fields=%v", fields)
	}
	if payload.Type != admin.TypeJSON {
		t.Fatalf("payload type = %q, want %q", payload.Type, admin.TypeJSON)
	}
}

func TestFieldsFromIsRegistrationTimeOnly(t *testing.T) {
	// Compile-time documentation: FieldsFrom is exported for Register
	// callers, not handlers. The handler-import test locks the other half.
	_, err := admin.FieldsFrom(Widget{})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRegisterExplicitFieldsSkipServerObligation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	type book struct {
		gorm.Model
		Title   string `gorm:"not null"`
		OwnerID uint   `gorm:"not null" gombit:"server"`
		Name    string `gorm:"not null" gombit:"read"`
	}
	app := newCookieApp(t)
	err := admin.Register(app, book{}, admin.Options{
		Slug: "policy-books",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "title", Type: admin.TypeString, Required: true},
			{Name: "owner_id", Type: admin.TypeInteger},
		},
	})
	if err != nil {
		t.Fatalf("explicit Fields must not apply server policy: %v", err)
	}
}

func TestRegisterColumnMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	type Mapped struct {
		ID    uint   `gorm:"primaryKey" json:"id"`
		Title string `gorm:"column:title_text" json:"title"`
	}
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&Mapped{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	err := admin.Register(app, Mapped{}, admin.Options{
		Slug: "mapped",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "title", Type: admin.TypeString, Column: "title_text", Required: true},
		},
		List: []string{"title"},
	})
	if err != nil {
		t.Fatalf("Register column mapping: %v", err)
	}
}

func TestRegisterErrorIsNotWrappedAsHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	err := admin.Register(app, Widget{}, admin.Options{})
	if err == nil {
		t.Fatal("expected error")
	}
	var env interface{ GetStatus() int }
	if errors.As(err, &env) {
		t.Fatalf("Register should not return an HTTP error, got %#v", err)
	}
}

type Attachment struct {
	gorm.Model
	Title string      `gorm:"not null"`
	File  *types.File `gorm:"size:512;uniqueIndex"`
}

// TestRegisterWithAFileColumn: a model with a storage-backed column, with
// no storage tag, registers (its field owns <table>/<column>/), delete
// included: the admin deletes the row through the claims (claims.DeleteWith),
// releasing its file in the same transaction and deleting it after.
func TestRegisterWithAFileColumn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := openSQLite(t)
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(append(claims.Models(), &Attachment{})...); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.BcryptCost = bcrypt.MinCost
	cfg.Auth.AccessTokenTTL = time.Minute
	cfg.Auth.RefreshTokenTTL = time.Hour
	cfg.Auth.Mode = config.AuthModeCookie
	cfg.Storage.Driver = config.StorageDriverMemory
	app, err := framework.New(framework.WithConfig(cfg), framework.WithDatabase(db), framework.WithLogger(zap.NewNop()))
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Register(app, Attachment{}, admin.Options{Slug: "attachments"}); err != nil {
		t.Fatalf("Register() = %v", err)
	}
	// A record holding a file, through the claims protocol.
	ctx := context.Background()
	cl := claims.New(db.DB, app.Storage())
	key := "attachments/file/held"
	if err := cl.Pending(ctx, key, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Storage().Put(ctx, key, strings.NewReader("bytes"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	k := types.File(key)
	rec := Attachment{Title: "a", File: &k}
	if err := cl.CreateWith(ctx, []string{key}, func(tx *gorm.DB) error { return tx.Create(&rec).Error }); err != nil {
		t.Fatal(err)
	}

	jar := loginSuperuser(t, app)
	meta := doRequest(app, jar, http.MethodGet, apiPrefix(app)+"/admin/meta/attachments", "")
	if meta.Code != http.StatusOK || !strings.Contains(meta.Body.String(), `"delete":true`) {
		t.Fatalf("meta = %d %s; want delete enabled", meta.Code, meta.Body)
	}
	del := doRequest(app, jar, http.MethodDelete, fmt.Sprintf("%s/admin/resources/attachments/%d", apiPrefix(app), rec.ID), "")
	if del.Code != http.StatusOK {
		t.Fatalf("DELETE of a file-backed record = %d %s", del.Code, del.Body)
	}
	var n int64
	db.Model(&Attachment{}).Where("id = ?", rec.ID).Count(&n)
	var claim claims.Claim
	err = db.Where("object_key = ?", key).Take(&claim).Error
	if n != 0 || (err == nil && claim.State != claims.Deleting) {
		t.Fatalf("after the delete: %d records, claim %+v (%v); want the record gone and its claim released", n, claim, err)
	}
	if ok, _ := storage.Exists(ctx, app.Storage(), key); ok {
		t.Fatal("the deleted record's file was kept")
	}

	id := admin.Field{Name: "id", Type: admin.TypeInteger, ReadOnly: true}
	// Explicit Fields cannot map the file column as any type but file or
	// image (whose writes go through the upload protocol): that would write
	// arbitrary keys past it, and strand the replaced key's claim.
	for name, fields := range map[string][]admin.Field{
		"by name":   {id, {Name: "title", Type: admin.TypeString}, {Name: "file", Type: admin.TypeString}},
		"by column": {id, {Name: "title", Type: admin.TypeString}, {Name: "attachment_key", Type: admin.TypeString, Column: "file"}},
		"read-only": {id, {Name: "title", Type: admin.TypeString}, {Name: "file", Type: admin.TypeString, ReadOnly: true}},
	} {
		err := admin.Register(app, Attachment{}, admin.Options{Slug: "attachments-" + strings.ReplaceAll(name, " ", "-"), Fields: fields})
		if err == nil || !strings.Contains(err.Error(), "upload protocol") {
			t.Errorf("Register() with the file column mapped %s = %v, want an error", name, err)
		}
	}
	typed := []admin.Field{id, {Name: "title", Type: admin.TypeString}, {Name: "file", Type: admin.TypeFile}}
	if err := admin.Register(app, Attachment{}, admin.Options{Slug: "attachments-typed", Fields: typed}); err != nil {
		t.Fatalf("Register() with the file column as a file field = %v", err)
	}
	if err := admin.Register(app, Attachment{}, admin.Options{Slug: "attachments-titled", Fields: []admin.Field{id, {Name: "title", Type: admin.TypeString}}}); err != nil {
		t.Fatalf("Register() with explicit Fields leaving the file out = %v", err)
	}
	// A fresh record holding a file: no admin write can attach a foreign key.
	key2 := "attachments/file/held2"
	if err := cl.Pending(ctx, key2, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Storage().Put(ctx, key2, strings.NewReader("bytes"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	k2 := types.File(key2)
	rec2 := Attachment{Title: "b", File: &k2}
	if err := cl.CreateWith(ctx, []string{key2}, func(tx *gorm.DB) error { return tx.Create(&rec2).Error }); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"attachments", "attachments-typed", "attachments-titled"} {
		patch := doRequest(app, jar, http.MethodPatch, fmt.Sprintf("%s/admin/resources/%s/%d", apiPrefix(app), slug, rec2.ID), `{"title":"c","file":"some/foreign/key"}`)
		var got Attachment
		if err := db.First(&got, rec2.ID).Error; err != nil {
			t.Fatal(err)
		}
		if got.File == nil || string(*got.File) != key2 {
			t.Fatalf("PATCH %s (%d %s) changed the file to %v", slug, patch.Code, patch.Body, got.File)
		}
	}
	var claim2 claims.Claim
	if err := db.Where("object_key = ?", key2).Take(&claim2).Error; err != nil || claim2.State != claims.Held {
		t.Fatalf("after the PATCHes the claim = %+v, %v; want held", claim2, err)
	}
}

// Contract has a required file (the shape `make resource` generates for
// attachment:file:required), and Report a versioned optional image.
type Contract struct {
	gorm.Model
	Title string     `gorm:"not null"`
	Doc   types.File `gorm:"size:512;not null;uniqueIndex"`
}

type Report struct {
	gorm.Model
	Title   string       `gorm:"not null"`
	Version int          `json:"version"`
	Scan    *types.Image `gorm:"size:512;uniqueIndex"`
}

// newMemoryFileApp is a cookie-mode app on in-memory storage with the
// claims table and models migrated.
func newMemoryFileApp(t *testing.T, models ...any) (*framework.App, *database.DB) {
	t.Helper()
	db := openSQLite(t)
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(append(claims.Models(), models...)...); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.BcryptCost = bcrypt.MinCost
	cfg.Auth.AccessTokenTTL = time.Minute
	cfg.Auth.RefreshTokenTTL = time.Hour
	cfg.Auth.Mode = config.AuthModeCookie
	cfg.Storage.Driver = config.StorageDriverMemory
	app, err := framework.New(framework.WithConfig(cfg), framework.WithDatabase(db), framework.WithLogger(zap.NewNop()))
	if err != nil {
		t.Fatal(err)
	}
	return app, db
}

// TestAdminCannotCreateWithARequiredFile: a required file column the
// admin maps (a file field) is created only with a confirmed upload; one it
// does not map (explicit Fields leaving it out) cannot be set, so create is
// off by default there, refused at request time, and an explicit
// Actions.Create is a registration error. No path stores the empty key.
func TestAdminCannotCreateWithARequiredFile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app, db := newMemoryFileApp(t, &Contract{}, &Report{})
	if err := admin.Register(app, Contract{}, admin.Options{Slug: "contracts"}); err != nil {
		t.Fatal(err)
	}
	id := admin.Field{Name: "id", Type: admin.TypeInteger, ReadOnly: true}
	titled := []admin.Field{id, {Name: "title", Type: admin.TypeString}}
	if err := admin.Register(app, Contract{}, admin.Options{Slug: "contracts-titled", Fields: titled}); err != nil {
		t.Fatal(err)
	}
	explicit := admin.Options{Slug: "contracts-all", Fields: titled, Actions: admin.Actions{List: true, Detail: true, Create: true, Update: true}}
	if err := admin.Register(app, Contract{}, explicit); err == nil || !strings.Contains(err.Error(), "cannot set") {
		t.Fatalf("Register() with Create and the required file unmapped = %v, want an error", err)
	}
	jar := loginSuperuser(t, app)
	base := apiPrefix(app) + "/admin/resources/"
	if rec := doRequest(app, jar, http.MethodPost, base+"contracts", `{"title":"x"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("admin create without the required file = %d %s, want 422", rec.Code, rec.Body)
	}
	meta := doRequest(app, jar, http.MethodGet, apiPrefix(app)+"/admin/meta/contracts-titled", "")
	if !strings.Contains(meta.Body.String(), `"create":false`) || !strings.Contains(meta.Body.String(), `"delete":false`) {
		t.Fatalf("meta = %s; want create and delete off with the file unmapped", meta.Body)
	}
	for range 2 {
		if rec := doRequest(app, jar, http.MethodPost, base+"contracts-titled", `{"title":"x"}`); rec.Code != http.StatusForbidden {
			t.Fatalf("admin create with the required file unmapped = %d %s, want 403", rec.Code, rec.Body)
		}
	}
	var n int64
	db.Model(&Contract{}).Count(&n)
	if n != 0 {
		t.Fatalf("%d contracts inserted; want none (no empty keys)", n)
	}
	// An optional file is different: create stores NULL, which is valid.
	if err := admin.Register(app, Report{}, admin.Options{Slug: "reports"}); err != nil {
		t.Fatal(err)
	}
	if rec := doRequest(app, jar, http.MethodPost, base+"reports", `{"title":"r"}`); rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("admin create with an optional file = %d %s", rec.Code, rec.Body)
	}

	// A Column naming another schema field than the Name: the field is
	// the Column's, alone, so no Name/Column mix can make the file column
	// look mapped while the accessors write another.
	aliased := []admin.Field{id, {Name: "title", Type: admin.TypeString, Column: "doc"}}
	if err := admin.Register(app, Contract{}, admin.Options{Slug: "contracts-aliased", Fields: aliased}); err == nil || !strings.Contains(err.Error(), "upload protocol") {
		t.Fatalf("Register() with Name title, Column doc = %v, want the file column refused", err)
	}
	if err := admin.Register(app, Contract{}, admin.Options{Slug: "contracts-nowhere", Fields: []admin.Field{id, {Name: "title", Type: admin.TypeString, Column: "nope"}}}); err == nil {
		t.Fatal("Register() with a Column that does not exist succeeded")
	}
	swapped := []admin.Field{id, {Name: "doc", Type: admin.TypeString, Column: "title"}}
	if err := admin.Register(app, Contract{}, admin.Options{Slug: "contracts-swapped", Fields: swapped}); err != nil {
		t.Fatalf("Register() with Name doc, Column title = %v", err)
	}
	meta = doRequest(app, jar, http.MethodGet, apiPrefix(app)+"/admin/meta/contracts-swapped", "")
	if !strings.Contains(meta.Body.String(), `"create":false`) || !strings.Contains(meta.Body.String(), `"delete":false`) {
		t.Fatalf("meta = %s; want create and delete off: the doc column is not mapped", meta.Body)
	}
	if rec := doRequest(app, jar, http.MethodPost, apiPrefix(app)+"/admin/resources/contracts-swapped", `{"doc":"x"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("admin create through the swapped mapping = %d %s, want 403", rec.Code, rec.Body)
	}
	if err := admin.Register(app, Contract{}, admin.Options{Slug: "contracts-swapped-all", Fields: swapped, Actions: admin.Actions{List: true, Create: true}}); err == nil {
		t.Fatal("Register() with Create through the swapped mapping succeeded")
	}
	db.Model(&Contract{}).Count(&n)
	if n != 0 {
		t.Fatalf("%d contracts inserted; want none", n)
	}
}

// TestAdminUpdateLeavesFileColumnsAlone: an admin update never puts back a
// file key over a concurrent change. Here another writer replaces the file
// between the admin's load and its write (simulated by a callback): the
// admin's write is fenced on the keys it loaded, so it answers 409 and
// writes nothing, and the replacement stands, on the plain and the
// versioned paths.
func TestAdminUpdateLeavesFileColumnsAlone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app, db := newMemoryFileApp(t, &Attachment{}, &Report{})
	for _, reg := range []struct {
		model any
		slug  string
	}{{Attachment{}, "attachments"}, {Report{}, "reports"}} {
		if err := admin.Register(app, reg.model, admin.Options{Slug: reg.slug}); err != nil {
			t.Fatal(err)
		}
	}
	jar := loginSuperuser(t, app)
	// replace runs once, right after the admin loads the row (a query on
	// table), committing on its own: another writer's change landing
	// between the admin's load and its write.
	var replace func(tx *gorm.DB)
	var table string
	if err := db.DB.Callback().Query().After("gorm:query").Register("test:replace-file", func(tx *gorm.DB) {
		if replace != nil && tx.Statement.Table == table {
			r := replace
			replace = nil
			r(tx.Session(&gorm.Session{NewDB: true}))
		}
	}); err != nil {
		t.Fatal(err)
	}
	old, newer := types.File("attachments/file/old"), types.File("attachments/file/new")
	att := Attachment{Title: "a", File: &old}
	if err := db.Create(&att).Error; err != nil {
		t.Fatal(err)
	}
	table = "attachments"
	replace = func(tx *gorm.DB) {
		if err := tx.Exec("UPDATE attachments SET file = ? WHERE id = ?", string(newer), att.ID).Error; err != nil {
			t.Error(err)
		}
	}
	if rec := doRequest(app, jar, http.MethodPatch, fmt.Sprintf("%s/admin/resources/attachments/%d", apiPrefix(app), att.ID), `{"title":"b"}`); rec.Code != http.StatusConflict {
		t.Fatalf("PATCH racing a file replacement = %d %s, want 409", rec.Code, rec.Body)
	}
	var got Attachment
	if err := db.First(&got, att.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Title != "a" || got.File == nil || *got.File != newer {
		t.Fatalf("after the PATCH: %+v; want nothing written and the replaced file kept", got)
	}

	oldScan, newScan := types.Image("reports/scan/old"), types.Image("reports/scan/new")
	rep := Report{Title: "r", Scan: &oldScan}
	if err := db.Create(&rep).Error; err != nil {
		t.Fatal(err)
	}
	table = "reports"
	replace = func(tx *gorm.DB) {
		if err := tx.Exec("UPDATE reports SET scan = ? WHERE id = ?", string(newScan), rep.ID).Error; err != nil {
			t.Error(err)
		}
	}
	if rec := doRequest(app, jar, http.MethodPatch, fmt.Sprintf("%s/admin/resources/reports/%d", apiPrefix(app), rep.ID), fmt.Sprintf(`{"title":"s","version":%d}`, rep.Version)); rec.Code != http.StatusConflict {
		t.Fatalf("versioned PATCH racing a file replacement = %d %s, want 409", rec.Code, rec.Body)
	}
	var gotRep Report
	if err := db.First(&gotRep, rep.ID).Error; err != nil {
		t.Fatal(err)
	}
	if gotRep.Title != "r" || gotRep.Scan == nil || *gotRep.Scan != newScan {
		t.Fatalf("after the versioned PATCH: %+v; want nothing written and the replaced file kept", gotRep)
	}
}
