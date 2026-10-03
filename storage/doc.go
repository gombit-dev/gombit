// Package storage is Gombit's object storage contract: a driver-neutral
// Storage interface for files (uploads, avatars, reports, exports), so
// application code never depends on a filesystem path or an S3 SDK call.
//
//	info, err := store.Put(ctx, "avatars/42.png", r, storage.PutOptions{
//		ContentType: "image/png",
//	})
//	body, info, err := store.Open(ctx, "avatars/42.png")
//	defer body.Close()
//	err = store.Delete(ctx, "avatars/42.png")
//
// The contract, which every driver keeps and the storagetest suite checks:
//
//   - Streaming. Put reads an io.Reader to EOF and Open returns an
//     io.ReadCloser; neither requires the whole object in memory, a
//     seekable source, or a known length.
//   - Atomic writes. A Put that fails (its reader errors, its context
//     ends, its declared size is wrong) leaves the key as it was; no one
//     ever reads a partial object. The exception is ErrUnknownOutcome: a
//     remote backend was sent the object and never confirmed or denied
//     storing it, so the key holds the old object or the new one, whole.
//   - Portable keys. ValidateKey is the same rule on every driver, and it
//     rejects any key that could climb out of a prefix ("..", a leading
//     '/', a backslash).
//   - Classified errors. ErrNotFound, ErrInvalidKey, ErrInvalidOptions,
//     ErrSizeMismatch, ErrUnsupported, ErrUnavailable, and
//     ErrUnknownOutcome match with
//     errors.Is on every driver, and MapError turns them into D10 errors
//     for a handler.
//
// Authorization stays with the application: decide who may read or write
// an object before calling Storage, and before handing out a URL.
package storage
