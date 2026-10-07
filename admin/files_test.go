package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/framework"
	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/claims"
	"github.com/gombit-dev/gombit/storage/memory"
	"github.com/gombit-dev/gombit/storage/upload"
	"github.com/gombit-dev/gombit/types"
)

// Paper has a required file and an optional image, each owning its prefix.
type Paper struct {
	gorm.Model
	Title string       `gorm:"not null"`
	Doc   types.File   `gorm:"size:512;not null;uniqueIndex" storage:"prefix=papers/doc/"`
	Photo *types.Image `gorm:"size:512;uniqueIndex" storage:"prefix=papers/photo/;max_bytes=4096"`
}

var (
	pdfBytes = []byte("%PDF-1.7 a paper")
	pngBytes = append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), bytes.Repeat([]byte{7}, 60)...)
)

// newFileApp is a cookie-mode app with Paper registered and in-memory
// storage (URLs signed with a key derived from the JWT secret).
func newFileApp(t *testing.T) *framework.App {
	t.Helper()
	db := openSQLite(t)
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(append(claims.Models(), &Paper{})...); err != nil {
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
	if err := admin.Register(app, Paper{}, admin.Options{Slug: "papers"}); err != nil {
		t.Fatal(err)
	}
	return app
}

// uploadFile asks the admin for a grant for field, and makes the upload.
func uploadFile(t *testing.T, app *framework.App, jar *cookieJar, field string, content []byte, contentType string) string {
	t.Helper()
	body := fmt.Sprintf(`{"size":%d,"content_type":%q,"filename":"%s.bin"}`, len(content), contentType, field)
	rec := doRequest(app, jar, http.MethodPost, apiPrefix(app)+"/admin/resources/papers/uploads/"+field, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("grant %s = %d %s", field, rec.Code, rec.Body)
	}
	var grant struct {
		Data struct {
			Key    string `json:"key"`
			Upload struct {
				Method  string            `json:"method"`
				URL     string            `json:"url"`
				Headers map[string]string `json:"headers"`
			} `json:"upload"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(grant.Data.Upload.Method, grant.Data.Upload.URL, bytes.NewReader(content))
	for k, v := range grant.Data.Upload.Headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, req) // no cookie, no CSRF token: the signed URL is the authorization
	if w.Code != http.StatusOK {
		t.Fatalf("PUT %s = %d %s", field, w.Code, w.Body)
	}
	return grant.Data.Key
}

func exists(t *testing.T, app *framework.App, key string) bool {
	t.Helper()
	ok, err := storage.Exists(context.Background(), app.Storage(), key)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func fileOf(t *testing.T, row map[string]any, name string) map[string]any {
	t.Helper()
	f, ok := row[name].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want a file object", name, row[name])
	}
	return f
}

func TestAdminFileFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newFileApp(t)
	jar := loginSuperuser(t, app)
	base := apiPrefix(app) + "/admin/resources/papers"

	// The meta tells the form what each file field accepts.
	rec := doRequest(app, jar, http.MethodGet, apiPrefix(app)+"/admin/meta/papers", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"type":"image"`) || !strings.Contains(rec.Body.String(), `"max_bytes":4096`) {
		t.Fatalf("meta = %d %s", rec.Code, rec.Body)
	}

	doc := uploadFile(t, app, jar, "doc", pdfBytes, "application/pdf")
	photo := uploadFile(t, app, jar, "photo", pngBytes, "image/png")
	rec = doRequest(app, jar, http.MethodPost, base, fmt.Sprintf(`{"title":"a","doc":%q,"photo":%q}`, doc, photo))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var created rowEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if f := fileOf(t, created.Data, "doc"); f["key"] != doc || f["filename"] != "doc.bin" || !strings.Contains(f["url"].(string), "signature=") {
		t.Fatalf("doc = %v", f)
	}
	id := fmt.Sprint(asInt(created.Data["id"]))

	// Reads carry file objects too.
	rec = doRequest(app, jar, http.MethodGet, base+"/"+id, "")
	var got rowEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if fileOf(t, got.Data, "photo")["key"] != photo {
		t.Fatalf("get = %s", rec.Body)
	}
	rec = doRequest(app, jar, http.MethodGet, base, "")
	var listed listEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	if len(listed.Data) != 1 || fileOf(t, listed.Data[0], "doc")["key"] != doc {
		t.Fatalf("list = %s", rec.Body)
	}

	// Sending the file object back keeps the file.
	docObj, _ := json.Marshal(fileOf(t, got.Data, "doc"))
	rec = doRequest(app, jar, http.MethodPatch, base+"/"+id, fmt.Sprintf(`{"title":"b","doc":%s}`, docObj))
	if rec.Code != http.StatusOK || !exists(t, app, doc) {
		t.Fatalf("patch keeping the file = %d %s", rec.Code, rec.Body)
	}

	// Replacing the photo deletes the old one once the update commits.
	photo2 := uploadFile(t, app, jar, "photo", pngBytes, "image/png")
	rec = doRequest(app, jar, http.MethodPatch, base+"/"+id, fmt.Sprintf(`{"photo":%q}`, photo2))
	if rec.Code != http.StatusOK || exists(t, app, photo) || !exists(t, app, photo2) {
		t.Fatalf("replace = %d %s (old kept: %v)", rec.Code, rec.Body, exists(t, app, photo))
	}
	// Clearing it deletes it too.
	rec = doRequest(app, jar, http.MethodPatch, base+"/"+id, `{"photo":null}`)
	if rec.Code != http.StatusOK || exists(t, app, photo2) {
		t.Fatalf("clear = %d %s", rec.Code, rec.Body)
	}

	// Refusals: another record's file, a key never uploaded, one outside
	// the field, and non-image bytes for the image (deleted).
	junk := uploadFile(t, app, jar, "photo", []byte("<!DOCTYPE html><script>x</script>"), "image/png")
	for body, want := range map[string]string{
		fmt.Sprintf(`{"title":"c","doc":%q}`, doc):                  "attached to another record",
		`{"title":"c","doc":"papers/doc/never"}`:                    "choose the file again", // never granted
		`{"title":"c","doc":"elsewhere/x"}`:                         "not an upload for this field",
		fmt.Sprintf(`{"title":"c","doc":%q,"photo":%q}`, doc, junk): "",
	} {
		rec = doRequest(app, jar, http.MethodPost, base, body)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("create %s = %d %s, want 422 %q", body, rec.Code, rec.Body, want)
		}
	}
	if exists(t, app, junk) || exists(t, app, upload.StagingKey(junk)) {
		t.Fatal("the refused image was kept (or reached its key)")
	}

	// A grant over the field's limits is refused before any upload.
	rec = doRequest(app, jar, http.MethodPost, base+"/uploads/photo", `{"size":5000,"content_type":"image/png"}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized grant = %d %s", rec.Code, rec.Body)
	}
	if rec = doRequest(app, jar, http.MethodPost, base+"/uploads/title", `{"size":1,"content_type":"text/plain"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("a grant for a non-file field = %d", rec.Code)
	}

	// Deleting the record deletes its files after the delete commits.
	rec = doRequest(app, jar, http.MethodDelete, base+"/"+id, "")
	if rec.Code != http.StatusOK || exists(t, app, doc) {
		t.Fatalf("delete = %d %s (file kept: %v)", rec.Code, rec.Body, exists(t, app, doc))
	}
}

// TestAdminUploadNeedsPermission: an operator without create or update
// permission on the model gets no upload grant.
func TestAdminUploadNeedsPermission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newFileApp(t)
	jar := loginUser(t, app, "viewer@example.com", testPassword)
	path := apiPrefix(app) + "/admin/resources/papers/uploads/doc"
	body := `{"size":10,"content_type":"application/pdf"}`
	if rec := doRequest(app, jar, http.MethodPost, path, body); rec.Code != http.StatusForbidden {
		t.Fatalf("grant without permission = %d %s", rec.Code, rec.Body)
	}
	grantGroupPermission(t, app, "viewer@example.com", "admin.papers.update")
	if rec := doRequest(app, jar, http.MethodPost, path, body); rec.Code != http.StatusOK {
		t.Fatalf("grant with update permission = %d %s", rec.Code, rec.Body)
	}
}

// Ledger is versioned (optimistic locking) and has an image.
type Ledger struct {
	gorm.Model
	Title   string       `gorm:"not null"`
	Version int          `json:"version"`
	Scan    *types.Image `gorm:"size:512;uniqueIndex" storage:"prefix=ledgers/scan/"`
}

// TestAdminFilesOnTheVersionedPath: a versioned model's update also
// accepts the new file and deletes the replaced one after committing.
func TestAdminFilesOnTheVersionedPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newFileApp(t)
	if err := app.DB().AutoMigrate(&Ledger{}); err != nil {
		t.Fatal(err)
	}
	if err := admin.Register(app, Ledger{}, admin.Options{Slug: "ledgers"}); err != nil {
		t.Fatal(err)
	}
	jar := loginSuperuser(t, app)
	grant := func(content []byte) string {
		body := fmt.Sprintf(`{"size":%d,"content_type":"image/png"}`, len(content))
		rec := doRequest(app, jar, http.MethodPost, apiPrefix(app)+"/admin/resources/ledgers/uploads/scan", body)
		var g struct {
			Data struct {
				Key    string `json:"key"`
				Upload struct {
					Method, URL string
					Headers     map[string]string
				} `json:"upload"`
			} `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &g)
		req := httptest.NewRequest(g.Data.Upload.Method, g.Data.Upload.URL, bytes.NewReader(content))
		for k, v := range g.Data.Upload.Headers {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		app.Router().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("grant/PUT = %d %s / %d %s", rec.Code, rec.Body, w.Code, w.Body)
		}
		return g.Data.Key
	}
	first := grant(pngBytes)
	base := apiPrefix(app) + "/admin/resources/ledgers"
	rec := doRequest(app, jar, http.MethodPost, base, fmt.Sprintf(`{"title":"l","scan":%q}`, first))
	var created rowEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := fmt.Sprint(asInt(created.Data["id"]))
	second := grant(pngBytes)
	rec = doRequest(app, jar, http.MethodPatch, base+"/"+id, fmt.Sprintf(`{"scan":%q,"version":%d}`, second, asInt(created.Data["version"])))
	if rec.Code != http.StatusOK || exists(t, app, first) || !exists(t, app, second) {
		t.Fatalf("versioned replace = %d %s (old kept: %v)", rec.Code, rec.Body, exists(t, app, first))
	}
	// A stale version refuses the whole change: the new file is
	// abandoned and the current one kept.
	third := grant(pngBytes)
	rec = doRequest(app, jar, http.MethodPatch, base+"/"+id, fmt.Sprintf(`{"scan":%q,"version":%d}`, third, asInt(created.Data["version"])))
	if rec.Code != http.StatusConflict || exists(t, app, third) || !exists(t, app, second) {
		t.Fatalf("a stale versioned replace = %d %s (new kept: %v, current kept: %v)", rec.Code, rec.Body, exists(t, app, third), exists(t, app, second))
	}
	var claim claims.Claim
	if err := app.DB().Where("object_key = ?", second).Take(&claim).Error; err != nil || claim.State != claims.Held {
		t.Fatalf("the current file's claim = %+v, %v; want held", claim, err)
	}
	rec = doRequest(app, jar, http.MethodPatch, base+"/"+id, `{"scan":"ledgers/scan/never"}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "choose the file again") {
		t.Fatalf("versioned update with a key never uploaded = %d %s", rec.Code, rec.Body)
	}
}

// flakyStore fails Stat while down, as an unreachable backend does.
type flakyStore struct {
	storage.Storage
	down *bool
}

func (f flakyStore) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if *f.down {
		return storage.ObjectInfo{}, storage.Wrap("stat", key, storage.ErrUnavailable)
	}
	return f.Storage.Stat(ctx, key)
}

