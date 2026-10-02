package framework

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/local"
	"github.com/gombit-dev/gombit/storage/memory"
	"github.com/gombit-dev/gombit/storage/presign"
	"github.com/gombit-dev/gombit/storage/s3"
)

// openStorage opens the object store cfg.Storage names. The local driver
// creates nothing until the first write, so an app that never stores a file
// never grows a storage directory, and is never warned about one.
//
// For the local and memory drivers it also returns the presign.Signer
// behind their URLs (nil when they have none: no GOMBIT_STORAGE_LOCAL_URL,
// or no secret to sign with), which New mounts presign.Handler for.
func openStorage(cfg config.Config, logger *zap.Logger) (storage.Storage, *presign.Signer, error) {
	if err := config.ValidateStorage(cfg.Storage); err != nil {
		return nil, nil, err
	}
	var signer *presign.Signer
	if cfg.Storage.Driver == config.StorageDriverLocal || cfg.Storage.Driver == config.StorageDriverMemory {
		var err error
		if signer, err = storageSigner(cfg); err != nil {
			return nil, nil, err
		}
	}
	switch cfg.Storage.Driver {
	case config.StorageDriverMemory:
		if signer != nil {
			return memory.New(memory.WithURLs(signer)), signer, nil
		}
		return memory.New(), nil, nil
	case config.StorageDriverLocal:
		opts := []local.Option{local.WithWarn(func(msg string, err error) { logger.Warn(msg, zap.Error(err)) })}
		if signer != nil {
			opts = append(opts, local.WithURLs(signer))
		}
		if cfg.Environment == config.EnvironmentProduction && !filepath.IsAbs(cfg.Storage.Local.Root) {
			opts = append(opts, local.WithFirstPut(func(root string) {
				logger.Warn("storage: production files are going to a directory relative to the working directory, "+
					"which a container loses when it is replaced; set GOMBIT_STORAGE_LOCAL_ROOT to a persistent volume "+
					"or use an object store",
					zap.String("root", root))
			}))
		}
		store, err := local.New(cfg.Storage.Local.Root, opts...)
		if err != nil {
			return nil, nil, fmt.Errorf("framework: %w", err)
		}
		return store, signer, nil
	case config.StorageDriverS3:
		c := cfg.Storage.S3
		if cfg.Environment == config.EnvironmentProduction && strings.HasPrefix(strings.ToLower(c.Endpoint), "http://") {
			logger.Warn("storage: the S3 endpoint is plain http, so objects cross the network unencrypted; use https in production",
				zap.String("endpoint", c.Endpoint))
		}
		store, err := s3.New(context.Background(), s3.Config{
			Endpoint:        c.Endpoint,
			Region:          c.Region,
			Bucket:          c.Bucket,
			Prefix:          c.Prefix,
			AccessKeyID:     c.AccessKeyID,
			SecretAccessKey: c.SecretAccessKey,
			ForcePathStyle:  c.ForcePathStyle,
			PublicPrefix:    cfg.Storage.PublicPrefix,
			PublicURL:       c.PublicURL,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("framework: %w", err)
		}
		return store, nil, nil
	default:
		return nil, nil, fmt.Errorf("framework: unsupported storage driver %q", cfg.Storage.Driver)
	}
}

// storageURLSecretLabel separates the URL-signing key derived from the JWT
// secret from every other use of it.
const storageURLSecretLabel = "gombit storage url signing key v1" // #nosec G101 -- a public derivation label, not a credential.

// storageSigner returns the Signer for the local and memory drivers' URLs:
// served at GOMBIT_STORAGE_LOCAL_URL and signed with
// GOMBIT_STORAGE_URL_SECRET, or a key derived from the JWT secret (HMAC
// with a fixed label, so a URL signature reveals nothing usable as a token
// signature). nil when either is missing.
func storageSigner(cfg config.Config) (*presign.Signer, error) {
	st := cfg.Storage
	if st.Local.URL == "" {
		return nil, nil
	}
	secret := []byte(st.URLSecret)
	if len(secret) == 0 {
		if cfg.Auth.JWTSecret == "" {
			return nil, nil
		}
		mac := hmac.New(sha256.New, []byte(cfg.Auth.JWTSecret))
		_, _ = mac.Write([]byte(storageURLSecretLabel))
		secret = mac.Sum(nil)
	}
	signer, err := presign.New(presign.Config{
		Base:         st.Local.URL,
		Secret:       secret,
		PublicPrefix: st.PublicPrefix,
		// A URL opens only this app in this environment, even where
		// several share a JWT secret.
		Scope: cfg.AppName + "\x00" + string(cfg.Environment),
	})
	if err != nil {
		return nil, fmt.Errorf("framework: storage URLs: %w", err)
	}
	if err := checkStorageURLPath(signer.Path(), cfg.API.Prefix); err != nil {
		return nil, err
	}
	return signer, nil
}

// frameworkPaths are the routes framework.New serves itself (and the
// OpenAPI documents, every path starting with contract.OpenAPIPath).
var frameworkPaths = []string{"/livez", "/readyz", "/metrics", "/admin", contract.DocsPath}

// checkStorageURLPath refuses a storage URL path that overlaps the API or
// a framework route, which would shadow them (or make the router panic).
func checkStorageURLPath(path, apiPrefix string) error {
	overlaps := func(a, b string) bool {
		return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
	}
	reserved := frameworkPaths
	if p := strings.TrimSuffix(apiPrefix, "/"); p != "" {
		reserved = append([]string{p}, reserved...)
	}
	for _, r := range reserved {
		if overlaps(path, r) {
			return fmt.Errorf("framework: GOMBIT_STORAGE_LOCAL_URL serves under %q, which overlaps %q; choose a path of its own, such as /_storage", path, r)
		}
	}
	if strings.HasPrefix(path, contract.OpenAPIPath) {
		return fmt.Errorf("framework: GOMBIT_STORAGE_LOCAL_URL serves under %q, which overlaps the OpenAPI documents (%s*)", path, contract.OpenAPIPath)
	}
	return nil
}

// mountStorageURLs serves the objects of store at signer's URLs on router
// (GET and HEAD under signer.Path()). A path the router cannot take (it
// conflicts with a route already there) is an error, not a panic.
func mountStorageURLs(router *gin.Engine, store storage.Storage, signer *presign.Signer, route *storageRoute) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("framework: GOMBIT_STORAGE_LOCAL_URL %q conflicts with a route: %v", signer.Path(), r)
		}
	}()
	h := gin.WrapH(presign.Handler(store, signer))
	router.GET(signer.Path()+"/*key", h)
	router.HEAD(signer.Path()+"/*key", h)
	router.PUT(signer.Path()+"/*key", h) // direct uploads
	route.mounted(signer.Path())
	return nil
}

