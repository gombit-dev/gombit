package claims_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/claims"
	"github.com/gombit-dev/gombit/storage/memory"
)

// doc is a record that refers to a stored file, one record per file.
type doc struct {
	ID      uint   `gorm:"primaryKey"`
	FileKey string `gorm:"size:512;uniqueIndex"`
}

func (doc) TableName() string { return "claims_test_docs" }

func TestSQLite(t *testing.T) {
	db, err := database.Open(config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    "file:" + filepath.Join(t.TempDir(), "claims.db") + "?cache=shared&_fk=1&_busy_timeout=5000",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runSuite(t, db.DB)
}

// runSuite runs every protocol test against db, on any dialect.
func runSuite(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.AutoMigrate(append(claims.Models(), &doc{})...); err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]func(*testing.T, *gorm.DB){
		"PendingIsUnique":           testPendingIsUnique,
		"CreateHolds":               testCreateHolds,
		"FailedCreateDeletes":       testFailedCreateDeletes,
		"RetriedConfirmationKeeps":  testRetriedConfirmationKeeps,
		"CommittedHoldIsNotDeleted": testCommittedHoldIsNotDeleted,
		"Sweep":                     testSweep,
		"DeleteWith":                testDeleteWith,
		"Update":                    testUpdate,
		"ConfirmRacesSweep":         testConfirmRacesSweep,
		"SweepFirstWins":            testSweepFirstWins,
		"ConfirmationInFlightWins":  testConfirmationInFlightWins,
	} {
		t.Run(name, func(t *testing.T) {
			clean(t, db)
			test(t, db)
		})
	}
}

func clean(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&claims.Claim{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&doc{}).Error; err != nil {
		t.Fatal(err)
	}
}

// upload stores an object under a newly claimed key, as an upload with
// Policy.Claims does.
func upload(t *testing.T, c *claims.Claims, store storage.Storage, key string) {
	t.Helper()
	ctx := context.Background()
	if err := c.Pending(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, key, strings.NewReader("bytes of "+key), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
}

func state(t *testing.T, db *gorm.DB, key string) string {
	t.Helper()
	var c claims.Claim
	err := db.Session(&gorm.Session{Logger: logger.Discard}).Where("object_key = ?", key).Take(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "none"
	}
	if err != nil {
		t.Fatal(err)
	}
	return c.State
}

func exists(t *testing.T, store storage.Storage, key string) bool {
	t.Helper()
	ok, err := storage.Exists(context.Background(), store, key)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func insert(key string) func(tx *gorm.DB) error {
	return func(tx *gorm.DB) error { return tx.Create(&doc{FileKey: key}).Error }
}

func testPendingIsUnique(t *testing.T, db *gorm.DB) {
	c := claims.New(db, memory.New())
	ctx := context.Background()
	if err := c.Pending(ctx, "u/1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Pending(ctx, "u/1"); !errors.Is(err, claims.ErrClaimed) {
		t.Fatalf("a second Pending = %v, want ErrClaimed", err)
	}
	// Refused up front on every dialect (SQLite would store it; the others
	// would fail with an error of their own).
	long := "u/" + strings.Repeat("k", claims.MaxKeyLen)
	if err := c.Pending(ctx, long); !errors.Is(err, claims.ErrKeyTooLong) {
		t.Fatalf("Pending of a %d-byte key = %v, want ErrKeyTooLong", len(long), err)
	}
	if err := c.Pending(ctx, strings.Repeat("k", claims.MaxKeyLen)); err != nil {
		t.Fatalf("Pending of a %d-byte key = %v", claims.MaxKeyLen, err)
	}
}

func testCreateHolds(t *testing.T, db *gorm.DB) {
	store := memory.New()
	c := claims.New(db, store)
	upload(t, c, store, "u/1")
	if err := c.CreateWith(context.Background(), []string{"u/1"}, insert("u/1")); err != nil {
		t.Fatal(err)
	}
	if state(t, db, "u/1") != claims.Held || !exists(t, store, "u/1") {
		t.Fatalf("after CreateWith: claim %s, object %v; want held, kept", state(t, db, "u/1"), exists(t, store, "u/1"))
	}
	if res, err := c.Sweep(context.Background(), 0); err != nil || res.Abandoned != 0 || !exists(t, store, "u/1") {
		t.Fatalf("Sweep = %+v, %v: a held object must never be swept", res, err)
	}
}

func testFailedCreateDeletes(t *testing.T, db *gorm.DB) {
	store := memory.New()
	c := claims.New(db, store)
	upload(t, c, store, "u/1")
	boom := errors.New("validation failed")
	err := c.CreateWith(context.Background(), []string{"u/1"}, func(*gorm.DB) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("CreateWith = %v, want the record's error", err)
	}
	if exists(t, store, "u/1") || state(t, db, "u/1") != "none" {
		t.Fatalf("after a failed create: object %v, claim %s; want both gone", exists(t, store, "u/1"), state(t, db, "u/1"))
	}
}

// testRetriedConfirmationKeeps: the first confirmation commits its record
// but its answer is lost; the client retries; the retry fails (the key is
// held, the record exists) and must not delete the object the first
// record refers to.
func testRetriedConfirmationKeeps(t *testing.T, db *gorm.DB) {
	store := memory.New()
	c := claims.New(db, store)
	upload(t, c, store, "u/1")
	ctx := context.Background()
	if err := c.CreateWith(ctx, []string{"u/1"}, insert("u/1")); err != nil {
		t.Fatal(err)
	}
	err := c.CreateWith(ctx, []string{"u/1"}, insert("u/1"))
	if err == nil {
		t.Fatal("a retried create of a held key succeeded")
	}
	var n int64
	db.Model(&doc{}).Where("file_key = ?", "u/1").Count(&n)
	if !exists(t, store, "u/1") || state(t, db, "u/1") != claims.Held || n != 1 {
		t.Fatalf("after the retry: object %v, claim %s, %d records; want kept, held, 1", exists(t, store, "u/1"), state(t, db, "u/1"), n)
	}
}

// testCommittedHoldIsNotDeleted: a transaction that held the key committed,
// though the caller was told it failed; the cleanup that follows (Abandon)
// finds the key held and keeps the object.
func testCommittedHoldIsNotDeleted(t *testing.T, db *gorm.DB) {
	store := memory.New()
	c := claims.New(db, store)
	upload(t, c, store, "u/1")
	ctx := context.Background()
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := c.Hold(ctx, tx, "u/1"); err != nil {
			return err
		}
		return insert("u/1")(tx)
	}); err != nil {
		t.Fatal(err)
	}
	deleted, err := c.Abandon(ctx, "u/1")
	if err != nil || deleted || !exists(t, store, "u/1") {
		t.Fatalf("Abandon of a committed hold = %v, %v (object %v); want kept", deleted, err, exists(t, store, "u/1"))
	}
}

