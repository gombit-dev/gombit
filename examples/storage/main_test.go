package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/claims"
	"github.com/gombit-dev/gombit/storage/memory"
	"github.com/gombit-dev/gombit/storage/presign"
)

// newFilesFor is the example's records and claims in a fresh SQLite
// database, over store.
func newFilesFor(t *testing.T, store storage.Storage, max int) *files {
	t.Helper()
	db, err := database.Open(config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: "file:" + t.TempDir() + "/example.db?_fk=1&_busy_timeout=5000"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fs, err := newFiles(db.DB, store, max)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func newServer(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// What framework.New does for the memory and local drivers.
	signer, err := presign.New(presign.Config{Base: "/_storage", Secret: []byte(strings.Repeat("k", 32)), PublicPrefix: "public/"})
	if err != nil {
		t.Fatal(err)
	}
	store := memory.New(memory.WithURLs(signer))
	h := gin.WrapH(presign.Handler(store, signer))
	r.GET("/_storage/*key", h)
	r.PUT("/_storage/*key", h)
	register(r, newFilesFor(t, store, 100))
	return r
}

func do(r http.Handler, method, path, body, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestUploadDownloadDelete(t *testing.T) {
	r := newServer(t)
	if w := do(r, http.MethodPut, "/files/photo", "jpeg bytes", "image/jpeg"); w.Code != http.StatusCreated {
		t.Fatalf("PUT = %d %s", w.Code, w.Body)
	}
	w := do(r, http.MethodGet, "/files/photo", "", "")
	if w.Code != http.StatusOK || w.Body.String() != "jpeg bytes" || w.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("GET = %d %q (%s)", w.Code, w.Body, w.Header().Get("Content-Type"))
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("Content-Disposition = %q, want an attachment (uploaded HTML must not render inline)", cd)
	}
	if w := do(r, http.MethodDelete, "/files/photo", "", ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", w.Code)
	}
	w = do(r, http.MethodGet, "/files/photo", "", "")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"not_found"`) {
		t.Fatalf("GET after delete = %d %s, want a D10 not_found", w.Code, w.Body)
	}
}

func TestUploadErrors(t *testing.T) {
	r := newServer(t)
	// A dot segment cannot form a valid key: a 404, not a 500.
	if w := do(r, http.MethodGet, "/files/..", "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("GET /files/.. = %d %s", w.Code, w.Body)
	}
	if w := do(r, http.MethodPut, "/files/bad", "x", "not a media type"); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PUT with a bad content type = %d %s", w.Code, w.Body)
	}
	big := bytes.Repeat([]byte("x"), maxUpload+1)
	if w := do(r, http.MethodPut, "/files/big", string(big), "application/octet-stream"); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("PUT over the limit = %d %s", w.Code, w.Body)
	}
	if w := do(r, http.MethodGet, "/files/big", "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("the oversized upload was stored (%d)", w.Code)
	}
}

func formUpload(t *testing.T, r http.Handler, filename string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(body)
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/uploads", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestFormUpload(t *testing.T) {
	r := newServer(t)
	gif := []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;")
	w := formUpload(t, r, "../../cat.gif", gif)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /uploads = %d %s", w.Code, w.Body)
	}
	var resp struct {
		Data struct{ ID, Filename, ContentType string } `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Data.Filename != "cat.gif" || strings.Contains(resp.Data.ID, "cat") {
		t.Fatalf("stored %+v; the client's name must be metadata, not the key", resp.Data)
	}
	w = do(r, http.MethodGet, "/uploads/"+resp.Data.ID, "", "")
	if w.Code != http.StatusOK || w.Body.String() != string(gif) || w.Header().Get("Content-Type") != "image/gif" {
		t.Fatalf("GET = %d (%s)", w.Code, w.Header().Get("Content-Type"))
	}
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename=cat.gif` {
		t.Fatalf("Content-Disposition = %q", cd)
	}

	// A signed link downloads it without the app's handler.
	w = do(r, http.MethodGet, "/uploads/"+resp.Data.ID+"/link", "", "")
	var link struct {
		Data struct {
			URL       string `json:"url"`
			ExpiresIn int    `json:"expires_in"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &link); err != nil || w.Code != http.StatusOK || link.Data.ExpiresIn != 300 {
		t.Fatalf("GET link = %d %s", w.Code, w.Body)
	}
	if w := do(r, http.MethodGet, link.Data.URL, "", ""); w.Code != http.StatusOK || w.Body.String() != string(gif) {
		t.Fatalf("GET %s = %d", link.Data.URL, w.Code)
	}
	unsigned := strings.SplitN(link.Data.URL, "?", 2)[0]
	if w := do(r, http.MethodGet, unsigned, "", ""); w.Code != http.StatusForbidden {
		t.Fatalf("GET without the signature = %d, want 403", w.Code)
	}

	// HTML with an image's name is refused by its bytes.
	if w := formUpload(t, r, "cat.png", []byte("<html><script>alert(1)</script>")); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("HTML named .png = %d %s", w.Code, w.Body)
	}
	if w := formUpload(t, r, "big.gif", append(gif, bytes.Repeat([]byte{0}, maxUpload)...)); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized upload = %d %s", w.Code, w.Body)
	}
}

