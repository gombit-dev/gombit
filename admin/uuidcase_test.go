package admin_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/framework"
	"github.com/google/uuid"
)

// uuidDoc keys by a char(36) uuid.UUID, as `gombit make resource --id uuid`
// scaffolds it: text on every driver, compared byte for byte on SQLite and
// PostgreSQL. uuid.UUID always writes the canonical lower case.
type uuidDoc struct {
	ID    uuid.UUID `gorm:"type:char(36);primaryKey" json:"id"`
	Title string    `json:"title"`
	Ref   uuid.UUID `gorm:"type:char(36)" json:"ref"`
	Tags  []uuidTag `gorm:"many2many:uuid_doc_tags;" json:"tags"`
}

type uuidTag struct {
	ID   uuid.UUID `gorm:"type:char(36);primaryKey" json:"id"`
	Name string    `json:"name"`
}

// uuidNote belongs to a uuidDoc by a uuid.UUID key.
type uuidNote struct {
	ID    uint      `gorm:"primaryKey" json:"id"`
	DocID uuid.UUID `gorm:"type:char(36)" json:"doc_id"`
	Doc   uuidDoc   `json:"-"`
}

// stringUUIDDoc keys by a Go string declared TypeUUID: the column holds the
// spelling written, which the admin keeps.
type stringUUIDDoc struct {
	ID    string `gorm:"type:char(36);primaryKey" json:"id"`
	Title string `json:"title"`
}

// TestResourceUUIDIsCaseInsensitive: a uuid over a uuid.UUID, spelled in upper
// case or another form uuid.Parse accepts, finds the row it names, as a path
// id, a filter, a many-to-many id and a belongs_to key; a uuid over a string
// keeps the spelling sent (issue #457). TestUUIDCaseAdminPostgres / MySQL run
// it on the matrix.
func TestResourceUUIDIsCaseInsensitive(t *testing.T) {
	runUUIDCaseAdmin(t, openSQLite(t))
}

