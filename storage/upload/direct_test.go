package upload_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	if f.Key != g.Key || f.Size != int64(len(png)) || f.ContentType != "image/png" || f.Filename != "me.png" {
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

// verifying is a store whose backend can keep more than ObjectInfo says
// (as S3 keeps unsigned headers), checked by VerifyUpload.
type verifying struct {
	*memory.Store
	err error
}

func (v verifying) VerifyUpload(context.Context, string) error { return v.err }

// TestConfirmAsksTheStoreToVerify: Confirm refuses, and deletes, an upload
// the store's VerifyUpload rejects (ErrInvalidOptions: the client set
// something the grant did not), and returns any other failure of the check
// without deleting anything.
func TestConfirmAsksTheStoreToVerify(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		err     error
		want    error
		deleted bool
	}{
		"rejected":    {fmt.Errorf("%w: the upload set Content-Encoding", storage.ErrInvalidOptions), upload.ErrMalformed, true},
		"unavailable": {storage.ErrUnavailable, storage.ErrUnavailable, false},
		"clean":       {nil, nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			store := verifying{Store: memory.New(), err: tc.err}
			if _, err := store.Put(ctx, "avatars/a", bytes.NewReader(png), storage.PutOptions{ContentType: "image/png"}); err != nil {
				t.Fatal(err)
			}
			_, err := upload.Confirm(ctx, store, "avatars/a", images)
			if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Fatalf("Confirm = %v, want %v", err, tc.want)
			}
			if exists, _ := storage.Exists(ctx, store, "avatars/a"); exists == tc.deleted {
				t.Fatalf("after Confirm the object exists = %v, want deleted = %v", exists, tc.deleted)
			}
		})
	}
}

// TestAuthorizeClaims: a direct upload's key is claimed before the grant is
// made, a failing claim makes no grant, and a grant that cannot be made
// drops its claim.
func TestAuthorizeClaims(t *testing.T) {
	store, _ := directStore(t)
	ctx := context.Background()
	cl := newFakeClaims(store)
	p := images
	p.Claims = cl
	g, err := upload.Authorize(ctx, store, p, int64(len(png)), "image/png", "a.png")
	if err != nil {
		t.Fatal(err)
	}
	if len(cl.claimed) != 1 || cl.claimed[0] != g.Key {
		t.Fatalf("claimed %v, want the granted key %s", cl.claimed, g.Key)
	}
	// Leased until the grant expires plus the time a PUT started by then
	// may take (storage.SignedUploadTimeout).
	if want := g.Request.Expires.Add(storage.SignedUploadTimeout); cl.leases[0].Before(want) {
		t.Fatalf("lease = %s, want at least %s", cl.leases[0], want)
	}
	boom := errors.New("claims: database down")
	cl.pendingErr = boom
	if _, err := upload.Authorize(ctx, store, p, int64(len(png)), "image/png", "a.png"); !errors.Is(err, boom) {
		t.Fatalf("Authorize with a failing claim = %v, want its error", err)
	}
	cl = newFakeClaims(store)
	p.Claims = cl
	if _, err := upload.Authorize(ctx, memory.New(), p, int64(len(png)), "image/png", "a.png"); !errors.Is(err, storage.ErrUnsupported) {
		t.Fatalf("Authorize on a store without direct uploads = %v", err)
	}
	if len(cl.claimed) != 1 || len(cl.pending) != 0 {
		t.Fatalf("claimed %v, still pending %v: a grant not made must drop its claim", cl.claimed, cl.pending)
	}
}

// TestConfirmUnderClaimsKeepsHeld: under Policy.Claims, a refused
// confirmation deletes the file only while its key is pending: a file a
// record holds (or one never claimed) is kept, whatever policy confirms it.
func TestConfirmUnderClaimsKeepsHeld(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	if _, err := store.Put(ctx, "avatars/a", strings.NewReader("<html>"), storage.PutOptions{ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	cl := newFakeClaims(store)
	p := images
	p.Claims = cl
	if _, err := upload.Confirm(ctx, store, "avatars/a", p); !errors.Is(err, upload.ErrType) {
		t.Fatalf("Confirm = %v, want ErrType", err)
	}
	if exists, _ := storage.Exists(ctx, store, "avatars/a"); !exists || len(cl.abandoned) != 1 {
		t.Fatalf("a held file: exists = %v, abandoned %v; want it kept, through Abandon", exists, cl.abandoned)
	}
	cl.pending["avatars/a"] = true
	if _, err := upload.Confirm(ctx, store, "avatars/a", p); !errors.Is(err, upload.ErrType) {
		t.Fatalf("Confirm = %v, want ErrType", err)
	}
	if exists, _ := storage.Exists(ctx, store, "avatars/a"); exists {
		t.Fatal("a refused pending file was kept")
	}
}
