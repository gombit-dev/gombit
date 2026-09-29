package framework

import (
	"fmt"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/local"
	"github.com/gombit-dev/gombit/storage/memory"
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
	default:
		return nil, fmt.Errorf("framework: unsupported storage driver %q", cfg.Storage.Driver)
	}
}