// TestAdminListSurvivesAStorageOutage: rows still load, with the file's
// key, while the store is unreachable.
func TestAdminListSurvivesAStorageOutage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := openSQLite(t)
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(append(claims.Models(), &Paper{})...); err != nil {
		t.Fatal(err)
	}
	mem := memory.New()
	if _, err := mem.Put(context.Background(), "papers/doc/x", bytes.NewReader(pdfBytes), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Paper{Title: "a", Doc: "papers/doc/x"}).Error; err != nil {
		t.Fatal(err)
	}
	down := true
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.BcryptCost = bcrypt.MinCost
	cfg.Auth.Mode = config.AuthModeCookie
	app, err := framework.New(framework.WithConfig(cfg), framework.WithDatabase(db), framework.WithStorage(flakyStore{Storage: mem, down: &down}), framework.WithLogger(zap.NewNop()))
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Register(app, Paper{}, admin.Options{Slug: "papers"}); err != nil {
		t.Fatal(err)
	}
	rec := doRequest(app, loginSuperuser(t, app), http.MethodGet, apiPrefix(app)+"/admin/resources/papers", "")
	var listed listEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	if rec.Code != http.StatusOK || len(listed.Data) != 1 || fileOf(t, listed.Data[0], "doc")["key"] != "papers/doc/x" {
		t.Fatalf("list during an outage = %d %s", rec.Code, rec.Body)
	}
}