func runUUIDCaseAdmin(t *testing.T, db *database.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	models := []any{&uuidNote{}, &uuidDoc{}, &uuidTag{}, &stringUUIDDoc{}}
	drop := func() {
		_ = db.Migrator().DropTable("uuid_doc_tags")
		_ = db.Migrator().DropTable(models...)
	}
	drop()
	t.Cleanup(drop)
	app := newCookieAppWithDB(t, db)
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	for _, reg := range []struct {
		model any
		opts  admin.Options
	}{
		{uuidTag{}, admin.Options{Slug: "uuid-tags"}},
		{uuidDoc{}, admin.Options{Slug: "uuid-docs", Filter: []string{"id", "ref"}, Fields: []admin.Field{
			{Name: "id", Type: admin.TypeUUID},
			{Name: "title", Type: admin.TypeString},
			{Name: "ref", Type: admin.TypeUUID},
			{Name: "tags", Type: admin.TypeRelation, Related: &admin.Relation{Kind: admin.RelManyToMany, Slug: "uuid-tags"}},
		}}},
		{uuidNote{}, admin.Options{Slug: "uuid-notes", Filter: []string{"doc_id"}, Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "doc_id", Type: admin.TypeRelation, Related: &admin.Relation{Kind: admin.RelBelongsTo, Slug: "uuid-docs"}},
		}}},
		{stringUUIDDoc{}, admin.Options{Slug: "string-uuid-docs", Filter: []string{"id"}, Fields: []admin.Field{
			{Name: "id", Type: admin.TypeUUID},
			{Name: "title", Type: admin.TypeString},
		}}},
	} {
		if err := admin.Register(app, reg.model, reg.opts); err != nil {
			t.Fatalf("admin.Register(%s): %v", reg.opts.Slug, err)
		}
	}
	jar := loginSuperuser(t, app)
	const (
		id    = "3f2b8c1a-9d4e-4f6a-8b7c-1a2b3c4d5e6f"
		ref   = "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"
		tagID = "0c9e6f4a-2b1d-4e3f-9a8b-7c6d5e4f3a2b"
	)
	upper := strings.ToUpper(id)
	if err := db.Create(&uuidTag{ID: uuid.MustParse(tagID), Name: "t"}).Error; err != nil {
		t.Fatalf("seed tag: %v", err)
	}

	// Written values are canonical: the raw column holds the lower case.
	create := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/uuid-docs",
		`{"id":"`+upper+`","title":"t","ref":"{`+strings.ToUpper(ref)+`}","tags":["`+strings.ToUpper(tagID)+`"]}`)
	if create.Code != http.StatusOK {
		t.Fatalf("create status = %d; body: %s", create.Code, create.Body.String())
	}
	var rawID, rawRef, rawTag string
	if err := db.Raw("SELECT id, ref FROM uuid_docs").Row().Scan(&rawID, &rawRef); err != nil || rawID != id || rawRef != ref {
		t.Fatalf("stored id, ref = %q, %q (err %v), want %q, %q", rawID, rawRef, err, id, ref)
	}
	if err := db.Raw("SELECT uuid_tag_id FROM uuid_doc_tags").Row().Scan(&rawTag); err != nil || rawTag != tagID {
		t.Fatalf("stored tag id = %q (err %v), want %q", rawTag, err, tagID)
	}

	for _, path := range []string{id, upper, "urn:uuid:" + upper, strings.ReplaceAll(upper, "-", "")} {
		if rec := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/uuid-docs/"+path, ""); rec.Code != http.StatusOK {
			t.Errorf("GET %s status = %d; body: %s", path, rec.Code, rec.Body.String())
		}
	}
	assertListTotal(t, app, jar, "uuid-docs?id="+upper, 1)
	assertListTotal(t, app, jar, "uuid-docs?id={"+id+"}", 1)
	assertListTotal(t, app, jar, "uuid-docs?ref="+strings.ToUpper(ref), 1)

	// A belongs_to key written and filtered in upper case.
	note := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/uuid-notes", `{"doc_id":"`+upper+`"}`)
	if note.Code != http.StatusOK {
		t.Fatalf("create note status = %d; body: %s", note.Code, note.Body.String())
	}
	assertListTotal(t, app, jar, "uuid-notes?doc_id="+upper, 1)
	if err := db.Delete(&uuidNote{}, "doc_id = ?", id).Error; err != nil {
		t.Fatalf("clear notes: %v", err)
	}

	// The stored key in another spelling is the same key: left as loaded.
	if rec := doRequest(app, jar, http.MethodPatch, "/api/v1/admin/resources/uuid-docs/"+upper, `{"id":"`+upper+`","title":"u","tags":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("PATCH %s status = %d; body: %s", upper, rec.Code, rec.Body.String())
	}
	if rec := doRequest(app, jar, http.MethodDelete, "/api/v1/admin/resources/uuid-docs/"+upper, ""); rec.Code != http.StatusOK {
		t.Fatalf("DELETE %s status = %d; body: %s", upper, rec.Code, rec.Body.String())
	}
	var n int64
	if err := db.Model(&uuidDoc{}).Count(&n).Error; err != nil || n != 0 {
		t.Fatalf("rows after delete = %d (err %v), want 0", n, err)
	}

	// A uuid over a string keeps the spelling it was written in, so a row
	// another writer stored in upper case stays reachable by that spelling.
	sUpper := strings.ToUpper(ref)
	if err := db.Create(&stringUUIDDoc{ID: sUpper, Title: "legacy"}).Error; err != nil {
		t.Fatalf("seed string-keyed row: %v", err)
	}
	if rec := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/string-uuid-docs/"+sUpper, ""); rec.Code != http.StatusOK {
		t.Errorf("GET string-keyed %s status = %d; body: %s", sUpper, rec.Code, rec.Body.String())
	}
	assertListTotal(t, app, jar, "string-uuid-docs?id="+sUpper, 1)
	if rec := doRequest(app, jar, http.MethodPatch, "/api/v1/admin/resources/string-uuid-docs/"+sUpper, `{"title":"kept","id":"`+sUpper+`"}`); rec.Code != http.StatusOK {
		t.Errorf("PATCH string-keyed %s status = %d; body: %s", sUpper, rec.Code, rec.Body.String())
	}
	// Another spelling of a string key is another key, as its lookups say:
	// a PATCH sending it is refused, not dropped as unchanged.
	rec := doRequest(app, jar, http.MethodPatch, "/api/v1/admin/resources/string-uuid-docs/"+sUpper, `{"id":"`+ref+`"}`)
	assertError(t, rec, http.StatusUnprocessableEntity, contract.CodeValidationError)
	created := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/string-uuid-docs", `{"id":"`+upper+`","title":"new"}`)
	if created.Code != http.StatusOK {
		t.Fatalf("create string-keyed status = %d; body: %s", created.Code, created.Body.String())
	}
	var rawStringID string
	if err := db.Raw("SELECT id FROM string_uuid_docs WHERE title = ?", "new").Row().Scan(&rawStringID); err != nil || rawStringID != upper {
		t.Fatalf("stored string id = %q (err %v), want the spelling sent %q", rawStringID, err, upper)
	}
}

// assertListTotal lists /api/v1/admin/resources/<query> and checks meta.total.
func assertListTotal(t *testing.T, app *framework.App, jar *cookieJar, query string, want int64) {
	t.Helper()
	rec := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/"+query, "")
	var env listEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || rec.Code != http.StatusOK || env.Meta == nil || env.Meta.Total != want {
		t.Errorf("list %s status = %d, err %v, want total %d; body: %s", query, rec.Code, err, want, rec.Body.String())
	}
}
