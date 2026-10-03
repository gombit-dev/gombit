package framework

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/local"
	"github.com/gombit-dev/gombit/storage/memory"
	"github.com/gombit-dev/gombit/storage/presign"
	"github.com/gombit-dev/gombit/storage/s3"
)

// TestStorageDefaultsToLocal: a new app stores files locally with no
// configuration, under the configured root, created on the first write.
func TestStorageDefaultsToLocal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "files")
	cfg := config.Default()
	cfg.Storage.Local.Root = root
	app := newTestApp(t, WithConfig(cfg))
	store, ok := app.Storage().(*local.Store)
	if !ok {
		t.Fatalf("Storage() = %T, want *local.Store by default", app.Storage())
	}
	if store.Root() != root {
		t.Fatalf("root = %q, want %q", store.Root(), root)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("opening the app created the storage root (%v)", err)
	}
	ctx := context.Background()
	if _, err := app.Storage().Put(ctx, "avatars/1.png", strings.NewReader("png"), storage.PutOptions{ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	if info, err := app.Storage().Stat(ctx, "avatars/1.png"); err != nil || info.ContentType != "image/png" {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
}

func TestStorageMemoryDriver(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Driver = config.StorageDriverMemory
	app := newTestApp(t, WithConfig(cfg))
	if _, ok := app.Storage().(*memory.Store); !ok {
		t.Fatalf("Storage() = %T, want *memory.Store", app.Storage())
	}
}

func TestWithStorage(t *testing.T) {
	mem := memory.New()
	app := newTestApp(t, WithStorage(mem))
	if app.Storage() != storage.Storage(mem) {
		t.Fatal("WithStorage was not used")
	}
	if _, err := New(WithStorage(nil)); err == nil {
		t.Fatal("WithStorage(nil) was accepted")
	}
}

func TestStorageS3Driver(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Driver = config.StorageDriverS3
	cfg.Storage.S3.Bucket = "uploads"
	cfg.Storage.S3.Endpoint = "http://127.0.0.1:9"
	cfg.Storage.S3.AccessKeyID = "id"
	cfg.Storage.S3.SecretAccessKey = "secret"
	app := newTestApp(t, WithConfig(cfg))
	if _, ok := app.Storage().(*s3.Store); !ok {
		t.Fatalf("Storage() = %T, want *s3.Store", app.Storage())
	}
	cfg.Storage.S3.Bucket = ""
	if _, err := New(WithConfig(cfg)); err == nil || !strings.Contains(err.Error(), "Storage.S3.Bucket") {
		t.Fatalf("New without a bucket = %v, want a Storage.S3.Bucket error", err)
	}
}

func TestStorageBadConfigFailsNew(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Driver = "s4"
	if _, err := New(WithConfig(cfg)); err == nil || !strings.Contains(err.Error(), "Storage.Driver") {
		t.Fatalf("New with an unknown storage driver = %v, want a Storage.Driver error", err)
	}
}

// TestStorageWarnsAboutAnEphemeralRootInProduction: in production, the
// first write to a local root relative to the working directory (which a
// container loses) logs a warning, once; an absolute root, or development,
// does not.
func TestStorageWarnsAboutAnEphemeralRootInProduction(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		name string
		env  config.Environment
		root string
		want int
	}{
		{"production relative", config.EnvironmentProduction, "storage", 1},
		{"production absolute", config.EnvironmentProduction, filepath.Join(t.TempDir(), "files"), 0},
		{"development relative", config.EnvironmentDevelopment, "dev-storage", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			cfg := config.Default()
			cfg.Environment = tc.env
			cfg.Storage.Local.Root = tc.root
			store, _, err := openStorage(cfg, zap.New(core))
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			for i := 0; i < 2; i++ {
				if _, err := store.Put(ctx, "k", strings.NewReader("x"), storage.PutOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if n := logs.FilterMessageSnippet("GOMBIT_STORAGE_LOCAL_ROOT").Len(); n != tc.want {
				t.Fatalf("%d warnings, want %d", n, tc.want)
			}
		})
	}
}

// TestStorageURLsAreServedByTheApp: with a URL secret, the local driver's
// signed and public URLs work through the app's own router, and a private
// object is not readable without a signature.
func TestStorageURLsAreServedByTheApp(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Local.Root = t.TempDir()
	cfg.Storage.URLSecret = strings.Repeat("u", config.MinStorageURLSecretLength)
	app := newTestApp(t, WithConfig(cfg))
	ctx := context.Background()
	for _, key := range []string{"private/report.txt", "public/logo.txt"} {
		if _, err := app.Storage().Put(ctx, key, strings.NewReader("bytes of "+key), storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	get := func(u string) (int, string) {
		w := httptest.NewRecorder()
		app.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, u, nil))
		return w.Code, w.Body.String()
	}
	signed, err := app.Storage().URL(ctx, "private/report.txt", storage.SignedURL(time.Minute))
	if err != nil || !strings.HasPrefix(signed, config.DefaultStorageLocalURL+"/") {
		t.Fatalf("URL = %q, %v", signed, err)
	}
	if code, body := get(signed); code != http.StatusOK || body != "bytes of private/report.txt" {
		t.Fatalf("GET signed = %d %q", code, body)
	}
	if code, body := get("/_storage/private/report.txt"); code != http.StatusForbidden || !strings.Contains(body, `"authorization"`) {
		t.Fatalf("GET private without a signature = %d %s", code, body)
	}
	public, err := app.Storage().URL(ctx, "public/logo.txt", storage.PublicURL())
	if err != nil {
		t.Fatal(err)
	}
	if code, body := get(public); code != http.StatusOK || body != "bytes of public/logo.txt" {
		t.Fatalf("GET public = %d %q", code, body)
	}
}

func TestStorageURLsNeedASecret(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Driver = config.StorageDriverMemory
	app := newTestApp(t, WithConfig(cfg))
	if _, err := app.Storage().URL(context.Background(), "k", storage.SignedURL(time.Minute)); !errors.Is(err, storage.ErrUnsupported) {
		t.Fatalf("URL with no secret = %v, want ErrUnsupported", err)
	}
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/_storage/k", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("/_storage is mounted with no URLs: %d", w.Code)
	}
}

// TestStorageURLKeyIsDerivedFromTheJWTSecret: without its own secret, the
// signing key comes from the JWT secret, separated from it: the same JWT
// secret gives the same URLs, and the key is not the JWT secret itself.
func TestStorageURLKeyIsDerivedFromTheJWTSecret(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.JWTSecret = strings.Repeat("j", 32)
	a, err := storageSigner(cfg)
	if err != nil || a == nil {
		t.Fatalf("storageSigner = %v, %v", a, err)
	}
	b, _ := storageSigner(cfg)
	ua, _ := a.URL("private/k", storage.SignedURL(time.Hour))
	ub, _ := b.URL("private/k", storage.SignedURL(time.Hour))
	if ua == "" || ua != ub {
		t.Fatalf("the derived key is not stable: %q vs %q", ua, ub)
	}
	cfg.Storage.URLSecret = cfg.Auth.JWTSecret // the underived key
	raw, _ := storageSigner(cfg)
	if ur, _ := raw.URL("private/k", storage.SignedURL(time.Hour)); ur == ua {
		t.Fatal("the URL key is the JWT secret itself")
	}
	// Pinned: HMAC-SHA256(JWT secret, label), so a change to the derivation
	// (which would break every link already handed out) is deliberate.
	mac := hmac.New(sha256.New, []byte(cfg.Auth.JWTSecret))
	_, _ = mac.Write([]byte("gombit storage url signing key v1"))
	pinned, err := presign.New(presign.Config{Base: config.DefaultStorageLocalURL, Secret: mac.Sum(nil), PublicPrefix: cfg.Storage.PublicPrefix, Scope: cfg.AppName + "\x00" + string(cfg.Environment)})
	if err != nil {
		t.Fatal(err)
	}
	if up, _ := pinned.URL("private/k", storage.SignedURL(time.Hour)); up != ua {
		t.Fatalf("the derived key changed: %q, want %q", ua, up)
	}
	cfg.Storage.Local.URL = ""
	if s, err := storageSigner(cfg); s != nil || err != nil {
		t.Fatalf("no local URL = %v, %v; want no signer", s, err)
	}
}

// TestStorageURLsAreScopedToTheApp: apps sharing a secret (staging and
// production with one JWT secret) do not accept each other's URLs.
func TestStorageURLsAreScopedToTheApp(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.JWTSecret = strings.Repeat("j", 32)
	staging, _ := storageSigner(cfg)
	cfg.Environment = config.EnvironmentProduction
	production, _ := storageSigner(cfg)
	u, _ := staging.URL("private/k", storage.SignedURL(time.Hour))
	parsed, _ := url.Parse(u)
	if err := production.Verify("private/k", parsed.Query()); !errors.Is(err, presign.ErrSignature) {
		t.Fatalf("production accepted a staging URL: %v", err)
	}
}

func TestStorageURLPathMayNotShadowTheFramework(t *testing.T) {
	for _, path := range []string{"/api", "/api/v1/files", "/admin", "/admin/files", "/livez", "/docs", "/openapi.json", "/openapi"} {
		cfg := config.Default()
		cfg.Storage.Driver = config.StorageDriverMemory
		cfg.Storage.URLSecret = strings.Repeat("u", 32)
		cfg.Storage.Local.URL = path
		if _, err := New(WithConfig(cfg)); err == nil || !strings.Contains(err.Error(), "GOMBIT_STORAGE_LOCAL_URL") {
			t.Errorf("GOMBIT_STORAGE_LOCAL_URL=%s = %v, want an error naming the setting", path, err)
		}
	}
	// A route the app already has is an error from New, not a panic.
	cfg := config.Default()
	cfg.Storage.Driver = config.StorageDriverMemory
	cfg.Storage.URLSecret = strings.Repeat("u", 32)
	cfg.Storage.Local.URL = "/files"
	r := gin.New()
	r.GET("/files/:id", func(*gin.Context) {})
	if _, err := New(WithConfig(cfg), WithRouter(r)); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("a conflicting route = %v", err)
	}
}

