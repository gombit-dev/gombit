package framework

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/local"
	"github.com/gombit-dev/gombit/storage/memory"
	"github.com/gombit-dev/gombit/storage/s3"
)

// openStorage opens the object store cfg.Storage names. The local driver
// creates nothing until the first write, so an app that never stores a file
// never grows a storage directory, and is never warned about one.
func openStorage(cfg config.Config, logger *zap.Logger) (storage.Storage, error) {
	if err := config.ValidateStorage(cfg.Storage); err != nil {
		return nil, err
	}
	switch cfg.Storage.Driver {
	case config.StorageDriverMemory:
		return memory.New(), nil
	case config.StorageDriverLocal:
		var opts []local.Option
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
			return nil, fmt.Errorf("framework: %w", err)
		}
		return store, nil
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
		})
		if err != nil {
			return nil, fmt.Errorf("framework: %w", err)
		}
		return store, nil
	default:
		return nil, fmt.Errorf("framework: unsupported storage driver %q", cfg.Storage.Driver)
	}
}
