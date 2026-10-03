package document_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/examples/storage/internal/document"
	"github.com/gombit-dev/gombit/framework"
	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/claims"
)

// TestFileFieldFlow: the example's Document resource, end to end through
// its generated handler: grant, direct upload, create, read, and the
// refusals (a held file, a key never uploaded or outside the field, an
// empty key, and an image field's non-image bytes, which are deleted).
func TestFileFieldFlow(t *testing.T) {
	cfg := config.Default()
	cfg.Environment = config.EnvironmentTest
	cfg.Storage.Driver = config.StorageDriverMemory
	cfg.Storage.URLSecret = strings.Repeat("u", 32)
	cfg.Database = config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: "file:document-example?mode=memory&cache=shared"}
	db, err := database.Open(cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(append(claims.Models(), &document.Document{})...); err != nil {
		t.Fatal(err)
	}
	app, err := framework.New(framework.WithConfig(cfg), framework.WithDatabase(db))
	if err != nil {
		t.Fatal(err)
	}
	document.Register(app)
	api := cfg.API.Prefix + "/documents"
	call := func(method, path string, body any) (int, map[string]any) {
		var buf bytes.Buffer
		_ = json.NewEncoder(&buf).Encode(body)
		req := httptest.NewRequest(method, path, &buf)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		app.Router().ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	upload := func(field string, content []byte, contentType string) string {
		code, out := call(http.MethodPost, api+"/uploads/"+field, map[string]any{"size": len(content), "content_type": contentType, "filename": "../" + field + ".bin"})
		if code != http.StatusOK {
			t.Fatalf("grant %s = %d %v", field, code, out)
		}
		data := out["data"].(map[string]any)
		up := data["upload"].(map[string]any)
		req := httptest.NewRequest(up["method"].(string), up["url"].(string), bytes.NewReader(content))
		for k, v := range up["headers"].(map[string]any) {
			req.Header.Set(k, v.(string))
		}
		w := httptest.NewRecorder()
		app.Router().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT %s = %d %s", field, w.Code, w.Body)
		}
		return data["key"].(string)
	}
	pdf := []byte("%PDF-1.7 a small document")
	key := upload("attachment", pdf, "application/pdf")
	if !strings.HasPrefix(key, "document/attachment/") {
		t.Fatalf("key = %q", key)
	}

	code, out := call(http.MethodPost, api, map[string]any{"title": "a", "attachment": key, "cover": nil})
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, out)
	}
	file := out["data"].(map[string]any)["attachment"].(map[string]any)
	if file["key"] != key || file["filename"] != "attachment.bin" || file["size"] != float64(len(pdf)) || !strings.Contains(file["url"].(string), "signature=") {
		t.Fatalf("created attachment = %v", file)
	}
	id := strconv.Itoa(int(out["data"].(map[string]any)["id"].(float64)))
	code, out = call(http.MethodGet, api+"/"+id, nil)
	if code != http.StatusOK || out["data"].(map[string]any)["attachment"].(map[string]any)["key"] != key {
		t.Fatalf("get = %d %v", code, out)
	}
	if _, out = call(http.MethodGet, api, nil); len(out["data"].([]any)) != 1 {
		t.Fatalf("list = %v", out)
	}

	// Another record cannot take the file; a key never uploaded is refused.
	if code, out := call(http.MethodPost, api, map[string]any{"title": "b", "attachment": key, "cover": nil}); code != http.StatusConflict {
		t.Fatalf("a held file = %d %v, want 409", code, out)
	}
	var claim claims.Claim
	if err := db.Where("object_key = ?", key).Take(&claim).Error; err != nil || claim.State != claims.Held {
		t.Fatalf("the attached file's claim = %+v, %v; want held", claim, err)
	}
	if ok, _ := storage.Exists(t.Context(), app.Storage(), key); !ok {
		t.Fatal("refusing a held file deleted it")
	}
	if code, out := call(http.MethodPost, api, map[string]any{"title": "c", "attachment": "document/attachment/never", "cover": nil}); code != http.StatusUnprocessableEntity {
		t.Fatalf("a key never uploaded = %d %v, want 422", code, out)
	}
	if code, out := call(http.MethodPost, api, map[string]any{"title": "d", "attachment": "other/prefix/x", "cover": nil}); code != http.StatusUnprocessableEntity {
		t.Fatalf("a key outside the field = %d %v, want 422", code, out)
	}
	if code, out := call(http.MethodPost, api, map[string]any{"title": "d", "attachment": key, "cover": ""}); code != http.StatusUnprocessableEntity {
		t.Fatalf("an empty cover key = %d %v, want 422", code, out)
	}

	// An image field refuses a file whose bytes are not an image, and
	// deletes it.
	html := upload("cover", []byte("<!DOCTYPE html><script>alert(1)</script>"), "image/png")
	key2 := upload("attachment", pdf, "application/pdf")
	if code, out := call(http.MethodPost, api, map[string]any{"title": "e", "attachment": key2, "cover": html}); code != http.StatusUnprocessableEntity {
		t.Fatalf("HTML as an image = %d %v, want 422", code, out)
	}
	if ok, _ := storage.Exists(t.Context(), app.Storage(), html); ok {
		t.Fatal("the refused cover was kept")
	}
	// A grant over the field's limits is refused before any upload.
	if code, _ := call(http.MethodPost, api+"/uploads/cover", map[string]any{"size": 1, "content_type": "text/html"}); code != http.StatusUnprocessableEntity {
		t.Fatalf("a grant for HTML as a cover = %d, want 422", code)
	}
}

// TestOpenAPIDescribesFiles: the contract names the upload operations and
// the file shapes, so the TypeScript client is typed for them.
func TestOpenAPIDescribesFiles(t *testing.T) {
	cfg := config.Default()
	cfg.Environment = config.EnvironmentTest
	cfg.Storage.Driver = config.StorageDriverMemory
	db, err := database.Open(config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: "file:document-openapi?mode=memory&cache=shared"})
	if err != nil {
		t.Fatal(err)
	}
	app, err := framework.New(framework.WithConfig(cfg), framework.WithDatabase(db))
	if err != nil {
		t.Fatal(err)
	}
	document.Register(app)
	spec, err := json.Marshal(app.API().OpenAPI())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"/api/v1/documents/uploads/attachment"`,
		`"/api/v1/documents/uploads/cover"`,
		`"FileInfo"`,
		`"UploadGrant"`,
		`"UploadGrantRequest"`,
		`"UploadRequest"`,
	} {
		if !strings.Contains(string(spec), want) {
			t.Errorf("OpenAPI lacks %s", want)
		}
	}
}