// storageRoute is the storage route New mounted (presign.Handler, for
// the local and memory drivers' URLs), if it mounted one: the runtime
// middleware leaves requests under it alone, and only those. CSRF (a
// direct upload is authorized by its signed URL alone, with no cookie
// involved, as a presigned S3 URL is), the JSON body limit (the signed
// length bounds it), and input sanitization (the file is stored byte for
// byte). Route ownership is what the app mounted, not what the
// configuration names: with WithStorage New mounts nothing, and a route an
// app registers under GOMBIT_STORAGE_LOCAL_URL keeps every protection.
type storageRoute struct{ prefix atomic.Pointer[string] }

// mounted records the path presign.Handler was mounted under (nothing for
// an app that brought its own router and middleware: WithRouter).
func (r *storageRoute) mounted(path string) {
	if r == nil {
		return
	}
	p := path + "/"
	r.prefix.Store(&p)
}

// owns reports whether path is under the mounted storage route.
func (r *storageRoute) owns(path string) bool {
	if r == nil {
		return false
	}
	p := r.prefix.Load()
	return p != nil && strings.HasPrefix(path, *p)
}

// skipStorageRoute runs h except for requests to the mounted storage route.
func skipStorageRoute(route *storageRoute, h gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if route.owns(c.Request.URL.Path) {
			c.Next()
			return
		}
		h(c)
	}
}

// storageOrigins are the origins the store's URLs point to when they are
// not the app's own (an S3 bucket's host, a CDN): the SPA pages' CSP
// allows them for images and requests (spaCSP), so the admin can preview a
// stored image and a page can upload directly to the store. It asks the
// store for a public, a signed, and an upload URL of a probe key; a URL the
// store cannot make, or a path on the app's own origin, adds nothing.
func storageOrigins(store storage.Storage, publicPrefix string) []string {
	if store == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const probe = "gombit-csp-probe"
	var urls []string
	if publicPrefix != "" {
		if u, err := store.URL(ctx, publicPrefix+probe, storage.PublicURL()); err == nil {
			urls = append(urls, u)
		}
	}
	if u, err := store.URL(ctx, probe, storage.SignedURL(time.Minute)); err == nil {
		urls = append(urls, u)
	}
	if req, err := storage.UploadURL(ctx, store, probe, storage.UploadURLOptions{Expires: time.Minute}); err == nil {
		urls = append(urls, req.URL)
	}
	var origins []string
	seen := map[string]bool{}
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			continue
		}
		origin := u.Scheme + "://" + u.Host
		if !seen[origin] {
			seen[origin] = true
			origins = append(origins, origin)
		}
	}
	return origins
}

// spaStorageOrigins is storageOrigins for the app's store, computed once
// (both SPA mounts use it). An S3 store that yields none is logged: its
// pages' CSP then blocks image previews and direct uploads to the bucket.
func (a *App) spaStorageOrigins() []string {
	a.storageOriginsOnce.Do(func() {
		a.storageOrigins = storageOrigins(a.storage, a.cfg.Storage.PublicPrefix)
		if len(a.storageOrigins) == 0 && a.cfg.Storage.Driver == config.StorageDriverS3 && a.logger != nil {
			a.logger.Warn("storage: could not make an S3 URL at startup (credentials?), so the admin and SPA pages' Content-Security-Policy does not allow the bucket: image previews and direct uploads from the browser will be blocked")
		}
	})
	return a.storageOrigins
}
