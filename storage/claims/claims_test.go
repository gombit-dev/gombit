package claims_test

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	"github.com/gombit-dev/gombit/storage/upload"
)

// doc is a record that refers to a stored file, one record per file.
type doc struct {
	ID      uint   `gorm:"primaryKey"`
	FileKey string `gorm:"size:512;uniqueIndex"`
}

func (doc) TableName() string { return "claims_test_docs" }

// ended is a lease that ended long ago.
var ended = time.Now().Add(-time.Hour)

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
		"Lease":                     testLease,
		"UploadOutlivesItsClaim":    testUploadOutlivesItsClaim,
		"Staged":                    testStaged,
		"LatePutOnlyStages":         testLatePutOnlyStages,
		"PromotionIsNotSwept":       testPromotionIsNotSwept,
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

// uploaded stores an object under a newly claimed key, as an upload with
// Policy.Claims does, whose lease has already ended (the upload is done):
// deleting its claim removes the row at once. testLease covers leases
// still running.
func uploaded(t *testing.T, c *claims.Claims, store storage.Storage, key string) {
	t.Helper()
	ctx := context.Background()
	if err := c.Pending(ctx, key, ended); err != nil {
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
	if err := c.Pending(ctx, "u/1", ended); err != nil {
		t.Fatal(err)
	}
	if err := c.Pending(ctx, "u/1", ended); !errors.Is(err, claims.ErrClaimed) {
		t.Fatalf("a second Pending = %v, want ErrClaimed", err)
	}
	// Refused up front on every dialect (SQLite would store it; the others
	// would fail with an error of their own).
	long := "u/" + strings.Repeat("k", claims.MaxKeyLen)
	if err := c.Pending(ctx, long, ended); !errors.Is(err, claims.ErrKeyTooLong) {
		t.Fatalf("Pending of a %d-byte key = %v, want ErrKeyTooLong", len(long), err)
	}
	if err := c.Pending(ctx, strings.Repeat("k", claims.MaxKeyLen), ended); err != nil {
		t.Fatalf("Pending of a %d-byte key = %v", claims.MaxKeyLen, err)
	}
}

func testCreateHolds(t *testing.T, db *gorm.DB) {
	store := memory.New()
	c := claims.New(db, store)
	uploaded(t, c, store, "u/1")
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
	uploaded(t, c, store, "u/1")
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
	uploaded(t, c, store, "u/1")
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
	uploaded(t, c, store, "u/1")
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
	uploaded(t, c, store, "u/stale")
	uploaded(t, c, store, "u/held")
	if err := c.CreateWith(ctx, []string{"u/held"}, insert("u/held")); err != nil {
		t.Fatal(err)
	}
	uploaded(t, c, store, "u/deleting") // a delete interrupted after its record went
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
		uploaded(t, c, store, fmt.Sprintf("u/backlog-%d", i))
		uploaded(t, c, store, fmt.Sprintf("u/doomed-%d", i))
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
	uploaded(t, c, store, "u/1")
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
	uploaded(t, c, store, "u/pending")
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
		uploaded(t, c, store, key)
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
	uploaded(t, c, store, "u/1")
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
	uploaded(t, c, store, "u/1")
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
	uploaded(t, c, store, "u/a")
	uploaded(t, c, store, "u/b")
	boom := errors.New("constraint")
	if err := c.CreateWith(ctx, []string{"u/a", "", "u/b"}, func(*gorm.DB) error { return boom }); err != boom { //nolint:errorlint // the error itself, unwrapped
		t.Fatalf("CreateWith = %v, want the record's error itself", err)
	}
	if exists(t, store, "u/a") || exists(t, store, "u/b") {
		t.Fatal("a failed create with two files kept one")
	}
	uploaded(t, c, store, "u/a")
	uploaded(t, c, store, "u/b")
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
	uploaded(t, c, store, "u/d")
	err := c.CreateWith(ctx, []string{"u/d", "u/a"}, insert("u/d"))
	var ke *claims.KeyError
	if !errors.Is(err, claims.ErrNotPending) || !errors.As(err, &ke) || ke.Key != "u/a" {
		t.Fatalf("CreateWith of a held key = %v, want a *claims.KeyError for u/a", err)
	}
	if !exists(t, store, "u/a") || exists(t, store, "u/d") {
		t.Fatal("the refused change deleted the held file or kept its pending one")
	}

	uploaded(t, c, store, "u/c")
	if err := c.Update(ctx, []string{"u/c"}, []string{"u/a"}, func(*gorm.DB) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("Update = %v, want the record's error", err)
	}
	if !exists(t, store, "u/a") || state(t, db, "u/a") != claims.Held || exists(t, store, "u/c") {
		t.Fatal("a failed replacement lost the old file or kept the new one")
	}
	uploaded(t, c, store, "u/c")
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

// expire ends the lease of key's claim (as if its upload's deadline, plus
// LeaseMargin, had passed).
func expire(t *testing.T, db *gorm.DB, key string) {
	t.Helper()
	if err := db.Model(&claims.Claim{}).Where("object_key = ?", key).
		Update("lease_until", time.Now().Add(-claims.LeaseMargin-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
}

// testLease: a claim whose upload may still be writing is abandoned, but
// its row stays as a tombstone until the lease ends: an object the upload
// publishes afterwards is deleted by the next sweep, and no record can take
// the key. Only after the lease is the row removed.
func testLease(t *testing.T, db *gorm.DB) {
	store := memory.New()
	c := claims.New(db, store)
	ctx := context.Background()
	if err := c.Pending(ctx, "u/slow", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// The upload is still streaming: nothing is stored yet.
	if res, err := c.Sweep(ctx, 0); err != nil || res.Abandoned != 1 || res.Finished != 0 {
		t.Fatalf("Sweep = %+v, %v; want it abandoned and kept as a tombstone", res, err)
	}
	if state(t, db, "u/slow") != claims.Deleting {
		t.Fatalf("claim = %s, want a deleting tombstone", state(t, db, "u/slow"))
	}
	// The upload publishes after all.
	if _, err := store.Put(ctx, "u/slow", strings.NewReader("late"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return c.Hold(ctx, tx, "u/slow") }); !errors.Is(err, claims.ErrNotPending) {
		t.Fatalf("Hold of a tombstone = %v, want ErrNotPending", err)
	}
	if res, err := c.Sweep(ctx, 0); err != nil || res.Waiting != 1 || exists(t, store, "u/slow") {
		t.Fatalf("Sweep = %+v, %v (object exists = %v); want the late object deleted, the tombstone kept", res, err, exists(t, store, "u/slow"))
	}
	// A lease that ended a moment ago is within LeaseMargin (clocks that
	// disagree, a publish already sent): the tombstone stays.
	if err := db.Model(&claims.Claim{}).Where("object_key = ?", "u/slow").Update("lease_until", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if res, err := c.Sweep(ctx, 0); err != nil || res.Waiting != 1 {
		t.Fatalf("Sweep within the margin = %+v, %v; want the tombstone kept", res, err)
	}
	expire(t, db, "u/slow")
	if res, err := c.Sweep(ctx, 0); err != nil || res.Finished != 1 || state(t, db, "u/slow") != "none" {
		t.Fatalf("Sweep after the lease = %+v, %v; want the tombstone removed", res, err)
	}
}

// blocked is an upload body that sends its first bytes, then blocks until
// released, as a slow client does.
type blocked struct {
	head    []byte
	release chan struct{}
}

func (b *blocked) Read(p []byte) (int, error) {
	if len(b.head) > 0 {
		n := copy(p, b.head)
		b.head = b.head[n:]
		return n, nil
	}
	<-b.release
	return 0, io.EOF
}

// testUploadOutlivesItsClaim: the schedule a claims-only sweep must
// survive. upload.Save claims its key and blocks in Put; the sweep
// abandons the claim; then the Put publishes. The object is not an orphan:
// the tombstone is still there, the next sweep deletes the object, and no
// record can take the key.
func testUploadOutlivesItsClaim(t *testing.T, db *gorm.DB) {
	store := memory.New()
	c := claims.New(db, store)
	ctx := context.Background()
	p := upload.Policy{MaxBytes: 1 << 20, Types: []string{"*/*"}, Prefix: "u/", Claims: c}
	body := &blocked{head: []byte(strings.Repeat("x", 2*upload.SniffBytes)), release: make(chan struct{})}
	type result struct {
		f   upload.File
		err error
	}
	done := make(chan result, 1)
	go func() {
		f, err := upload.Save(ctx, store, body, "slow.txt", p)
		done <- result{f, err}
	}()
	// Wait for the claim: Save is now in Put, reading the blocked body.
	var key string
	for deadline := time.Now().Add(10 * time.Second); key == ""; {
		var keys []string
		db.Model(&claims.Claim{}).Pluck("object_key", &keys)
		if len(keys) == 1 {
			key = keys[0]
		} else if time.Now().After(deadline) {
			t.Fatal("Save never claimed its key")
		} else {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if res, err := c.Sweep(ctx, 0); err != nil || res.Abandoned != 1 {
		t.Fatalf("Sweep during the upload = %+v, %v", res, err)
	}
	close(body.release)
	r := <-done
	if r.err != nil || r.f.Key != key {
		t.Fatalf("Save = %+v, %v", r.f, r.err)
	}
	if !exists(t, store, key) || state(t, db, key) != claims.Deleting {
		t.Fatalf("after the late publish: object exists = %v, claim %s; want both (the tombstone)", exists(t, store, key), state(t, db, key))
	}
	if err := c.CreateWith(ctx, []string{key}, insert(key)); !errors.Is(err, claims.ErrNotPending) {
		t.Fatalf("CreateWith of the swept upload = %v, want ErrNotPending", err)
	}
	if _, err := c.Sweep(ctx, 0); err != nil || exists(t, store, key) {
		t.Fatalf("the next sweep = %v, object exists = %v; want it deleted", err, exists(t, store, key))
	}
	expire(t, db, key)
	if _, err := c.Sweep(ctx, 0); err != nil || state(t, db, key) != "none" {
		t.Fatalf("the sweep after the lease = %v, claim %s", err, state(t, db, key))
	}
}

// stage claims key for a direct upload and stores its staged object, as
// upload.Authorize and the client's PUT do.
func stage(t *testing.T, c *claims.Claims, store storage.Storage, key string, until time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := c.Stage(ctx, key, until); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, upload.StagingKey(key), strings.NewReader("staged "+key), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
}

// testStaged: a staged claim cannot be held before it is promoted (its key
// was never written by the application); Promote succeeds once; a promoted
// claim is held; Unpromote puts a promoting claim back; abandoning a staged
// claim deletes its staging object too.
func testStaged(t *testing.T, db *gorm.DB) {
	store := memory.New()
	c := claims.New(db, store)
	ctx := context.Background()
	stage(t, c, store, "u/s", ended)
	hold := func(key string) error {
		return db.Transaction(func(tx *gorm.DB) error { return c.Hold(ctx, tx, key) })
	}
	if err := hold("u/s"); !errors.Is(err, claims.ErrNotPending) {
		t.Fatalf("Hold of a staged, unpromoted key = %v, want ErrNotPending", err)
	}
	if ok, err := c.Promote(ctx, "u/s", time.Now().Add(time.Hour)); err != nil || !ok {
		t.Fatalf("Promote = %v, %v", ok, err)
	}
	if ok, err := c.Promote(ctx, "u/s", time.Now().Add(time.Hour)); err != nil || ok {
		t.Fatalf("a second Promote = %v, %v; want false", ok, err)
	}
	var claim claims.Claim
	if err := db.Where("object_key = ?", "u/s").Take(&claim).Error; err != nil || claim.State != claims.Promoting || time.Until(claim.LeaseUntil) < 50*time.Minute {
		t.Fatalf("promoted claim = %+v, %v; want promoting, the lease extended", claim, err)
	}
	if err := c.Unpromote(ctx, "u/s"); err != nil || state(t, db, "u/s") != claims.Pending {
		t.Fatalf("Unpromote = %v, claim %s", err, state(t, db, "u/s"))
	}
	if ok, _ := c.Promote(ctx, "u/s", time.Now()); !ok {
		t.Fatal("Promote after Unpromote failed")
	}
	if err := hold("u/s"); err != nil || state(t, db, "u/s") != claims.Held {
		t.Fatalf("Hold of a promoted key = %v, claim %s", err, state(t, db, "u/s"))
	}
	if ok, _ := c.Promote(ctx, "u/1-not-staged", time.Now()); ok {
		t.Fatal("Promote of an unclaimed key succeeded")
	}
	uploaded(t, c, store, "u/plain")
	if ok, _ := c.Promote(ctx, "u/plain", time.Now()); ok {
		t.Fatal("Promote of a key stored by the application (not staged) succeeded")
	}

	stage(t, c, store, "u/gone", ended)
	if deleted, err := c.Abandon(ctx, "u/gone"); err != nil || !deleted {
		t.Fatalf("Abandon = %v, %v", deleted, err)
	}
	if exists(t, store, upload.StagingKey("u/gone")) || state(t, db, "u/gone") != "none" {
		t.Fatal("abandoning a staged claim left its staging object or claim")
	}
}

// testLatePutOnlyStages: the S3 schedule. A client starts a presigned PUT
// just before its grant expires and trickles the body; the sweep abandons
// the claim, and its tombstone ends; then the PUT completes. It can only
// publish the staging object: the claimed key is never written, and
// SweepStaging deletes the leftover. A promoted (held) key's leftover
// staged copy goes too; staging objects of live claims stay.
func testLatePutOnlyStages(t *testing.T, db *gorm.DB) {
	store := memory.New()
	c := claims.New(db, store)
	ctx := context.Background()
	if err := c.Stage(ctx, "u/late", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if res, err := c.Sweep(ctx, 0); err != nil || res.Abandoned != 1 {
		t.Fatalf("Sweep = %+v, %v", res, err)
	}
	expire(t, db, "u/late")
	if _, err := c.Sweep(ctx, 0); err != nil || state(t, db, "u/late") != "none" {
		t.Fatalf("the sweep after the lease = %v, claim %s; want the tombstone gone", err, state(t, db, "u/late"))
	}
	// The trickled PUT completes now, long after everything.
	if _, err := store.Put(ctx, upload.StagingKey("u/late"), strings.NewReader("late"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if exists(t, store, "u/late") {
		t.Fatal("a direct upload wrote the claimed key")
	}
	stage(t, c, store, "u/live", time.Now().Add(time.Hour))
	stage(t, c, store, "u/held", ended)
	if ok, _ := c.Promote(ctx, "u/held", time.Now()); !ok {
		t.Fatal("Promote failed")
	}
	if err := c.CreateWith(ctx, []string{"u/held"}, insert("u/held")); err != nil {
		t.Fatal(err)
	}
	n, err := c.SweepStaging(ctx)
	if err != nil || n != 2 {
		t.Fatalf("SweepStaging = %d, %v; want the late and the promoted leftovers", n, err)
	}
	if exists(t, store, upload.StagingKey("u/late")) || exists(t, store, upload.StagingKey("u/held")) || !exists(t, store, upload.StagingKey("u/live")) {
		t.Fatal("SweepStaging deleted the wrong staging objects")
	}
}

// testPromotionIsNotSwept: a sweep never abandons a promoting claim before
// its lease ends (the application may be copying to the key); after it,
// the claim is abandoned like a stale pending one, and its key and staged
// object are deleted.
func testPromotionIsNotSwept(t *testing.T, db *gorm.DB) {
	store := memory.New()
	c := claims.New(db, store)
	ctx := context.Background()
	stage(t, c, store, "u/p", ended)
	if ok, _ := c.Promote(ctx, "u/p", time.Now().Add(time.Hour)); !ok {
		t.Fatal("Promote failed")
	}
	if _, err := store.Put(ctx, "u/p", strings.NewReader("copied"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if res, err := c.Sweep(ctx, 0); err != nil || res.Abandoned != 0 || state(t, db, "u/p") != claims.Promoting {
		t.Fatalf("Sweep during the promotion = %+v, %v, claim %s", res, err, state(t, db, "u/p"))
	}
	expire(t, db, "u/p")
	if res, err := c.Sweep(ctx, 0); err != nil || res.Abandoned != 1 {
		t.Fatalf("Sweep after the promotion's lease = %+v, %v", res, err)
	}
	if exists(t, store, "u/p") || exists(t, store, upload.StagingKey("u/p")) {
		t.Fatal("the abandoned promotion left its key or staged object")
	}
}
