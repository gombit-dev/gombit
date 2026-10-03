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

// TestAuthorizeClaims: under Policy.Claims a direct upload is staged: its
// key is claimed (Stage) before the grant is made, and the grant uploads
// to the staging key, never to the key. A failing claim makes no grant,
// and a grant that cannot be made drops its claim.
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
	if len(cl.claimed) != 1 || cl.claimed[0] != g.Key || !cl.staged[g.Key] {
		t.Fatalf("claimed %v (staged %v), want the granted key %s staged", cl.claimed, cl.staged, g.Key)
	}
	if !strings.Contains(g.Request.URL, "/"+upload.StagingKey(g.Key)+"?") {
		t.Fatalf("the grant uploads to %s, want the staging key %s", g.Request.URL, upload.StagingKey(g.Key))
	}
	// Leased until the grant expires plus the time a PUT started by then
	// may take through the app's route (storage.SignedUploadTimeout).
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
	p.Prefix = upload.StagingPrefix + "x/"
	if _, err := upload.Authorize(ctx, store, p, int64(len(png)), "image/png", "a.png"); err == nil {
		t.Fatal("a policy prefix under the staging prefix was accepted")
	}
}

// TestConfirmPromotesStagedUploads: under Policy.Claims, Confirm promotes
// the claim, checks the staged object, copies it to the key, and deletes
// the staged copy. A refused staged object is deleted and the claim goes
// back to pending (the grant may upload again). A key that is not a
// staged upload awaiting confirmation (promoted already, or held) is
// checked as it is, and a refusal deletes nothing.
func TestConfirmPromotesStagedUploads(t *testing.T) {
	store, srv := directStore(t)
	ctx := context.Background()
	cl := newFakeClaims(store)
	p := images
	p.Claims = cl
	exists := func(key string) bool {
		ok, err := storage.Exists(ctx, store, key)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	grant := func(body []byte) upload.Grant {
		g, err := upload.Authorize(ctx, store, p, int64(len(body)), "image/png", "a.png")
		if err != nil {
			t.Fatal(err)
		}
		if code := perform(t, srv, g, body); code != http.StatusOK {
			t.Fatalf("PUT = %d", code)
		}
		if exists(g.Key) || !exists(upload.StagingKey(g.Key)) {
			t.Fatal("the grant did not upload to the staging key only")
		}
		return g
	}

	g := grant(png)
	f, err := upload.Confirm(ctx, store, g.Key, p)
	if err != nil || f.Key != g.Key || f.Filename != "a.png" || f.Size != int64(len(png)) {
		t.Fatalf("Confirm = %+v, %v", f, err)
	}
	if !exists(g.Key) || exists(upload.StagingKey(g.Key)) || !cl.promoting[g.Key] {
		t.Fatalf("after Confirm: key %v, staged %v, promoting %v; want the key, no staged copy, promoting", exists(g.Key), exists(upload.StagingKey(g.Key)), cl.promoting[g.Key])
	}
	// A retried confirmation checks the promoted key as it is.
	if f, err := upload.Confirm(ctx, store, g.Key, p); err != nil || f.Key != g.Key {
		t.Fatalf("a retried Confirm = %+v, %v", f, err)
	}

	html := []byte("<html><script>alert(1)</script>")
	bad := grant(html)
	if _, err := upload.Confirm(ctx, store, bad.Key, p); !errors.Is(err, upload.ErrType) {
		t.Fatalf("Confirm of HTML = %v, want ErrType", err)
	}
	if exists(bad.Key) || exists(upload.StagingKey(bad.Key)) || !cl.pending[bad.Key] {
		t.Fatalf("a refused staged object: key %v, staged %v, pending %v; want neither stored, the claim pending again", exists(bad.Key), exists(upload.StagingKey(bad.Key)), cl.pending[bad.Key])
	}
	if _, err := upload.Confirm(ctx, store, bad.Key, p); !errors.Is(err, upload.ErrNoFile) {
		t.Fatalf("Confirm with nothing staged = %v, want ErrNoFile", err)
	}

	// A held key (no pending or promoting claim) that fails the policy is
	// refused, and kept.
	if _, err := store.Put(ctx, "avatars/held", strings.NewReader("<html>"), storage.PutOptions{ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	if _, err := upload.Confirm(ctx, store, "avatars/held", p); !errors.Is(err, upload.ErrType) || !exists("avatars/held") {
		t.Fatalf("Confirm of a held key failing the policy = %v (kept: %v); want ErrType, kept", err, exists("avatars/held"))
	}
}

// publishOutcome is a store (a storage.Publisher) whose Publish copies,
// then reports err, and which records the fences it is asked for.
type publishOutcome struct {
	*memory.Store
	err        error
	prepareErr error // PreparePublish fails with it, returning its token
	fenced     *[]string
}

func (o publishOutcome) PreparePublish(ctx context.Context, src, dst string) (string, error) {
	token, err := storage.PreparePublish(ctx, o.Store, src, dst)
	if err == nil && o.prepareErr != nil {
		return token, o.prepareErr
	}
	return token, err
}

func (o publishOutcome) Publish(ctx context.Context, token string) (storage.ObjectInfo, error) {
	info, err := storage.Publish(ctx, o.Store, token)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if o.err != nil {
		return storage.ObjectInfo{}, o.err
	}
	return info, nil
}

func (o publishOutcome) Fence(_ context.Context, token string) error {
	*o.fenced = append(*o.fenced, token)
	return nil
}

// TestConfirmNeverAssumesACopyFailed: a promotion's copy is recorded on the
// claim (Publishing) before it is published. One of unknown outcome leaves
// the claim promoting with that record (the copy may exist: the claims
// protocol fences it before forgetting the key), and a retried Confirm
// finds the key as it is; a definite failure is fenced, then the claim
// goes back to pending.
func TestConfirmNeverAssumesACopyFailed(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		err       error
		promoting bool
	}{
		"unknown":  {storage.Wrap("publish", "k", errors.Join(storage.ErrUnknownOutcome, storage.ErrUnavailable)), true},
		"definite": {storage.Wrap("publish", "k", storage.ErrUnavailable), false},
	} {
		t.Run(name, func(t *testing.T) {
			mem := memory.New()
			cl := newFakeClaims(mem)
			p := images
			p.Claims = cl
			key := "avatars/k"
			if err := cl.Stage(ctx, key, time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, err := mem.Put(ctx, upload.StagingKey(key), bytes.NewReader(png), storage.PutOptions{ContentType: "image/png"}); err != nil {
				t.Fatal(err)
			}
			var fenced []string
			store := publishOutcome{Store: mem, err: tc.err, fenced: &fenced}
			if _, err := upload.Confirm(ctx, store, key, p); !errors.Is(err, storage.ErrUnavailable) {
				t.Fatalf("Confirm = %v, want the copy's failure", err)
			}
			if cl.promoting[key] != tc.promoting || cl.pending[key] == tc.promoting {
				t.Fatalf("promoting %v, pending %v; want promoting = %v", cl.promoting[key], cl.pending[key], tc.promoting)
			}
			if tc.promoting {
				if cl.published[key] == "" || len(fenced) != 0 {
					t.Fatalf("recorded %q, fenced %v; want the token kept for the protocol to fence", cl.published[key], fenced)
				}
				// The copy did happen: a retry finds it.
				if f, err := upload.Confirm(ctx, mem, key, p); err != nil || f.Key != key {
					t.Fatalf("retried Confirm = %+v, %v", f, err)
				}
			} else if len(fenced) != 1 {
				t.Fatalf("fenced %v; a definite failure must be fenced before the claim is put back", fenced)
			}
		})
	}
}

// TestSaveStagesOnRemoteStores: under Policy.Claims, on a store that
// publishes remotely (a storage.Publisher, as S3 is), Save puts the file at
// the staging key and promotes it: the claimed key is written only by the
// recorded, fenceable copy.
func TestSaveStagesOnRemoteStores(t *testing.T) {
	ctx := context.Background()
	mem := memory.New()
	cl := newFakeClaims(mem)
	p := images
	p.Claims = cl
	var fenced []string
	store := publishOutcome{Store: mem, fenced: &fenced}
	f, err := upload.Save(ctx, store, bytes.NewReader(png), "a.png", p)
	if err != nil {
		t.Fatal(err)
	}
	if !cl.staged[f.Key] || !cl.promoting[f.Key] || cl.published[f.Key] == "" {
		t.Fatalf("claim of %s: staged %v, promoting %v, recorded %q; want a recorded promotion", f.Key, cl.staged[f.Key], cl.promoting[f.Key], cl.published[f.Key])
	}
	if ok, _ := storage.Exists(ctx, mem, f.Key); !ok {
		t.Fatal("the promoted file is missing")
	}
	if ok, _ := storage.Exists(ctx, mem, upload.StagingKey(f.Key)); ok {
		t.Fatal("the staged copy was left")
	}
}

// TestPromotionKeepsALeftoverPreparation: a preparation that failed but
// returned its token (something of the copy may remain) leaves the claim
// promoting with the token recorded, for the protocol to fence.
func TestPromotionKeepsALeftoverPreparation(t *testing.T) {
	ctx := context.Background()
	mem := memory.New()
	cl := newFakeClaims(mem)
	p := images
	p.Claims = cl
	key := "avatars/k"
	if err := cl.Stage(ctx, key, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Put(ctx, upload.StagingKey(key), bytes.NewReader(png), storage.PutOptions{ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	var fenced []string
	boom := errors.New("a part copy went unanswered")
	store := publishOutcome{Store: mem, prepareErr: boom, fenced: &fenced}
	if _, err := upload.Confirm(ctx, store, key, p); !errors.Is(err, boom) {
		t.Fatalf("Confirm = %v, want the preparation's failure", err)
	}
	if !cl.promoting[key] || cl.published[key] == "" {
		t.Fatalf("promoting %v, recorded %q; want the claim kept with the token", cl.promoting[key], cl.published[key])
	}
}
