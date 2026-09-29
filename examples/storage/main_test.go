package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/gombit-dev/gombit/storage/memory"
)

func newServer(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	register(r, memory.New())
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

	// HTML with an image's name is refused by its bytes.
	if w := formUpload(t, r, "cat.png", []byte("<html><script>alert(1)</script>")); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("HTML named .png = %d %s", w.Code, w.Body)
	}
	if w := formUpload(t, r, "big.gif", append(gif, bytes.Repeat([]byte{0}, maxUpload)...)); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized upload = %d %s", w.Code, w.Body)
	}
}