// Poster has two image fields that share a prefix: only the claim's scope
// tells their grants apart.
type Poster struct {
	gorm.Model
	Title string       `gorm:"not null"`
	Front *types.Image `gorm:"size:512;uniqueIndex" storage:"prefix=posters/"`
	Back  *types.Image `gorm:"size:512;uniqueIndex" storage:"prefix=posters/"`
}

// TestAdminGrantBelongsToItsField: a grant the admin issued for one field
// cannot be written into another field of the model, even under the same
// prefix with the same policy; it is refused as a field error, and its own
// field accepts it.
func TestAdminGrantBelongsToItsField(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newFileApp(t)
	if err := app.DB().AutoMigrate(&Poster{}); err != nil {
		t.Fatal(err)
	}
	if err := admin.Register(app, Poster{}, admin.Options{Slug: "posters"}); err != nil {
		t.Fatal(err)
	}
	jar := loginSuperuser(t, app)
	body := fmt.Sprintf(`{"size":%d,"content_type":"image/png"}`, len(pngBytes))
	rec := doRequest(app, jar, http.MethodPost, apiPrefix(app)+"/admin/resources/posters/uploads/front", body)
	var g struct {
		Data struct {
			Key    string `json:"key"`
			Upload struct {
				Method, URL string
				Headers     map[string]string
			} `json:"upload"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("grant = %d %s", rec.Code, rec.Body)
	}
	req := httptest.NewRequest(g.Data.Upload.Method, g.Data.Upload.URL, bytes.NewReader(pngBytes))
	for k, v := range g.Data.Upload.Headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", w.Code, w.Body)
	}
	base := apiPrefix(app) + "/admin/resources/posters"
	rec = doRequest(app, jar, http.MethodPost, base, fmt.Sprintf(`{"title":"p","back":%q}`, g.Data.Key))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "not an upload for this field") {
		t.Fatalf("the front's grant written to back = %d %s, want a 422 field error", rec.Code, rec.Body)
	}
	rec = doRequest(app, jar, http.MethodPost, base, fmt.Sprintf(`{"title":"p","front":%q}`, g.Data.Key))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("the front's grant written to front = %d %s", rec.Code, rec.Body)
	}
}

// TestAdminReplaceLosesToAConcurrentReplacement: an admin write replacing a
// file releases the old key in its transaction. If another writer released
// it meanwhile (a concurrent replacement), that fails: the admin answers
// 409, writes nothing, and its new upload is abandoned.
func TestAdminReplaceLosesToAConcurrentReplacement(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newFileApp(t)
	jar := loginSuperuser(t, app)
	base := apiPrefix(app) + "/admin/resources/papers"
	doc := uploadFile(t, app, jar, "doc", pdfBytes, "application/pdf")
	rec := doRequest(app, jar, http.MethodPost, base, fmt.Sprintf(`{"title":"a","doc":%q}`, doc))
	var created rowEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := fmt.Sprint(asInt(created.Data["id"]))
	// Another writer released the held key (its replacement committed).
	if err := app.DB().Model(&claims.Claim{}).Where("object_key = ?", doc).Update("state", claims.Deleting).Error; err != nil {
		t.Fatal(err)
	}
	doc2 := uploadFile(t, app, jar, "doc", pdfBytes, "application/pdf")
	rec = doRequest(app, jar, http.MethodPatch, base+"/"+id, fmt.Sprintf(`{"doc":%q}`, doc2))
	if rec.Code != http.StatusConflict {
		t.Fatalf("replacing a file another writer released = %d %s, want 409", rec.Code, rec.Body)
	}
	var paper Paper
	if err := app.DB().First(&paper, id).Error; err != nil || string(paper.Doc) != doc {
		t.Fatalf("the row = %+v, %v; want it unchanged", paper, err)
	}
	if exists(t, app, doc2) {
		t.Fatal("the losing write's upload was kept")
	}
}

// afterLoad runs fn once, right after the next query on table, committing on
// its own: another writer's change landing between an admin request's load
// and its write.
func afterLoad(t *testing.T, db *gorm.DB, table string, fn func(tx *gorm.DB)) {
	t.Helper()
	armed := true
	name := "test:after-load-" + t.Name()
	if err := db.Callback().Query().After("gorm:query").Register(name, func(tx *gorm.DB) {
		if armed && tx.Statement.Table == table {
			armed = false
			fn(tx.Session(&gorm.Session{NewDB: true}))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(name) })
}

// TestAdminFileRaces: the admin's writes are fenced on the file keys they
// loaded, including empty ones, so a first attach or a delete that races
// another writer's attach answers 409 and writes nothing: no file is left
// held with no row referring to it.
func TestAdminFileRaces(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newFileApp(t)
	jar := loginSuperuser(t, app)
	base := apiPrefix(app) + "/admin/resources/papers"
	doc := uploadFile(t, app, jar, "doc", pdfBytes, "application/pdf")
	rec := doRequest(app, jar, http.MethodPost, base, fmt.Sprintf(`{"title":"a","doc":%q}`, doc))
	var created rowEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := asInt(created.Data["id"])

	// First attach: the photo is empty; another writer attaches P1 after
	// the admin loaded the row, while the admin attaches P2.
	p1 := types.Image("papers/photo/p1")
	p2 := uploadFile(t, app, jar, "photo", pngBytes, "image/png")
	afterLoad(t, app.DB(), "papers", func(tx *gorm.DB) {
		if err := tx.Exec("UPDATE papers SET photo = ? WHERE id = ?", string(p1), id).Error; err != nil {
			t.Error(err)
		}
	})
	rec = doRequest(app, jar, http.MethodPatch, fmt.Sprintf("%s/%d", base, id), fmt.Sprintf(`{"photo":%q}`, p2))
	if rec.Code != http.StatusConflict {
		t.Fatalf("a first attach racing another = %d %s, want 409", rec.Code, rec.Body)
	}
	var paper Paper
	if err := app.DB().First(&paper, id).Error; err != nil || paper.Photo == nil || *paper.Photo != p1 {
		t.Fatalf("the row = %+v, %v; want the other writer's photo", paper, err)
	}
	if exists(t, app, p2) {
		t.Fatal("the losing attach's upload was kept (held by nothing)")
	}

	// Delete: the photo is cleared, the admin loads the row, another writer
	// attaches P3, then the admin deletes.
	if err := app.DB().Model(&Paper{}).Where("id = ?", id).Update("photo", nil).Error; err != nil {
		t.Fatal(err)
	}
	p3 := types.Image("papers/photo/p3")
	afterLoad(t, app.DB(), "papers", func(tx *gorm.DB) {
		if err := tx.Exec("UPDATE papers SET photo = ? WHERE id = ?", string(p3), id).Error; err != nil {
			t.Error(err)
		}
	})
	rec = doRequest(app, jar, http.MethodDelete, fmt.Sprintf("%s/%d", base, id), "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("a delete racing an attach = %d %s, want 409", rec.Code, rec.Body)
	}
	var n int64
	app.DB().Model(&Paper{}).Where("id = ?", id).Count(&n)
	if n != 1 || !exists(t, app, doc) {
		t.Fatalf("after the refused delete: %d rows, doc kept %v; want both", n, exists(t, app, doc))
	}
}

// TestAdminRequiredFileRefusesTheEmptyKey: a blank key ("" or a file object
// with an empty key) is no file. A required file field refuses it, on
// create and on update (which would otherwise release and delete the
// file); an optional one is cleared by it. A malformed file object (no key,
// a null or non-string one) is refused, and changes nothing.
func TestAdminRequiredFileRefusesTheEmptyKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newFileApp(t)
	jar := loginSuperuser(t, app)
	base := apiPrefix(app) + "/admin/resources/papers"
	for _, body := range []string{`{"title":"x","doc":""}`, `{"title":"x","doc":{"key":""}}`} {
		if rec := doRequest(app, jar, http.MethodPost, base, body); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "is required") {
			t.Fatalf("create %s = %d %s, want 422 is required", body, rec.Code, rec.Body)
		}
	}
	var n int64
	app.DB().Model(&Paper{}).Count(&n)
	if n != 0 {
		t.Fatalf("%d papers stored with an empty key", n)
	}
	doc := uploadFile(t, app, jar, "doc", pdfBytes, "application/pdf")
	photo := uploadFile(t, app, jar, "photo", pngBytes, "image/png")
	rec := doRequest(app, jar, http.MethodPost, base, fmt.Sprintf(`{"title":"a","doc":%q,"photo":%q}`, doc, photo))
	var created rowEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := fmt.Sprint(asInt(created.Data["id"]))
	for _, body := range []string{`{"doc":""}`, `{"doc":{"key":""}}`} {
		if rec := doRequest(app, jar, http.MethodPatch, base+"/"+id, body); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("update %s = %d %s, want 422", body, rec.Code, rec.Body)
		}
	}
	if !exists(t, app, doc) {
		t.Fatal("a blank key deleted the required file")
	}
	// A malformed file object is not a blank: a 422, and nothing changes.
	for _, body := range []string{`{"photo":{"key":5}}`, `{"photo":{"key":null}}`, `{"photo":{}}`, `{"photo":{"filename":"x.png"}}`, `{"doc":{"key":5}}`} {
		rec := doRequest(app, jar, http.MethodPatch, base+"/"+id, body)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "must be a string") {
			t.Fatalf("update %s = %d %s, want 422 must be a string", body, rec.Code, rec.Body)
		}
		var claim claims.Claim
		if err := app.DB().Where("object_key = ?", photo).Take(&claim).Error; err != nil || claim.State != claims.Held || !exists(t, app, photo) {
			t.Fatalf("after %s: the photo's claim %+v (%v), stored %v; want it held and kept", body, claim, err, exists(t, app, photo))
		}
	}
	if rec := doRequest(app, jar, http.MethodPatch, base+"/"+id, `{"photo":""}`); rec.Code != http.StatusOK || exists(t, app, photo) {
		t.Fatalf("clearing the optional photo = %d %s (kept %v)", rec.Code, rec.Body, exists(t, app, photo))
	}
}

// TestAdminFileFieldKindMatchesTheColumn: an explicit field on a file or
// image column is declared as the column's own kind. TypeFile on an image
// column would run the generic policy (any type, HTML included) on a column
// the model declares an image; it is a registration error, as is any other
// type.
func TestAdminFileFieldKindMatchesTheColumn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newFileApp(t)
	id := admin.Field{Name: "id", Type: admin.TypeInteger, ReadOnly: true}
	title := admin.Field{Name: "title", Type: admin.TypeString}
	doc := admin.Field{Name: "doc", Type: admin.TypeFile}
	for name, photo := range map[string]admin.Field{
		"file on an image": {Name: "photo", Type: admin.TypeFile},
		"string":           {Name: "photo", Type: admin.TypeString},
	} {
		err := admin.Register(app, Paper{}, admin.Options{Slug: "papers-" + strings.ReplaceAll(name, " ", "-"), Fields: []admin.Field{id, title, doc, photo}})
		if err == nil || !strings.Contains(err.Error(), `declare it as "image"`) {
			t.Errorf("Register() with photo as %s = %v, want an error", name, err)
		}
	}
	if err := admin.Register(app, Paper{}, admin.Options{Slug: "papers-images", Fields: []admin.Field{id, title, doc, {Name: "photo", Type: admin.TypeImage}}}); err != nil {
		t.Fatalf("Register() with photo as an image = %v", err)
	}
	if err := admin.Register(app, Paper{}, admin.Options{Slug: "papers-doc-image", Fields: []admin.Field{id, title, {Name: "doc", Type: admin.TypeImage}}}); err == nil {
		t.Fatal("Register() with a file column as an image succeeded")
	}
}

// noStore is a Host without object storage.
type noStore struct{ app *framework.App }

func (h noStore) API() huma.API         { return h.app.API() }
func (h noStore) DB() *gorm.DB          { return h.app.DB() }
func (h noStore) Config() config.Config { return h.app.Config() }
func (h noStore) Router() *gin.Engine   { return h.app.Router() }

// TestAdminFileFieldsNeedStorage: file fields are written through the
// claims, over the host's store; a host without one cannot register them
// (clearing a file or deleting a row would strand the claims).
func TestAdminFileFieldsNeedStorage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newFileApp(t)
	if err := admin.Register(noStore{app}, Paper{}, admin.Options{Slug: "papers-nostore"}); err == nil || !strings.Contains(err.Error(), "no object storage") {
		t.Fatalf("Register() of file fields on a host without storage = %v, want an error", err)
	}
}

// Notice has a hidden file column (gombit:"server": the derived fields
// leave it out), so the admin does not write it through the claims.
type Notice struct {
	gorm.Model
	Title  string      `gorm:"not null"`
	Signed *types.File `gorm:"size:512;uniqueIndex" gombit:"server" storage:"prefix=notices/signed/"`
}

// TestAdminHiddenFileColumn: a file column the admin does not map (hidden
// here) gets no admin action it cannot honour: delete is off (meta, 403)
// and asking for it is a registration error; an update leaves the column
// out of its write, so a concurrent replacement of it survives a PATCH.
func TestAdminHiddenFileColumn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newFileApp(t)
	if err := app.DB().AutoMigrate(&Notice{}); err != nil {
		t.Fatal(err)
	}
	if err := admin.Register(app, Notice{}, admin.Options{Slug: "notices"}); err != nil {
		t.Fatal(err)
	}
	explicit := admin.Options{Slug: "notices-all", Actions: admin.Actions{List: true, Detail: true, Update: true, Delete: true}}
	if err := admin.Register(app, Notice{}, explicit); err == nil || !strings.Contains(err.Error(), "cannot delete") {
		t.Fatalf("Register() with Delete and a hidden file column = %v, want an error", err)
	}
	jar := loginSuperuser(t, app)
	meta := doRequest(app, jar, http.MethodGet, apiPrefix(app)+"/admin/meta/notices", "")
	if !strings.Contains(meta.Body.String(), `"delete":false`) {
		t.Fatalf("meta = %s; want delete off", meta.Body)
	}
	old, newer := types.File("notices/signed/old"), types.File("notices/signed/new")
	n := Notice{Title: "a", Signed: &old}
	if err := app.DB().Create(&n).Error; err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("%s/admin/resources/notices/%d", apiPrefix(app), n.ID)
	if rec := doRequest(app, jar, http.MethodDelete, base, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("DELETE = %d %s, want 403", rec.Code, rec.Body)
	}
	afterLoad(t, app.DB(), "notices", func(tx *gorm.DB) {
		if err := tx.Exec("UPDATE notices SET signed = ? WHERE id = ?", string(newer), n.ID).Error; err != nil {
			t.Error(err)
		}
	})
	if rec := doRequest(app, jar, http.MethodPatch, base, `{"title":"b"}`); rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d %s", rec.Code, rec.Body)
	}
	var got Notice
	if err := app.DB().First(&got, n.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Title != "b" || got.Signed == nil || *got.Signed != newer {
		t.Fatalf("after the PATCH: %+v; want the title changed and the hidden column's replacement kept", got)
	}
}

// A record with a file field deleted between the PATCH's load and its write
// is a 404, not a file race (issue #450).
func TestFencedPatchOfADeletedRowIs404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newFileApp(t)
	jar := loginSuperuser(t, app)
	base := apiPrefix(app) + "/admin/resources/papers"
	doc := uploadFile(t, app, jar, "doc", pdfBytes, "application/pdf")
	rec := doRequest(app, jar, http.MethodPost, base, fmt.Sprintf(`{"title":"a","doc":%q}`, doc))
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var created rowEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := asInt(created.Data["id"])
	fired := false
	if err := app.DB().Callback().Update().Before("gorm:update").Register("test:delete_paper", func(tx *gorm.DB) {
		if fired || tx.Statement.Table != "papers" {
			return
		}
		fired = true
		if err := tx.Session(&gorm.Session{NewDB: true}).Exec("DELETE FROM papers WHERE id = ?", id).Error; err != nil {
			t.Errorf("concurrent delete: %v", err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	res := doRequest(app, jar, http.MethodPatch, fmt.Sprintf("%s/%d", base, id), `{"title":"edited"}`)
	assertError(t, res, http.StatusNotFound, "not_found")
}
