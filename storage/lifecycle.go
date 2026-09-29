package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Filename is the object's filename (the FilenameMetadata metadata), for
// display and Content-Disposition; empty when it was stored without one.
func (o ObjectInfo) Filename() string { return o.Metadata[FilenameMetadata] }

// Lister is a Storage that can enumerate its objects, which cleaning up
// after abandoned uploads needs (Sweep). The local, memory, and S3 drivers
// implement it.
type Lister interface {
	// List calls fn with every object whose key starts with prefix ("" for
	// all), in an order the driver chooses, until fn returns an error,
	// which List then returns. Each ObjectInfo has the Key, Size, ETag, and
	// ModTime; ContentType and Metadata may be empty (an S3 listing does
	// not carry them: Stat the object for them). fn may delete the object
	// it is given. An object stored or deleted while List runs may or may
	// not be seen.
	List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error
}

// List enumerates s's objects under prefix (Lister), or fails with
// ErrUnsupported when s cannot.
func List(ctx context.Context, s Storage, prefix string, fn func(ObjectInfo) error) error {
	l, ok := s.(Lister)
	if !ok {
		return Wrap("list", prefix, fmt.Errorf("%w: %T cannot list its objects", ErrUnsupported, s))
	}
	return l.List(ctx, prefix, fn)
}

// DeleteIfFails runs fn, the work that records a stored object (typically
// the database insert that references key), and deletes the object when fn
// fails, so a failed insert leaves no unreferenced file behind. The
// deletion outlives ctx (a request that failed because its client went
// away still cleans up), and its own failure is joined to fn's error. The
// result is fn's error: nil when the object is kept.
//
// Use it only for an object the failed work was the sole reference to (a
// file just uploaded under a generated key), never for one that may be
// shared.
func DeleteIfFails(ctx context.Context, s Storage, key string, fn func() error) error {
	err := fn()
	if err == nil {
		return nil
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if derr := s.Delete(dctx, key); derr != nil {
		return errors.Join(err, fmt.Errorf("storage: delete %q after the failure: %w", key, derr))
	}
	return err
}

// DeleteOwned deletes key when it is owned under ownedPrefix, the ownership
// contract: a record owns the objects under the prefix its field stores
// them under (upload.Policy.Prefix, whose keys are generated one per file)
// and nothing else. A key outside ownedPrefix (a shared file, another
// field's, or one the application chose) is not deleted, and the result
// is false. Call it after the record's deletion has committed: deleting
// first loses the file of a record whose deletion then fails.
//
// ownedPrefix must end with '/' (ErrInvalidOptions otherwise), so "avatars"
// never owns "avatars-shared/...".
func DeleteOwned(ctx context.Context, s Storage, key, ownedPrefix string) (bool, error) {
	if err := ValidatePublicPrefix(ownedPrefix); err != nil || ownedPrefix == "" {
		return false, fmt.Errorf("%w: owned prefix %q must be a key path ending with '/'", ErrInvalidOptions, ownedPrefix)
	}
	if !strings.HasPrefix(key, ownedPrefix) {
		return false, nil
	}
	if err := s.Delete(ctx, key); err != nil {
		return false, err
	}
	return true, nil
}

// SweepResult is what Sweep did.
type SweepResult struct {
	// Checked is how many objects were old enough to consider.
	Checked int
	// Deleted is how many of them were deleted.
	Deleted int
}

// Sweep deletes abandoned objects under prefix: those last stored more
// than olderThan ago that referenced reports nothing refers to (typically
// a database lookup of the key). It is the cleanup for uploads never
// recorded: a direct upload granted but never confirmed, or a file stored
// whose record was never written (a crash between the two). Run it
// periodically (a job), under a prefix whose objects are owned one per
// record (see DeleteOwned), with olderThan longer than an upload can take
// to be recorded (for direct uploads, more than the grant's lifetime).
//
// referenced is called once per candidate: keep it an indexed lookup of
// the key. A referenced error stops the sweep and is returned, with what
// was done so far. It needs a Lister (ErrUnsupported otherwise).
func Sweep(ctx context.Context, s Storage, prefix string, olderThan time.Duration, referenced func(ctx context.Context, key string) (bool, error)) (SweepResult, error) {
	var res SweepResult
	if olderThan <= 0 {
		return res, fmt.Errorf("%w: Sweep needs a positive age, not %s", ErrInvalidOptions, olderThan)
	}
	if err := ValidatePublicPrefix(prefix); err != nil || prefix == "" {
		return res, fmt.Errorf("%w: sweep prefix %q must be a key path ending with '/'", ErrInvalidOptions, prefix)
	}
	cutoff := time.Now().Add(-olderThan)
	err := List(ctx, s, prefix, func(o ObjectInfo) error {
		if o.ModTime.IsZero() || !o.ModTime.Before(cutoff) {
			return nil
		}
		res.Checked++
		used, err := referenced(ctx, o.Key)
		if err != nil {
			return err
		}
		if used {
			return nil
		}
		if err := s.Delete(ctx, o.Key); err != nil {
			return err
		}
		res.Deleted++
		return nil
	})
	return res, err
}
