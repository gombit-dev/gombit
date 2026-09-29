package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/gombit-dev/gombit/contract"
)

// Sentinels for errors.Is. Drivers wrap them (usually in an *Error), so
// application code classifies a failure the same way on every driver.
var (
	// ErrNotFound: no object is stored under the key.
	ErrNotFound = errors.New("storage: object not found")
	// ErrInvalidKey: the key breaks the key rules (see ValidateKey).
	ErrInvalidKey = errors.New("storage: invalid object key")
	// ErrInvalidOptions: a PutOptions or URLOptions value is invalid (a
	// malformed content type or metadata, a negative size or expiry).
	ErrInvalidOptions = errors.New("storage: invalid options")
	// ErrSizeMismatch: the bytes read differ in length from the declared
	// PutOptions.Size.
	ErrSizeMismatch = errors.New("storage: object length differs from its declared size")
	// ErrUnsupported: the driver cannot do this (a URL from a driver with no
	// URL scheme).
	ErrUnsupported = errors.New("storage: operation not supported by this driver")
	// ErrUnavailable: the backend could not be reached or failed
	// transiently; retrying later may succeed.
	ErrUnavailable = errors.New("storage: backend unavailable")
)

// Error is a failed storage operation: what was attempted, on which key,
// and why. Err is the classified cause (one of the sentinels, possibly
// wrapping the driver's own error), reachable with errors.Is/As.
type Error struct {
	Op  string // "put", "open", "stat", "delete", "url"
	Key string
	Err error
}

func (e *Error) Error() string {
	return fmt.Sprintf("storage: %s %q: %v", e.Op, e.Key, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Wrap returns err as an *Error for op on key, or nil when err is nil. An
// err that is already an *Error is returned unchanged.
func Wrap(op, key string, err error) error {
	if err == nil {
		return nil
	}
	var se *Error
	if errors.As(err, &se) {
		return err
	}
	return &Error{Op: op, Key: key, Err: err}
}

// MapError maps a storage error to a D10 category error for a handler:
//
//   - ErrNotFound becomes not_found (with notFound as the message);
//   - ErrInvalidOptions and ErrSizeMismatch become validation (the request
//     sent a content type, metadata, or length that cannot be stored);
//   - ErrUnavailable, and a context that ended (the request timed out or
//     the client went away), become dependency_unavailable;
//   - anything else, ErrInvalidKey included, becomes internal (with
//     internal as the message): keys are built by the server, so an invalid
//     one is a server bug. Validate a key taken from a request with
//     ValidateKey first, and answer not_found.
//
// The driver's own error text never reaches the client.
func MapError(ctx context.Context, err error, notFound, internal string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotFound):
		return contract.WithContext(ctx, contract.NotFound(notFound))
	case errors.Is(err, ErrInvalidOptions):
		return contract.WithContext(ctx, contract.Validation("The object's content type or metadata is invalid.", nil))
	case errors.Is(err, ErrSizeMismatch):
		return contract.WithContext(ctx, contract.Validation("The upload's length does not match its declared size.", nil))
	case errors.Is(err, ErrUnavailable):
		return contract.WithContext(ctx, contract.DependencyUnavailable("File storage is temporarily unavailable."))
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return contract.WithContext(ctx, contract.DependencyUnavailable("The file storage request did not finish in time."))
	default:
		return contract.WithContext(ctx, contract.Internal(internal))
	}
}
