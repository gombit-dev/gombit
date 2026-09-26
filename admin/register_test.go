package admin_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/admin"
	"github.com/google/uuid"
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