func testSweep(t *testing.T, db *gorm.DB) {
	store := memory.New()
	c := claims.New(db, store)
	ctx := context.Background()
	upload(t, c, store, "u/stale")
	upload(t, c, store, "u/held")
	if err := c.CreateWith(ctx, []string{"u/held"}, insert("u/held")); err != nil {
		t.Fatal(err)
	}
	upload(t, c, store, "u/deleting") // a delete interrupted after its record went
	if err := c.CreateWith(ctx, []string{"u/deleting"}, insert("u/deleting")); err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return c.Release(ctx, tx, "u/deleting") }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, "u/unclaimed", strings.NewReader("x"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	// A grace longer than the pending claim's age keeps it.
	if res, err := c.Sweep(ctx, time.Hour); err != nil || res.Abandoned != 0 || res.Finished != 1 {
		t.Fatalf("Sweep(1h) = %+v, %v; want nothing abandoned, the interrupted delete finished", res, err)
	}
	if !exists(t, store, "u/stale") || exists(t, store, "u/deleting") {
		t.Fatal("Sweep(1h) abandoned a fresh pending upload or did not finish the delete")
	}
	time.Sleep(10 * time.Millisecond)
	res, err := c.Sweep(ctx, time.Millisecond)
	if err != nil || res.Abandoned != 1 {
		t.Fatalf("Sweep = %+v, %v; want the stale pending upload abandoned", res, err)
	}
	for key, want := range map[string]bool{"u/stale": false, "u/held": true, "u/unclaimed": true} {
		if got := exists(t, store, key); got != want {
			t.Errorf("after Sweep %s exists = %v, want %v", key, got, want)
		}
	}

	// A backlog larger than a batch is swept in full, page by page.
	defer claims.SetSweepBatch(2)()
	for i := range 5 {
		upload(t, c, store, fmt.Sprintf("u/backlog-%d", i))
		upload(t, c, store, fmt.Sprintf("u/doomed-%d", i))
		if err := c.CreateWith(ctx, []string{fmt.Sprintf("u/doomed-%d", i)}, insert(fmt.Sprintf("u/doomed-%d", i))); err != nil {
			t.Fatal(err)
		}
		if err := db.Transaction(func(tx *gorm.DB) error { return c.Release(ctx, tx, fmt.Sprintf("u/doomed-%d", i)) }); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(10 * time.Millisecond)
	if res, err := c.Sweep(ctx, time.Millisecond); err != nil || res.Abandoned != 5 || res.Finished != 5 {
		t.Fatalf("Sweep of a backlog = %+v, %v; want 5 abandoned and 5 finished", res, err)
	}
}

func testDeleteWith(t *testing.T, db *gorm.DB) {
	store := memory.New()
	c := claims.New(db, store)
	ctx := context.Background()
	upload(t, c, store, "u/1")
	if err := c.CreateWith(ctx, []string{"u/1"}, insert("u/1")); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("constraint")
	if err := c.DeleteWith(ctx, []string{"u/1"}, func(*gorm.DB) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("DeleteWith = %v, want the record's error", err)
	}
	if !exists(t, store, "u/1") || state(t, db, "u/1") != claims.Held {
		t.Fatal("a failed record delete removed the file or its hold")
	}
	if err := c.DeleteWith(ctx, []string{"u/1"}, func(tx *gorm.DB) error { return tx.Where("file_key = ?", "u/1").Delete(&doc{}).Error }); err != nil {
		t.Fatal(err)
	}
	if exists(t, store, "u/1") || state(t, db, "u/1") != "none" {
		t.Fatal("after DeleteWith the file or its claim remains")
	}

	// A record whose file has no claim (stored before claims, or none):
	// the record is deleted, and the file is left alone.
	if _, err := store.Put(ctx, "u/legacy", strings.NewReader("x"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&doc{FileKey: "u/legacy"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteWith(ctx, []string{"u/legacy"}, func(tx *gorm.DB) error { return tx.Where("file_key = ?", "u/legacy").Delete(&doc{}).Error }); err != nil {
		t.Fatalf("DeleteWith of an unclaimed key = %v", err)
	}
	var n int64
	db.Model(&doc{}).Where("file_key = ?", "u/legacy").Count(&n)
	if n != 0 || !exists(t, store, "u/legacy") {
		t.Fatalf("unclaimed: %d records left, file exists = %v; want the record gone and the file kept", n, exists(t, store, "u/legacy"))
	}

	// A claim that is not held is not this record's: nothing changes.
	upload(t, c, store, "u/pending")
	if err := db.Create(&doc{FileKey: "u/pending"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteWith(ctx, []string{"u/pending"}, func(tx *gorm.DB) error { return tx.Where("file_key = ?", "u/pending").Delete(&doc{}).Error }); !errors.Is(err, claims.ErrNotHeld) {
		t.Fatalf("DeleteWith of a pending key = %v, want ErrNotHeld", err)
	}
	db.Model(&doc{}).Where("file_key = ?", "u/pending").Count(&n)
	if n != 1 || !exists(t, store, "u/pending") || state(t, db, "u/pending") != claims.Pending {
		t.Fatal("a refused DeleteWith changed the record, the file, or the claim")
	}
}

// testConfirmRacesSweep: a confirmation (CreateWith) and a Sweep that finds
// the same upload stale race, many times; whatever the order, a record
// never refers to a missing object: either the confirmation won (record,
// object, held) or the sweep did (no record).
func testConfirmRacesSweep(t *testing.T, db *gorm.DB) {
	store := memory.New()
	c := claims.New(db, store)
	ctx := context.Background()
	confirmed, swept := 0, 0
	for i := 0; i < 40; i++ {
		key := fmt.Sprintf("u/race-%d", i)
		upload(t, c, store, key)
		var wg sync.WaitGroup
		var createErr error
		wg.Add(2)
		jitter := func() { time.Sleep(time.Duration(rand.IntN(3000)) * time.Microsecond) } //nolint:gosec // scheduling jitter, not a secret
		go func() { defer wg.Done(); jitter(); createErr = c.CreateWith(ctx, []string{key}, insert(key)) }()
		go func() { defer wg.Done(); jitter(); _, _ = c.Sweep(ctx, 0) }()
		wg.Wait()
		var n int64
		db.Model(&doc{}).Where("file_key = ?", key).Count(&n)
		switch n {
		case 1:
			confirmed++
			if !exists(t, store, key) || state(t, db, key) != claims.Held {
				t.Fatalf("round %d: a record refers to %s, but the object exists = %v and its claim is %s", i, key, exists(t, store, key), state(t, db, key))
			}
			if createErr != nil {
				t.Fatalf("round %d: the record committed but CreateWith = %v", i, createErr)
			}
		case 0:
			swept++
			if createErr == nil {
				t.Fatalf("round %d: CreateWith succeeded without a record", i)
			}
		}
	}
	t.Logf("%d confirmations won, %d sweeps won", confirmed, swept)
}

// testSweepFirstWins: the sweep claims a stale upload before its
// confirmation: the object is deleted, and the confirmation then fails
// (the key is no longer pending) without writing a record.
func testSweepFirstWins(t *testing.T, db *gorm.DB) {
	store := memory.New()
	c := claims.New(db, store)
	ctx := context.Background()
	upload(t, c, store, "u/1")
	if res, err := c.Sweep(ctx, 0); err != nil || res.Abandoned != 1 {
		t.Fatalf("Sweep = %+v, %v", res, err)
	}
	err := c.CreateWith(ctx, []string{"u/1"}, insert("u/1"))
	if !errors.Is(err, claims.ErrNotPending) {
		t.Fatalf("a confirmation after the sweep = %v, want ErrNotPending", err)
	}
	var n int64
	db.Model(&doc{}).Where("file_key = ?", "u/1").Count(&n)
	if n != 0 || exists(t, store, "u/1") {
		t.Fatalf("%d records, object %v; want neither", n, exists(t, store, "u/1"))
	}
}

// testConfirmationInFlightWins: a confirmation has held the key in a
// transaction not yet committed when a sweep finds the upload stale. The
// sweep's conditional update waits for that transaction (or fails, on an
// engine that refuses to wait), and either way does not delete the object
// the committed record then refers to.
func testConfirmationInFlightWins(t *testing.T, db *gorm.DB) {
	store := memory.New()
	c := claims.New(db, store)
	ctx := context.Background()
	upload(t, c, store, "u/1")
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	if err := c.Hold(ctx, tx, "u/1"); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	swept := make(chan error, 1)
	go func() {
		_, err := c.Sweep(ctx, 0)
		swept <- err
	}()
	time.Sleep(100 * time.Millisecond) // the sweep is now waiting on the held row (or has failed)
	if err := insert("u/1")(tx); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	<-swept // its error, if any, is an engine refusing to wait: it deleted nothing
	if !exists(t, store, "u/1") || state(t, db, "u/1") != claims.Held {
		t.Fatalf("after the in-flight confirmation committed: object %v, claim %s; want kept, held", exists(t, store, "u/1"), state(t, db, "u/1"))
	}
}

// testUpdate: one record, several files. Creating with two keys holds both,
// or abandons both; replacing a file holds the new one and deletes the old
// one only once the change commits; a failed replacement keeps the old one
// and abandons the new one; unchanged and empty keys are left alone.
func testUpdate(t *testing.T, db *gorm.DB) {
	store := memory.New()
	c := claims.New(db, store)
	ctx := context.Background()
	upload(t, c, store, "u/a")
	upload(t, c, store, "u/b")
	boom := errors.New("constraint")
	if err := c.CreateWith(ctx, []string{"u/a", "", "u/b"}, func(*gorm.DB) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("CreateWith = %v, want the record's error", err)
	}
	if exists(t, store, "u/a") || exists(t, store, "u/b") {
		t.Fatal("a failed create with two files kept one")
	}
	upload(t, c, store, "u/a")
	upload(t, c, store, "u/b")
	if err := c.CreateWith(ctx, []string{"u/a", "", "u/b"}, insert("u/a")); err != nil {
		t.Fatal(err)
	}
	if state(t, db, "u/a") != claims.Held || state(t, db, "u/b") != claims.Held {
		t.Fatal("CreateWith with two files did not hold both")
	}

	replace := func(from, to string) func(tx *gorm.DB) error {
		return func(tx *gorm.DB) error {
			return tx.Model(&doc{}).Where("file_key = ?", from).Update("file_key", to).Error
		}
	}
	// A key another record holds fails the change, naming the key.
	upload(t, c, store, "u/d")
	err := c.CreateWith(ctx, []string{"u/d", "u/a"}, insert("u/d"))
	var ke *claims.KeyError
	if !errors.Is(err, claims.ErrNotPending) || !errors.As(err, &ke) || ke.Key != "u/a" {
		t.Fatalf("CreateWith of a held key = %v, want a *claims.KeyError for u/a", err)
	}
	if !exists(t, store, "u/a") || exists(t, store, "u/d") {
		t.Fatal("the refused change deleted the held file or kept its pending one")
	}

	upload(t, c, store, "u/c")
	if err := c.Update(ctx, []string{"u/c"}, []string{"u/a"}, func(*gorm.DB) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("Update = %v, want the record's error", err)
	}
	if !exists(t, store, "u/a") || state(t, db, "u/a") != claims.Held || exists(t, store, "u/c") {
		t.Fatal("a failed replacement lost the old file or kept the new one")
	}
	upload(t, c, store, "u/c")
	if err := c.Update(ctx, []string{"u/c", "u/b"}, []string{"u/a", "u/b", ""}, replace("u/a", "u/c")); err != nil {
		t.Fatal(err)
	}
	if exists(t, store, "u/a") || state(t, db, "u/a") != "none" {
		t.Fatal("the replaced file or its claim remains")
	}
	if !exists(t, store, "u/c") || state(t, db, "u/c") != claims.Held {
		t.Fatal("the new file is not held")
	}
	if !exists(t, store, "u/b") || state(t, db, "u/b") != claims.Held {
		t.Fatal("a file in both lists (unchanged) was touched")
	}
}
