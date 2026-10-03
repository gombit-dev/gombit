// Package claims is the ownership protocol for stored files: which object a
// record owns, so that cleaning up never deletes a file a record refers to.
//
// A file and the database record that refers to it are two writes in two
// systems. Neither "the insert returned an error" nor "no record refers to
// it right now" proves a file is unreferenced: an insert can commit and
// lose its answer, and a record can be written a moment after the check.
// Claims makes the database the authority instead. Every file under the
// protocol has a row in storage_claims, keyed by its storage key, in one of
// three states:
//
//	pending   stored (or about to be) and referenced by no record yet
//	held      referenced by a record: never deleted
//	deleting  being deleted: no record may take it
//
// The transitions are conditional updates, so each is atomic against the
// others, across processes:
//
//	Pending         (none)  -> pending   when an upload's key is generated
//	Hold     (tx)   pending -> held      in the transaction writing the record
//	Release  (tx)   held    -> deleting  in the transaction deleting the record
//	Abandon         pending -> deleting  then the object, then the row
//	Sweep           pending -> deleting  for stale pending claims, likewise
//
// Only a deleting claim's object is ever deleted, and a key reaches
// deleting only from pending (no record holds it) or through Release (the
// transaction deleting its record). So a Hold and an Abandon or Sweep of
// one key cannot both win: one transition commits first, and the other's
// condition no longer matches. A record whose insert committed although the
// caller saw an error holds its key, so the cleanup that follows finds it
// held and keeps the object.
//
// Use CreateWith to write a record that refers to new uploads, Update to
// change which files a record refers to, DeleteWith to delete a record, and
// Sweep, periodically, for uploads no record ever took.
// Objects without a claim are outside the protocol, and nothing here ever
// deletes them. Storage drivers know nothing of claims: storage.Storage is
// objects only.
package claims

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/gombit-dev/gombit/storage"
)

// Claim is a row of storage_claims: the ownership state of one stored
// object. Add it to the application's migrations (Models).
type Claim struct {
	// Key is the object's storage key, at most MaxKeyLen bytes (column
	// object_key: "key" is a reserved word in MySQL).
	Key string `gorm:"column:object_key;primaryKey;size:512"`
	// State is pending, held, or deleting.
	State string `gorm:"size:16;not null;index:idx_storage_claims_sweep,priority:1"`
	// CreatedAt is when the key was claimed; Sweep abandons a pending claim
	// once it is older than the sweep's grace period.
	CreatedAt time.Time `gorm:"index:idx_storage_claims_sweep,priority:2"`
	UpdatedAt time.Time
}

// MaxKeyLen is the longest key a claim can hold, in bytes: shorter than
// storage keys may be (1024), to fit a primary key on every database
// (MySQL's index limit). Generated upload keys are the policy's prefix
// plus 32 characters, so keep prefixes under 480 bytes.
const MaxKeyLen = 512

// TableName is storage_claims.
func (Claim) TableName() string { return "storage_claims" }

// Models are the tables claims needs, for an application's migrations.
func Models() []any { return []any{&Claim{}} }

// The states of a claim.
const (
	Pending  = "pending"
	Held     = "held"
	Deleting = "deleting"
)

var (
	// ErrClaimed: the key already has a claim (Pending).
	ErrClaimed = errors.New("claims: the key is already claimed")
	// ErrNotPending: the key's claim is not pending (Hold): it is already
	// held by a record, being deleted, or was never claimed.
	ErrNotPending = errors.New("claims: the key is not pending")
	// ErrNotHeld: the key's claim is not held (Release).
	ErrNotHeld = errors.New("claims: the key is not held")
	// ErrKeyTooLong: the key is longer than MaxKeyLen (Pending).
	ErrKeyTooLong = errors.New("claims: the key is too long")
)

// Claims runs the protocol over db (the table) and store (the objects).
type Claims struct {
	db    *gorm.DB
	store storage.Storage
	warn  func(msg string, err error)
}

// Option configures Claims.
type Option func(*Claims)

// WithWarn sets a function told about failures that do not fail the
// operation, such as an object whose delete failed after its record was
// gone (Sweep retries it).
func WithWarn(fn func(msg string, err error)) Option {
	return func(c *Claims) { c.warn = fn }
}