func TestDirectUpload(t *testing.T) {
	r := newServer(t)
	gif := []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;")
	w := do(r, http.MethodPost, "/uploads/direct", fmt.Sprintf(`{"size":%d,"content_type":"image/gif","filename":"../cat.gif"}`, len(gif)), "application/json")
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /uploads/direct = %d %s", w.Code, w.Body)
	}
	var grant struct {
		Data struct {
			ID     string                `json:"id"`
			Upload storage.UploadRequest `json:"upload"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}
	// The client's PUT, with the grant: to the app's storage route, for the
	// local driver this test runs on (S3 would take it directly).
	req := httptest.NewRequest(grant.Data.Upload.Method, grant.Data.Upload.URL, bytes.NewReader(gif))
	for k, v := range grant.Data.Upload.Header {
		req.Header.Set(k, v)
	}
	put := httptest.NewRecorder()
	r.ServeHTTP(put, req)
	if put.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", put.Code, put.Body)
	}
	w = do(r, http.MethodPost, "/uploads/direct/"+grant.Data.ID+"/confirm", "", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"filename":"cat.gif"`) {
		t.Fatalf("confirm = %d %s", w.Code, w.Body)
	}
	if w := do(r, http.MethodPost, "/uploads/direct", `{"size":99999999,"content_type":"image/gif"}`, "application/json"); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a grant over the limit = %d %s", w.Code, w.Body)
	}
	if w := do(r, http.MethodPost, "/uploads/direct/nothing-here/confirm", "", ""); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("confirming nothing = %d %s", w.Code, w.Body)
	}
}

// TestCleanup: the three cleanup cases of the example.
func TestCleanup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := memory.New()
	fs := newFilesFor(t, store, 1)
	r := gin.New()
	register(r, fs)
	gif := []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;")
	ctx := context.Background()

	// A record's insert fails: the file just stored is deleted. Its claim
	// stays as a tombstone (deleting) until the upload's lease ends.
	if w := formUpload(t, r, "a.gif", gif); w.Code != http.StatusCreated {
		t.Fatalf("first upload = %d", w.Code)
	}
	if w := formUpload(t, r, "b.gif", gif); w.Code != http.StatusConflict {
		t.Fatalf("an upload whose record cannot be written = %d %s", w.Code, w.Body)
	}
	if n := len(store.Keys()); n != 1 {
		t.Fatalf("%d files stored, want only the recorded one", n)
	}
	var rec fileRecord
	if err := fs.db.Take(&rec).Error; err != nil {
		t.Fatal(err)
	}
	if got := claimStates(t, fs); len(got) != 2 || got[rec.Key] != claims.Held {
		t.Fatalf("claims = %v, want %s held and the failed upload's tombstone", got, rec.Key)
	}

	// Deleting the record deletes the file it holds, and its claim.
	if w := do(r, http.MethodDelete, "/"+rec.Key, "", ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", w.Code, w.Body)
	}
	if n := len(store.Keys()); n != 0 {
		t.Fatalf("%d files left after the record was deleted", n)
	}
	if got := claimStates(t, fs); len(got) != 2 || got[rec.Key] != claims.Deleting {
		t.Fatalf("claims after the delete = %v, want two tombstones", got)
	}
	if w := do(r, http.MethodDelete, "/"+rec.Key, "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("DELETE again = %d", w.Code)
	}

	// An upload no record ever held (a grant never confirmed) is swept once
	// it is past the grace period; a young one is not, nor is a file
	// without a claim.
	for _, k := range []string{images.Prefix + "abandoned", images.Prefix + "young"} {
		if err := fs.claims.Pending(ctx, k, "", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{images.Prefix + "abandoned", images.Prefix + "young", "shared/logo.png"} {
		if _, err := store.Put(ctx, k, bytes.NewReader(gif), storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := fs.db.Model(&claims.Claim{}).Where("object_key = ?", images.Prefix+"abandoned").
		Update("created_at", time.Now().Add(-3*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	res, err := fs.claims.Sweep(ctx, time.Hour)
	if err != nil || res.Abandoned != 1 {
		t.Fatalf("sweep = %+v, %v", res, err)
	}
	if keys := store.Keys(); len(keys) != 2 {
		t.Fatalf("after the sweep: %v", keys)
	}

	// Once the uploads' leases have ended, the sweep removes the
	// tombstones; the young pending claim stays.
	if err := fs.db.Model(&claims.Claim{}).Where("state = ?", claims.Deleting).
		Update("lease_until", time.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if res, err := fs.claims.Sweep(ctx, time.Hour); err != nil || res.Finished != 3 {
		t.Fatalf("sweep after the leases = %+v, %v; want the 3 tombstones finished", res, err)
	}
	if got := claimStates(t, fs); len(got) != 1 || got[images.Prefix+"young"] != claims.Pending {
		t.Fatalf("claims at the end = %v, want only the young pending one", got)
	}
}

func claimStates(t *testing.T, fs *files) map[string]string {
	t.Helper()
	var rows []claims.Claim
	if err := fs.db.Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, c := range rows {
		m[c.Key] = c.State
	}
	return m
}
