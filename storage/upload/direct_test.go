package upload_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/memory"
	"github.com/gombit-dev/gombit/storage/presign"
	"github.com/gombit-dev/gombit/storage/upload"
)

// directStore is a memory store with direct uploads, served by srv.
func directStore(t *testing.T) (*memory.Store, *httptest.Server) {
	t.Helper()
	signer, err := presign.New(presign.Config{Base: "/_storage", Secret: []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	store := memory.New(memory.WithURLs(signer))
	srv := httptest.NewServer(presign.Handler(store, signer))
	t.Cleanup(srv.Close)
	return store, srv
}

// perform makes the grant's request with body, as a browser would.
func perform(t *testing.T, srv *httptest.Server, g upload.Grant, body []byte) int {
	t.Helper()
	r, _ := http.NewRequest(g.Request.Method, srv.URL+g.Request.URL, strings.NewReader(string(body)))
	for k, v := range g.Request.Header {
		r.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestDirectUploadFlow(t *testing.T) {
	store, srv := directStore(t)
	ctx := context.Background()
	g, err := upload.Authorize(ctx, store, images, int64(len(png)), "image/png", `C:\photos\me.png`)
	if err != nil {
		t.Fatal(err)
	}
	if !generatedKey.MatchString(g.Key) || g.Request.Method != http.MethodPut {
		t.Fatalf("grant = %+v", g)
	}
	if got := time.Until(g.Request.Expires); got < upload.DefaultGrantExpiry-time.Minute || got > upload.DefaultGrantExpiry+time.Second {
		t.Fatalf("the grant lives %s, want about %s", got, upload.DefaultGrantExpiry)
	}
	if _, err := upload.Confirm(ctx, store, g.Key, images); !errors.Is(err, upload.ErrNoFile) {
		t.Fatalf("Confirm before the upload = %v, want ErrNoFile", err)
	}
	if code := perform(t, srv, g, png); code != http.StatusOK {
		t.Fatalf("the direct upload = %d", code)
	}
	f, err := upload.Confirm(ctx, store, g.Key, images)
	if err != nil {
		t.Fatal(err)
	}
	if f.Key != g.Key || f.Size != int64(len(png)) || f.ContentType != "image/png" || f.Filename() != "me.png" {
		t.Fatalf("confirmed %+v", f)
	}
}

func TestAuthorizeRefuses(t *testing.T) {
	store, _ := directStore(t)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		size int64
		typ  string
		want error
	}{
		"too large":        {images.MaxBytes + 1, "image/png", upload.ErrTooLarge},
		"a negative size":  {-1, "image/png", upload.ErrMalformed},
		"a type not asked": {10, "text/html", upload.ErrType},
		"not a type":       {10, "png", upload.ErrType},
		"no type":          {10, "", upload.ErrType},
	} {
		if _, err := upload.Authorize(ctx, store, images, tc.size, tc.typ, ""); !errors.Is(err, tc.want) {
			t.Errorf("%s: Authorize = %v, want %v", name, err, tc.want)
		}
	}
	if _, err := upload.Authorize(ctx, memory.New(), images, 10, "image/png", ""); !errors.Is(err, storage.ErrUnsupported) {
		t.Fatalf("a store without direct uploads = %v, want ErrUnsupported", err)
	}
	p := images
	p.GrantExpiry = storage.MaxURLExpiry + time.Second
	if _, err := upload.Authorize(ctx, store, p, 10, "image/png", ""); err == nil {
		t.Fatal("a grant lifetime over the maximum was accepted")
	}
}

// TestConfirmChecksTheBytes: what the client declared is a claim; Confirm
// judges what arrived and deletes what the policy refuses.
func TestConfirmChecksTheBytes(t *testing.T) {
	store, srv := directStore(t)
	ctx := context.Background()
	html := []byte("<!DOCTYPE html><script>alert(1)</script>")
	g, err := upload.Authorize(ctx, store, images, int64(len(html)), "image/png", "cat.png")
	if err != nil {
		t.Fatal(err)
	}
	if code := perform(t, srv, g, html); code != http.StatusOK {
		t.Fatalf("upload = %d", code)
	}
	if _, err := upload.Confirm(ctx, store, g.Key, images); !errors.Is(err, upload.ErrType) {
		t.Fatalf("HTML declared as image/png = %v, want ErrType", err)
	}
	if ok, _ := storage.Exists(ctx, store, g.Key); ok {
		t.Fatal("the refused upload was kept")
	}

	// A JPEG declared as PNG: both allowed, but the store would serve it
	// under the wrong type.
	jpeg := append([]byte("\xff\xd8\xff"), make([]byte, 40)...)
	g, _ = upload.Authorize(ctx, store, images, int64(len(jpeg)), "image/png", "")
	perform(t, srv, g, jpeg)
	if _, err := upload.Confirm(ctx, store, g.Key, images); !errors.Is(err, upload.ErrType) {
		t.Fatalf("a JPEG declared as PNG = %v, want ErrType", err)
	}

	// A file over a tighter policy than the grant's.
	g, _ = upload.Authorize(ctx, store, images, int64(len(png)), "image/png", "")
	perform(t, srv, g, png)
	tight := images
	tight.MaxBytes = 10
	if _, err := upload.Confirm(ctx, store, g.Key, tight); !errors.Is(err, upload.ErrTooLarge) {
		t.Fatalf("Confirm over MaxBytes = %v, want ErrTooLarge", err)
	}
	if ok, _ := storage.Exists(ctx, store, g.Key); ok {
		t.Fatal("the oversized upload was kept")
	}

	if _, err := upload.Confirm(ctx, store, "elsewhere/secret", images); !errors.Is(err, upload.ErrMalformed) {
		t.Fatalf("a key outside the prefix = %v, want ErrMalformed", err)
	}
}

// TestConfirmReportsAFailedCleanup: when Confirm refuses an uploaded file
// and cannot delete it, the error names the key that still holds it
// (*upload.CleanupError), as every upload's cleanup does.
func TestConfirmReportsAFailedCleanup(t *testing.T) {
	store, srv := directStore(t)
	ctx := context.Background()
	html := []byte("<!DOCTYPE html><script>alert(1)</script>")
	g, err := upload.Authorize(ctx, store, images, int64(len(html)), "image/png", "cat.png")
	if err != nil {
		t.Fatal(err)
	}
	if code := perform(t, srv, g, html); code != http.StatusOK {
		t.Fatalf("upload = %d", code)
	}
	deleteErr := errors.New("delete failed")
	_, err = upload.Confirm(ctx, failingDelete{Storage: store, err: deleteErr}, g.Key, images)
	var cleanup *upload.CleanupError
	if !errors.Is(err, upload.ErrType) || !errors.As(err, &cleanup) || cleanup.Key != g.Key || !errors.Is(cleanup.Err, deleteErr) {
		t.Fatalf("Confirm = %v; want ErrType and an *upload.CleanupError for %q", err, g.Key)
	}
}