// New returns Claims over db and store.
func New(db *gorm.DB, store storage.Storage, opts ...Option) *Claims {
	c := &Claims{db: db, store: store}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Pending claims key for an upload about to store an object under it, which
// no record refers to yet. A key that already has a claim fails with
// ErrClaimed, and one longer than MaxKeyLen with ErrKeyTooLong. Uploads
// call it through upload.Policy.Claims.
func (c *Claims) Pending(ctx context.Context, key string) error {
	if len(key) > MaxKeyLen {
		return fmt.Errorf("%w: %d bytes, more than %d", ErrKeyTooLong, len(key), MaxKeyLen)
	}
	res := c.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&Claim{Key: key, State: Pending})
	if res.Error != nil {
		return fmt.Errorf("claims: pending %q: %w", key, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: %q", ErrClaimed, key)
	}
	return nil
}

// Hold moves key from pending to held, in tx, the transaction that writes
// the record referring to key: the record and the hold commit together, or
// neither does. A key that is not pending (already held, being deleted, or
// never claimed) fails with ErrNotPending, and tx should roll back.
func (c *Claims) Hold(ctx context.Context, tx *gorm.DB, key string) error {
	return transition(ctx, tx, key, Pending, Held, ErrNotPending)
}

// Release moves key from held to deleting, in tx, the transaction that
// deletes the record referring to key. Once tx commits, delete the object
// with Finish (DeleteWith does both). A key that is not held fails with
// ErrNotHeld.
func (c *Claims) Release(ctx context.Context, tx *gorm.DB, key string) error {
	return transition(ctx, tx, key, Held, Deleting, ErrNotHeld)
}

// transition moves key from one state to another, if it is in from: one
// conditional update, atomic against every other transition of key.
func transition(ctx context.Context, db *gorm.DB, key, from, to string, notFrom error) error {
	res := db.WithContext(ctx).Model(&Claim{}).
		Where("object_key = ? AND state = ?", key, from).
		Updates(map[string]any{"state": to, "updated_at": time.Now()})
	if res.Error != nil {
		return fmt.Errorf("claims: %s %q: %w", to, key, res.Error)
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("%w: %q", notFrom, key)
	}
	return nil
}

// CreateWith writes a record that refers to keys, new uploads' pending
// keys: fn writes the record in tx, and the keys are held in the same
// transaction. When the transaction fails, CreateWith abandons the
// uploads: it deletes each object, but only if its key is still pending.
// If the transaction in fact committed (its answer lost), or another
// request already holds a key (a retried confirmation), the key is held
// and its object is kept. Empty keys (an optional file left out) are
// skipped. The result is the transaction's error.
func (c *Claims) CreateWith(ctx context.Context, keys []string, fn func(tx *gorm.DB) error) error {
	return c.Update(ctx, keys, nil, fn)
}

// DeleteWith deletes a record that refers to keys: fn deletes the record in
// tx, and the keys are released in the same transaction. Once it commits,
// their objects are deleted (Finish); a failure to delete one then is
// reported through the warning hook, and Sweep finishes it. A key with no
// claim at all (a file stored before the application adopted claims) is
// outside the protocol: the record is deleted and the object is left
// alone. A claim that is not held (pending or deleting: not this
// record's) fails with ErrNotHeld, and the record is kept. Empty keys are
// skipped. The result is the transaction's error: nil means the record is
// gone.
func (c *Claims) DeleteWith(ctx context.Context, keys []string, fn func(tx *gorm.DB) error) error {
	return c.Update(ctx, nil, keys, fn)
}

// Update changes a record's files: fn writes the record in tx, the keys in
// hold (new uploads it now refers to) are held, and the keys in release
// (files it no longer refers to) are released, all in one transaction. If
// the transaction fails, the uploads in hold are abandoned, as by
// CreateWith; once it commits, the released files are deleted, as by
// DeleteWith. A key in both lists is unchanged and left out of both, and
// empty keys are skipped. CreateWith and DeleteWith are Update with only
// hold or only release.
func (c *Claims) Update(ctx context.Context, hold, release []string, fn func(tx *gorm.DB) error) error {
	hold, release = changed(hold, release)
	var released []string
	err := c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, key := range hold {
			if err := c.Hold(ctx, tx, key); err != nil {
				return err
			}
		}
		if err := fn(tx); err != nil {
			return err
		}
		for _, key := range release {
			claimed, err := c.release(ctx, tx, key)
			if err != nil {
				return err
			}
			if claimed {
				released = append(released, key)
			}
		}
		return nil
	})
	dctx, cancel := detached(ctx)
	defer cancel()
	if err != nil {
		errs := []error{err}
		for _, key := range hold {
			if _, aerr := c.Abandon(dctx, key); aerr != nil {
				errs = append(errs, aerr)
			}
		}
		return errors.Join(errs...)
	}
	for _, key := range released {
		if err := c.Finish(dctx, key); err != nil && c.warn != nil {
			c.warn("claims: a record let go of a file, but deleting it failed; Sweep will retry", err)
		}
	}
	return nil
}

// release is Release, except that a key with no claim at all is outside
// the protocol: it reports false, and nothing is to be deleted.
func (c *Claims) release(ctx context.Context, tx *gorm.DB, key string) (bool, error) {
	err := c.Release(ctx, tx, key)
	if !errors.Is(err, ErrNotHeld) {
		return err == nil, err
	}
	var n int64
	if cerr := tx.WithContext(ctx).Model(&Claim{}).Where("object_key = ?", key).Count(&n).Error; cerr != nil {
		return false, fmt.Errorf("claims: release %q: %w", key, cerr)
	}
	if n > 0 {
		return false, err
	}
	return false, nil
}

