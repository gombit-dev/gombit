package framework

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/local"
	"github.com/gombit-dev/gombit/storage/memory"
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
			store, err := openStorage(cfg, zap.New(core))
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