// TestStorageURLsServeRangesAndHEAD, at an absolute GOMBIT_STORAGE_LOCAL_URL
// (mounted at its path, URLs carrying the host).
func TestStorageURLsServeRangesAndHEAD(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Driver = config.StorageDriverMemory
	cfg.Storage.URLSecret = strings.Repeat("u", 32)
	cfg.Storage.Local.URL = "https://files.example.com/blobs"
	app := newTestApp(t, WithConfig(cfg))
	ctx := context.Background()
	if _, err := app.Storage().Put(ctx, "private/video.bin", strings.NewReader("0123456789"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	u, err := app.Storage().URL(ctx, "private/video.bin", storage.SignedURL(time.Minute))
	if err != nil || !strings.HasPrefix(u, "https://files.example.com/blobs/private/video.bin?") {
		t.Fatalf("URL = %q, %v", u, err)
	}
	path := strings.TrimPrefix(u, "https://files.example.com")
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Range", "bytes=2-5")
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent || w.Body.String() != "2345" {
		t.Fatalf("Range GET = %d %q", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	app.Router().ServeHTTP(w, httptest.NewRequest(http.MethodHead, path, nil))
	if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "10" {
		t.Fatalf("HEAD = %d, %d bytes, length %q", w.Code, w.Body.Len(), w.Header().Get("Content-Length"))
	}
	etag := w.Header().Get("ETag")
	req = httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("If-None-Match", etag)
	w = httptest.NewRecorder()
	app.Router().ServeHTTP(w, req)
	if etag == "" || w.Code != http.StatusNotModified {
		t.Fatalf("conditional GET (ETag %q) = %d", etag, w.Code)
	}
}
