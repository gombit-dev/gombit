package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/memory"
	"github.com/gombit-dev/gombit/storage/presign"
)

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
	register(r, store)
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
