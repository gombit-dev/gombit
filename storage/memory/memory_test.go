package memory_test

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/memory"
	"github.com/gombit-dev/gombit/storage/storagetest"
)

func TestConformance(t *testing.T) {
	storagetest.Run(t, func(*testing.T) storage.Storage { return memory.New() })
}

// TestMetadataIsCopied: neither the map given to Put nor the one an info
// returns is shared with what is stored.
func TestMetadataIsCopied(t *testing.T) {
	s := memory.New()
	ctx := context.Background()
	md := map[string]string{"owner": "42"}
	if _, err := s.Put(ctx, "k", strings.NewReader("x"), storage.PutOptions{Metadata: md}); err != nil {
		t.Fatal(err)
	}
	md["owner"] = "changed after Put"
	info, err := s.Stat(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	info.Metadata["owner"] = "changed by a reader"
	again, _ := s.Stat(ctx, "k")
	if again.Metadata["owner"] != "42" {
		t.Fatalf("stored metadata = %v, want it unchanged", again.Metadata)
	}
}

// TestReadersKeepTheirVersion: a reader opened before an overwrite reads
// the version it opened.
func TestReadersKeepTheirVersion(t *testing.T) {
	s := memory.New()
	ctx := context.Background()
	_, _ = s.Put(ctx, "k", strings.NewReader("old"), storage.PutOptions{})
	body, _, err := s.Open(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.Put(ctx, "k", strings.NewReader("new!"), storage.PutOptions{})
	got, _ := io.ReadAll(body)
	if string(got) != "old" {
		t.Fatalf("the open reader read %q, want the version it opened", got)
	}
}

func TestKeysAndClock(t *testing.T) {
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	s := memory.New(memory.WithClock(func() time.Time { return at }))
	ctx := context.Background()
	for _, k := range []string{"b", "a/x"} {
		info, err := s.Put(ctx, k, strings.NewReader(k), storage.PutOptions{})
		if err != nil || !info.ModTime.Equal(at) {
			t.Fatalf("Put(%q) = %+v, %v", k, info, err)
		}
	}
	keys := s.Keys()
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"a/x", "b"}) {
		t.Fatalf("Keys = %v", keys)
	}
}
