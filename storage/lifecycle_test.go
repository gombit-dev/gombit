package storage_test

import (
	"context"
	"errors"
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
	if info.Filename() != "f.txt" || (storage.ObjectInfo{}).Filename() != "" {
		t.Fatalf("Filename = %q", info.Filename())
	}
}

// TestDeleteIfFails: the insert-failure case.
func TestDeleteIfFails(t *testing.T) {
	s := store(t, time.Now, "uploads/a", "uploads/b")
	insertFailed := errors.New("insert failed")
	if err := storage.DeleteIfFails(context.Background(), s, "uploads/a", func() error { return insertFailed }); !errors.Is(err, insertFailed) {
		t.Fatalf("DeleteIfFails = %v, want the insert's error", err)
	}
	if ok, _ := storage.Exists(context.Background(), s, "uploads/a"); ok {
		t.Fatal("the file of a failed insert was kept")
	}
	if err := storage.DeleteIfFails(context.Background(), s, "uploads/b", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if ok, _ := storage.Exists(context.Background(), s, "uploads/b"); !ok {
		t.Fatal("the file of a successful insert was deleted")
	}
	// A request whose client went away still cleans up.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := storage.DeleteIfFails(ctx, s, "uploads/b", func() error { return ctx.Err() }); !errors.Is(err, context.Canceled) {
		t.Fatalf("DeleteIfFails = %v", err)
	}
	if ok, _ := storage.Exists(context.Background(), s, "uploads/b"); ok {
		t.Fatal("a canceled request's file was kept")
	}
}

// failingDelete fails every Delete.
type failingDelete struct{ *memory.Store }

func (failingDelete) Delete(context.Context, string) error { return storage.ErrUnavailable }

func TestDeleteIfFailsReportsAFailedCleanup(t *testing.T) {
	s := failingDelete{store(t, time.Now, "k")}
	insertFailed := errors.New("insert failed")
	err := storage.DeleteIfFails(context.Background(), s, "k", func() error { return insertFailed })
	if !errors.Is(err, insertFailed) || !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("DeleteIfFails = %v, want the insert's error and the failed delete", err)
	}
}

// TestDeleteOwned: the model-delete case, and the ownership contract.
func TestDeleteOwned(t *testing.T) {
	ctx := context.Background()
	s := store(t, time.Now, "avatars/1", "avatars-shared/logo", "library/stock.png")
	for key, want := range map[string]bool{"avatars/1": true, "avatars-shared/logo": false, "library/stock.png": false} {
		deleted, err := storage.DeleteOwned(ctx, s, key, "avatars/")
		if err != nil || deleted != want {
			t.Errorf("DeleteOwned(%q) = %v, %v; want %v", key, deleted, err, want)
		}
		if ok, _ := storage.Exists(ctx, s, key); ok == want {
			t.Errorf("%q exists = %v after DeleteOwned", key, ok)
		}
	}
	for _, bad := range []string{"", "avatars", "../x/"} {
		if _, err := storage.DeleteOwned(ctx, s, "avatars-shared/logo", bad); !errors.Is(err, storage.ErrInvalidOptions) {
			t.Errorf("DeleteOwned with owned prefix %q = %v, want ErrInvalidOptions", bad, err)
		}
	}
	if ok, _ := storage.Exists(ctx, s, "avatars-shared/logo"); !ok {
		t.Fatal("a shared file was deleted")
	}
}

// TestSweep: the abandoned-upload case.
func TestSweep(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Add(-2 * time.Hour)
	clock := func() time.Time { return now }
	s := store(t, clock, "uploads/recorded", "uploads/abandoned", "uploads/recent", "other/abandoned")
	// uploads/recent is stored later: still within the grace period.
	now = time.Now().Add(-time.Minute)
	_, _ = s.Put(ctx, "uploads/recent", strings.NewReader("x"), storage.PutOptions{})
	referenced := func(_ context.Context, key string) (bool, error) { return key == "uploads/recorded", nil }
	res, err := storage.Sweep(ctx, s, "uploads/", time.Hour, referenced)
	if err != nil {
		t.Fatal(err)
	}
	if res.Checked != 2 || res.Deleted != 1 {
		t.Fatalf("Sweep = %+v, want 2 checked, 1 deleted", res)
	}
	for key, want := range map[string]bool{"uploads/recorded": true, "uploads/abandoned": false, "uploads/recent": true, "other/abandoned": true} {
		if ok, _ := storage.Exists(ctx, s, key); ok != want {
			t.Errorf("after Sweep, %s exists = %v, want %v", key, ok, want)
		}
	}
	lookup := errors.New("the database is down")
	if _, err := storage.Sweep(ctx, s, "other/", time.Hour, func(context.Context, string) (bool, error) { return false, lookup }); !errors.Is(err, lookup) {
		t.Fatalf("Sweep with a failing lookup = %v, want it", err)
	}
	if ok, _ := storage.Exists(ctx, s, "other/abandoned"); !ok {
		t.Fatal("a failed lookup deleted the object")
	}
	for _, tc := range []struct {
		prefix string
		age    time.Duration
	}{{"", time.Hour}, {"uploads", time.Hour}, {"uploads/", 0}} {
		if _, err := storage.Sweep(ctx, s, tc.prefix, tc.age, referenced); !errors.Is(err, storage.ErrInvalidOptions) {
			t.Errorf("Sweep(%q, %s) = %v, want ErrInvalidOptions", tc.prefix, tc.age, err)
		}
	}
	if _, err := storage.Sweep(ctx, plain{}, "uploads/", time.Hour, referenced); !errors.Is(err, storage.ErrUnsupported) {
		t.Fatalf("Sweep on a store that cannot list = %v", err)
	}
}
