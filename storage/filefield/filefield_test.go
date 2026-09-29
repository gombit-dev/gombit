package filefield_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/field"
	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/filefield"
	"github.com/gombit-dev/gombit/storage/memory"
	"github.com/gombit-dev/gombit/storage/presign"
	"github.com/gombit-dev/gombit/storage/upload"
	"github.com/gombit-dev/gombit/types"
)

type document struct {
	ID    uint
	Title string
	Cover *types.Image `gorm:"size:512;uniqueIndex"`
}

var png = append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), bytes.Repeat([]byte{7}, 100)...)

func setup(t *testing.T) (*gorm.DB, *memory.Store, upload.Policy) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&document{}); err != nil {
		t.Fatal(err)
	}
	signer, err := presign.New(presign.Config{Base: "/_storage", Secret: []byte(strings.Repeat("k", 32)), PublicPrefix: "public/"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := filefield.Policy("", field.Image, "documents/cover/")
	if err != nil {
		t.Fatal(err)
	}
	return db, memory.New(memory.WithURLs(signer)), p
}

func putFile(t *testing.T, s storage.Storage, key string, body []byte, contentType string) {
	t.Helper()
	if _, err := s.Put(context.Background(), key, bytes.NewReader(body), storage.PutOptions{ContentType: contentType, Metadata: map[string]string{storage.FilenameMetadata: "c.png"}}); err != nil {
		t.Fatal(err)
	}
}

func TestPolicy(t *testing.T) {
	p, err := filefield.Policy("prefix=docs/pdf/; max_bytes=2048; types=application/pdf, image/png", field.File, "ignored/")
	if err != nil {
		t.Fatal(err)
	}
	if p.Prefix != "docs/pdf/" || p.MaxBytes != 2048 || len(p.Types) != 2 || p.Types[1] != "image/png" {
		t.Fatalf("policy = %+v", p)
	}
	img, _ := filefield.Policy("", field.Image, "a/b/")
	fil, _ := filefield.Policy("", field.File, "a/b/")
	if img.Prefix != "a/b/" || img.MaxBytes != filefield.DefaultMaxBytes || img.Types[0] != "image/png" || fil.Types[0] != "*/*" {
		t.Fatalf("defaults: image %+v, file %+v", img, fil)
	}
	for _, tc := range []struct {
		tag  string
		kind field.Kind
	}{
		{"max_bytes=0", field.File},
		{"max_bytes=lots", field.File},
		{"maxbytes=10", field.File}, // a typo is not ignored
		{"prefix", field.File},
		{"prefix=noslash", field.File},
		{"prefix=../x/", field.File},
		{"prefix=" + strings.Repeat("p", 500) + "/", field.File},
		{"types=Image/PNG", field.Image},
		{"", field.String},
	} {
		if _, err := filefield.Policy(tc.tag, tc.kind, "d/"); err == nil {
			t.Errorf("Policy(%q, %s) succeeded", tc.tag, tc.kind)
		}
	}
	if _, err := filefield.Policy("", field.File, ""); err == nil {
		t.Error("a field without a prefix was accepted")
	}
}

func TestAccept(t *testing.T) {
	db, store, p := setup(t)
	ctx := context.Background()
	if err := filefield.Accept(ctx, db, store, &document{}, "cover", "", p); err != nil {
		t.Fatalf("no file = %v", err)
	}
	putFile(t, store, "documents/cover/good", png, "image/png")
	if err := filefield.Accept(ctx, db, store, &document{}, "cover", "documents/cover/good", p); err != nil {
		t.Fatalf("a good upload = %v", err)
	}
	key := types.Image("documents/cover/good")
	if err := db.Create(&document{Title: "a", Cover: &key}).Error; err != nil {
		t.Fatal(err)
	}
	// Another record cannot take it.
	if err := filefield.Accept(ctx, db, store, &document{}, "cover", "documents/cover/good", p); !errors.Is(err, filefield.ErrReferenced) {
		t.Fatalf("a file another record holds = %v, want ErrReferenced", err)
	}
	if ok, _ := storage.Exists(ctx, store, "documents/cover/good"); !ok {
		t.Fatal("refusing a held file deleted it")
	}
	// Nothing uploaded, outside the prefix, or failing the policy.
	for key, want := range map[string]error{
		"documents/cover/never":  upload.ErrNoFile,
		"other/cover/stolen":     upload.ErrMalformed,
		"documents/cover/script": upload.ErrType,
	} {
		if key == "documents/cover/script" {
			putFile(t, store, key, []byte("<html><script>x</script>"), "image/png")
		}
		if err := filefield.Accept(ctx, db, store, &document{}, "cover", key, p); !errors.Is(err, want) {
			t.Errorf("Accept(%q) = %v, want %v", key, err, want)
		}
	}
	if ok, _ := storage.Exists(ctx, store, "documents/cover/script"); ok {
		t.Fatal("a file failing the policy was kept")
	}
}

func TestReferencedBySweeps(t *testing.T) {
	db, _, _ := setup(t)
	ctx := context.Background()
	old := memory.New(memory.WithClock(func() time.Time { return time.Now().Add(-2 * time.Hour) }))
	for _, k := range []string{"documents/cover/kept", "documents/cover/abandoned"} {
		putFile(t, old, k, png, "image/png")
	}
	key := types.Image("documents/cover/kept")
	if err := db.Create(&document{Title: "a", Cover: &key}).Error; err != nil {
		t.Fatal(err)
	}
	res, err := storage.Sweep(ctx, old, "documents/cover/", time.Hour, filefield.ReferencedBy(db, &document{}, "cover"))
	if err != nil || res.Deleted != 1 {
		t.Fatalf("Sweep = %+v, %v", res, err)
	}
	if ok, _ := storage.Exists(ctx, old, "documents/cover/kept"); !ok {
		t.Fatal("the attached file was swept")
	}
}

func TestResolve(t *testing.T) {
	_, store, _ := setup(t)
	ctx := context.Background()
	if d, err := filefield.Resolve(ctx, store, ""); d != nil || err != nil {
		t.Fatalf("no key = %+v, %v", d, err)
	}
	putFile(t, store, "documents/cover/a", png, "image/png")
	d, err := filefield.Resolve(ctx, store, "documents/cover/a")
	if err != nil || d.Filename != "c.png" || d.Size != int64(len(png)) || d.ContentType != "image/png" || !strings.Contains(d.URL, "signature=") {
		t.Fatalf("private = %+v, %v", d, err)
	}
	putFile(t, store, "public/logo", png, "image/png")
	if d, _ := filefield.Resolve(ctx, store, "public/logo"); d.URL != "/_storage/public/logo" {
		t.Fatalf("public URL = %q", d.URL)
	}
	if d, err := filefield.Resolve(ctx, store, "documents/cover/gone"); err != nil || !d.Missing || d.Key != "documents/cover/gone" {
		t.Fatalf("missing = %+v, %v", d, err)
	}
	plain := memory.New()
	putFile(t, plain, "k", png, "image/png")
	if d, err := filefield.Resolve(ctx, plain, "k"); err != nil || d.URL != "" || d.Size == 0 {
		t.Fatalf("a store without URLs = %+v, %v", d, err)
	}
}

func TestMapError(t *testing.T) {
	ctx := context.Background()
	for err, want := range map[error]int{
		filefield.ErrReferenced: http.StatusConflict,
		upload.ErrType:          http.StatusUnprocessableEntity,
		upload.ErrTooLarge:      http.StatusRequestEntityTooLarge,
		storage.ErrUnsupported:  http.StatusInternalServerError,
	} {
		var env *contract.ErrorEnvelope
		if !errors.As(filefield.MapError(ctx, err), &env) || env.GetStatus() != want {
			t.Errorf("MapError(%v) = %v, want %d", err, env, want)
		}
	}
}

func TestForEach(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	seen := map[int]bool{}
	if err := filefield.ForEach(ctx, 50, func(_ context.Context, i int) error {
		mu.Lock()
		seen[i] = true
		mu.Unlock()
		return nil
	}); err != nil || len(seen) != 50 {
		t.Fatalf("ForEach = %v, ran %d of 50", err, len(seen))
	}
	boom := errors.New("boom")
	var running, peak int32
	err := filefield.ForEach(ctx, 100, func(ctx context.Context, i int) error {
		n := atomic.AddInt32(&running, 1)
		defer atomic.AddInt32(&running, -1)
		for {
			p := atomic.LoadInt32(&peak)
			if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		if i == 3 {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) || peak > filefield.ResolveConcurrency {
		t.Fatalf("ForEach = %v with %d at once, want boom with at most %d", err, peak, filefield.ResolveConcurrency)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := filefield.ForEach(canceled, 3, func(context.Context, int) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("ForEach on a canceled context = %v", err)
	}
	if err := filefield.ForEach(ctx, 0, nil); err != nil {
		t.Fatalf("ForEach over nothing = %v", err)
	}
}

// noPublicURLs is a store whose public URLs are unsupported (S3 without
// GOMBIT_STORAGE_S3_PUBLIC_URL) but which signs URLs.
type noPublicURLs struct{ *memory.Store }

func (s noPublicURLs) URL(ctx context.Context, key string, opts storage.URLOptions) (string, error) {
	if !opts.Signed {
		return "", storage.ErrUnsupported
	}
	return s.Store.URL(ctx, key, opts)
}

func TestResolveFallsBackToASignedURL(t *testing.T) {
	_, store, _ := setup(t)
	putFile(t, store, "public/avatar", png, "image/png")
	d, err := filefield.Resolve(context.Background(), noPublicURLs{store}, "public/avatar")
	if err != nil || !strings.Contains(d.URL, "signature=") {
		t.Fatalf("a public file without public URLs = %+v, %v; want a signed URL", d, err)
	}
}
