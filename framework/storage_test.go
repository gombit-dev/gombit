package framework

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"io"
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

// TestStorageDirectUploadsThroughTheApp: the app's storage route stores a
// direct upload made with the grant's request, and the route is exempt
// from cookie-mode CSRF (the signed URL is the authorization).
func TestStorageDirectUploadsThroughTheApp(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Driver = config.StorageDriverMemory
	cfg.Storage.URLSecret = strings.Repeat("u", 32)
	app := newTestApp(t, WithConfig(cfg))
	ctx := context.Background()
	req, err := storage.UploadURL(ctx, app.Storage(), "uploads/a.txt", storage.UploadURLOptions{Expires: time.Minute, Size: 5, ContentType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(req.Method, req.URL, strings.NewReader("hello"))
	for k, v := range req.Header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", w.Code, w.Body)
	}
	if info, err := app.Storage().Stat(ctx, "uploads/a.txt"); err != nil || info.Size != 5 {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
	if !app.storageRoute.owns(http.MethodPut, "/_storage/uploads/a.txt") || app.storageRoute.owns(http.MethodPut, "/_storagex/a") || app.storageRoute.owns(http.MethodPost, "/_storage/uploads/a.txt") {
		t.Fatal("the middleware leaves alone something other than the mounted route's signed uploads")
	}
}

// TestStorageRouteIsCSRFExemptInCookieMode: with cookie auth, a direct
// upload to the storage route passes without a CSRF token, and any other
// unsafe request still needs one.
func TestStorageRouteIsCSRFExemptInCookieMode(t *testing.T) {
	cfg := config.Default()
	cfg.Environment = config.EnvironmentTest
	cfg.Auth.JWTSecret = strings.Repeat("j", 32)
	cfg.Auth.Mode = config.AuthModeCookie
	router, route, err := newRouter(cfg, nil, nil, func(c *gin.Context) { c.Status(http.StatusOK) }, nil)
	if err != nil {
		t.Fatal(err)
	}
	ok := func(c *gin.Context) { c.Status(http.StatusOK) }
	router.PUT("/_storage/*key", ok)
	router.PUT("/api/things/:id", ok)
	// Before the storage route is mounted, a route under its path is an
	// ordinary route: CSRF applies.
	put := func(path string) int {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPut, path, strings.NewReader("x")))
		return w.Code
	}
	if got := put("/_storage/uploads/a"); got != http.StatusForbidden {
		t.Fatalf("PUT under an unmounted storage path = %d, want 403: CSRF applies", got)
	}
	route.mounted("/_storage")
	for path, want := range map[string]int{"/_storage/uploads/a": http.StatusOK, "/api/things/1": http.StatusForbidden} {
		if got := put(path); got != want {
			t.Errorf("PUT %s = %d, want %d", path, got, want)
		}
	}
	if route.owns(http.MethodPut, "/_storagex/a") || route.owns(http.MethodPut, "/_storage") || route.owns(http.MethodPost, "/_storage/uploads/a") {
		t.Fatal("the mounted route claims a request outside its signed uploads")
	}
}

// TestWithStorageKeepsProtectionsUnderTheStoragePath: an app that brings
// its own store (WithStorage) gets no framework storage route, so a route
// it registers under GOMBIT_STORAGE_LOCAL_URL is an ordinary route: the
// JSON body limit (and CSRF, and sanitization) still apply to it.
func TestWithStorageKeepsProtectionsUnderTheStoragePath(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Driver = config.StorageDriverMemory
	cfg.Storage.URLSecret = strings.Repeat("u", 32)
	app := newTestApp(t, WithConfig(cfg), WithStorage(memory.New()))
	app.Router().POST("/_storage/hook", func(c *gin.Context) {
		var v map[string]any
		if err := c.ShouldBindJSON(&v); err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		c.Status(http.StatusOK)
	})
	body := `{"pad":"` + strings.Repeat("x", int(maxRequestBodyBytes)) + `"}`
	r := httptest.NewRequest(http.MethodPost, "/_storage/hook", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized JSON POST to an app route under the storage path = %d, want 413: the body limit applies", w.Code)
	}
	if app.storageRoute.owns(http.MethodPost, "/_storage/hook") {
		t.Fatal("WithStorage mounted no storage route, yet the middleware would leave its path alone")
	}
}

