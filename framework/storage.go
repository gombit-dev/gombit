package framework

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/gombit-dev/gombit/config"
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
	signer, err := presign.New(presign.Config{Base: st.Local.URL, Secret: secret, PublicPrefix: st.PublicPrefix})
	if err != nil {
		return nil, fmt.Errorf("framework: storage URLs: %w", err)
	}
	return signer, nil
}

// mountStorageURLs serves the objects of store at signer's URLs on router
// (GET and HEAD under signer.Path()).
func mountStorageURLs(router *gin.Engine, store storage.Storage, signer *presign.Signer) {
	h := gin.WrapH(presign.Handler(store, signer))
	router.GET(signer.Path()+"/*key", h)
	router.HEAD(signer.Path()+"/*key", h)
}
