package storage_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/memory"
)

func store(t *testing.T, now func() time.Time, keys ...string) *memory.Store {
	t.Helper()
	s := memory.New(memory.WithClock(now))
	for _, k := range keys {
		if _, err := s.Put(context.Background(), k, strings.NewReader("x"), storage.PutOptions{Metadata: map[string]string{storage.FilenameMetadata: "f.txt"}}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestFilename(t *testing.T) {
	s := store(t, time.Now, "k")
	info, _ := s.Stat(context.Background(), "k")
	if info.StoredFilename() != "f.txt" || (storage.ObjectInfo{}).StoredFilename() != "" {
		t.Fatalf("Filename = %q", info.StoredFilename())
	}
}