// TestStorageRouteSkipsBodyRewritingMiddleware: a direct upload of a JSON
// file larger than the JSON body limit, with input sanitization on, is
// stored whole and unchanged.
func TestStorageRouteSkipsBodyRewritingMiddleware(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Driver = config.StorageDriverMemory
	cfg.Storage.URLSecret = strings.Repeat("u", 32)
	cfg.Security.SanitizeInput = true
	app := newTestApp(t, WithConfig(cfg))
	ctx := context.Background()
	doc := `{"note":"<b>kept</b>","pad":"` + strings.Repeat("x", int(maxRequestBodyBytes)) + `"}`
	req, err := storage.UploadURL(ctx, app.Storage(), "uploads/doc.json", storage.UploadURLOptions{Expires: time.Minute, Size: int64(len(doc)), ContentType: "application/json"})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(req.Method, req.URL, strings.NewReader(doc))
	for k, v := range req.Header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d %.200s", w.Code, w.Body)
	}
	body, _, err := app.Storage().Open(ctx, "uploads/doc.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	got, _ := io.ReadAll(body)
	if string(got) != doc {
		t.Fatalf("stored %d bytes, want the %d sent unchanged", len(got), len(doc))
	}
	// Elsewhere, the JSON limit still applies.
	w = httptest.NewRecorder()
	big := httptest.NewRequest(http.MethodPut, "/api/anything", strings.NewReader(doc))
	big.Header.Set("Content-Type", "application/json")
	app.Router().ServeHTTP(w, big)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a large JSON PUT elsewhere = %d, want 413", w.Code)
	}
}

// TestAppRoutesUnderTheMountedStoragePathKeepProtections: with the
// framework's storage route mounted, an application route under the same
// path with another method (a POST hook) is application code, not a signed
// upload: the JSON body limit still applies to it, while the route's own
// signed PUTs stay exempt.
func TestAppRoutesUnderTheMountedStoragePathKeepProtections(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Driver = config.StorageDriverMemory
	cfg.Storage.URLSecret = strings.Repeat("u", 32)
	app := newTestApp(t, WithConfig(cfg))
	if !app.storageRoute.owns(http.MethodPut, "/_storage/x") {
		t.Fatal("the storage route was not mounted")
	}
	app.Router().POST("/_storage/hook", func(c *gin.Context) {
		var v map[string]any
		if err := c.ShouldBindJSON(&v); err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		c.Status(http.StatusOK)
	})
	body := `{"pad":"` + strings.Repeat("x", int(maxRequestBodyBytes)) + `"}`
	r := httptest.NewRequest(http.MethodPost, "/_storage/hook", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized JSON POST to an app route under the mounted storage path = %d, want 413", w.Code)
	}
}

// absoluteURLs is a store whose URLs are on another origin (an S3 bucket).
type absoluteURLs struct{ *memory.Store }

func (absoluteURLs) URL(_ context.Context, key string, opts storage.URLOptions) (string, error) {
	if !opts.Signed {
		return "https://cdn.example.com/" + key, nil
	}
	return "https://bucket.s3.example.com/" + key + "?X-Amz-Signature=x", nil
}

func (absoluteURLs) UploadURL(_ context.Context, key string, _ storage.UploadURLOptions) (storage.UploadRequest, error) {
	return storage.UploadRequest{Method: http.MethodPut, URL: "https://bucket.s3.example.com/" + key + "?X-Amz-Signature=y"}, nil
}

// TestSPAPagesAllowTheStoreOrigins: the admin and embedded SPA pages may
// show images from, and upload to, the store's own origin; a store whose
// URLs are app paths adds nothing.
func TestSPAPagesAllowTheStoreOrigins(t *testing.T) {
	origins := storageOrigins(absoluteURLs{memory.New()}, "public/")
	if len(origins) != 2 || origins[0] != "https://cdn.example.com" || origins[1] != "https://bucket.s3.example.com" {
		t.Fatalf("storageOrigins = %v", origins)
	}
	csp := spaCSP(origins)[0]
	for _, want := range []string{
		"img-src 'self' data: https://cdn.example.com https://bucket.s3.example.com",
		"connect-src 'self' https://cdn.example.com https://bucket.s3.example.com",
		"script-src 'self';",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	cfg := config.Default()
	cfg.Storage.Driver = config.StorageDriverMemory
	cfg.Storage.URLSecret = strings.Repeat("u", 32)
	app := newTestApp(t, WithConfig(cfg))
	if got := storageOrigins(app.Storage(), "public/"); len(got) != 0 {
		t.Fatalf("app-path URLs gave origins %v", got)
	}
	if got := spaCSP(nil); got[0] != spaContentSecurityPolicy {
		t.Fatalf("no origins changed the CSP: %v", got)
	}
	if got := storageOrigins(memory.New(), "public/"); got != nil {
		t.Fatalf("a store without URLs gave %v", got)
	}
}