// changed is hold and release without empty keys, duplicates, or the keys
// in both (unchanged files).
func changed(hold, release []string) ([]string, []string) {
	in := func(keys []string) map[string]bool {
		m := make(map[string]bool, len(keys))
		for _, k := range keys {
			m[k] = true
		}
		return m
	}
	inHold, inRelease := in(hold), in(release)
	pick := func(keys []string, other map[string]bool) []string {
		var out []string
		seen := map[string]bool{"": true}
		for _, k := range keys {
			if !seen[k] && !other[k] {
				out = append(out, k)
			}
			seen[k] = true
		}
		return out
	}
	return pick(hold, inRelease), pick(release, inHold)
}

// Abandon deletes the object of key if its claim is pending (no record took
// it): pending moves to deleting, then the object is deleted, then the
// claim. It reports whether it deleted; a key held by a record, or with no
// claim, is left alone (false, nil).
func (c *Claims) Abandon(ctx context.Context, key string) (bool, error) {
	if err := transition(ctx, c.db, key, Pending, Deleting, ErrNotPending); err != nil {
		if errors.Is(err, ErrNotPending) {
			return false, nil
		}
		return false, err
	}
	return true, c.Finish(ctx, key)
}

// Finish completes a deleting claim: the object is deleted, then the claim.
// It does nothing for a key whose claim is not deleting.
func (c *Claims) Finish(ctx context.Context, key string) error {
	var claim Claim
	err := c.db.WithContext(ctx).Where("object_key = ? AND state = ?", key, Deleting).Take(&claim).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("claims: finish %q: %w", key, err)
	}
	if err := c.store.Delete(ctx, key); err != nil {
		return fmt.Errorf("claims: delete the object of %q: %w", key, err)
	}
	if err := c.db.WithContext(ctx).Where("object_key = ? AND state = ?", key, Deleting).Delete(&Claim{}).Error; err != nil {
		return fmt.Errorf("claims: forget %q: %w", key, err)
	}
	return nil
}

// SweepResult is what Sweep did.
type SweepResult struct {
	Abandoned int // stale pending claims whose objects were deleted
	Finished  int // deleting claims completed (interrupted deletes)
	Failed    int // keys that failed (see the error), retried next time
}

// Sweep cleans up after uploads no record ever took, and after interrupted
// deletes: it abandons every pending claim older than grace (moving it to
// deleting first, so a Hold racing it loses or wins cleanly), and finishes
// every deleting claim. Make grace longer than an upload's grant plus its
// confirmation can take: a pending claim younger than that may still be
// held. It reads claims, never lists the store: an object without a claim
// is never touched.
func (c *Claims) Sweep(ctx context.Context, grace time.Duration) (SweepResult, error) {
	var res SweepResult
	var errs []error
	cutoff := time.Now().Add(-grace)
	err := c.eachKey(ctx, func(db *gorm.DB) *gorm.DB {
		return db.Where("state = ? AND created_at < ?", Pending, cutoff)
	}, func(key string) {
		deleted, err := c.Abandon(ctx, key)
		switch {
		case err != nil:
			res.Failed++
			errs = append(errs, err)
		case deleted:
			res.Abandoned++
		}
	})
	if err == nil {
		err = c.eachKey(ctx, func(db *gorm.DB) *gorm.DB {
			return db.Where("state = ?", Deleting)
		}, func(key string) {
			if err := c.Finish(ctx, key); err != nil {
				res.Failed++
				errs = append(errs, err)
				return
			}
			res.Finished++
		})
	}
	if err != nil {
		errs = append(errs, fmt.Errorf("claims: sweep: %w", err))
	}
	return res, errors.Join(errs...)
}

// sweepBatch is how many keys Sweep reads at a time (a variable for the
// tests).
var sweepBatch = 500

// eachKey calls fn with the key of every claim where selects, in key order,
// sweepBatch keys at a time (so a large backlog is never held in memory at
// once), until they run out or ctx ends.
func (c *Claims) eachKey(ctx context.Context, where func(*gorm.DB) *gorm.DB, fn func(key string)) error {
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var keys []string
		q := where(c.db.WithContext(ctx).Model(&Claim{})).Where("object_key > ?", after)
		if err := q.Order("object_key").Limit(sweepBatch).Pluck("object_key", &keys).Error; err != nil {
			return err
		}
		for _, key := range keys {
			fn(key)
		}
		if len(keys) < sweepBatch {
			return nil
		}
		after = keys[len(keys)-1]
	}
}

// detached is ctx without its cancellation, bounded: cleanup that must run
// even when the request that needed it has ended.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}
