# Storage

Package `storage` is Gombit's object storage contract: one driver-neutral
interface for files such as uploads, avatars, reports and exports. Application
code depends on `storage.Storage`, never on a filesystem path or an S3 SDK
call, so moving from local development to S3-compatible storage in
production changes configuration, not code.

> **Status: the contract only (STORAGE-1).** No driver ships yet. The local
> filesystem and in-memory drivers (STORAGE-2) and the S3-compatible driver
> (STORAGE-3) implement this interface, and later issues add upload helpers,
> signed URLs, direct uploads, and admin fields
> ([epic #279](https://github.com/gombit-dev/gombit/issues/279)).

## The interface

```go
type Storage interface {
	Put(ctx context.Context, key string, r io.Reader, opts PutOptions) (ObjectInfo, error)
	Open(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	Delete(ctx context.Context, key string) error
	URL(ctx context.Context, key string, opts URLOptions) (string, error)
}
```

```go
info, err := store.Put(ctx, "avatars/42.png", r, storage.PutOptions{
	ContentType: "image/png",
	Metadata:    map[string]string{"original-name": header.Filename},
})

body, info, err := store.Open(ctx, "avatars/42.png")
if err != nil {
	return storage.MapError(ctx, err, "avatar not found", "could not read the avatar")
}
defer body.Close()
_, err = io.Copy(w, body)

ok, err := storage.Exists(ctx, store, "avatars/42.png")
err = store.Delete(ctx, "avatars/42.png")
```

### What every driver guarantees

| Guarantee | What it means |
| --- | --- |
| **Streaming** | `Put` reads an `io.Reader` to EOF in pieces, and `Open` returns an `io.ReadCloser`. Neither needs the whole object in memory, a seekable source, or a known length. |
| **Atomic writes** | A `Put` that fails leaves the key as it was: the previous object whole, or still absent. That covers the reader returning an error, the context ending, and a length that differs from the declared `Size`. No one ever reads a partial object. Concurrent `Put`s to one key leave one of them whole. |
| **Overwrite** | `Put` replaces an existing object, including its content type and metadata. |
| **Idempotent delete** | Deleting a missing object is not an error, so a retried delete is safe. |
| **Portable keys** | Every method validates its key with the same rule (below) and fails with `ErrInvalidKey` before touching the backend. |
| **Context** | Every method honors `ctx`. A `Put` canceled mid-stream fails with the context's error and stores nothing. |

### `PutOptions` and `ObjectInfo`

- **`ContentType`** is the object's media type. It must parse as a media
  type (`text/plain; charset=utf-8` is fine). Empty stores
  `application/octet-stream`.
- **`Size`**, when positive, is the exact length the reader will produce.
  `Put` then fails with `ErrSizeMismatch`, storing nothing, if the reader
  ends early or runs long. Zero means unknown.
- **`Metadata`** is small user metadata returned in `ObjectInfo`. Names are
  lower-case ASCII letters, digits and `-`. Values are UTF-8 without control
  characters. Names and values together are at most 2 KiB, S3's limit. Keep
  a client's original filename here, never in the key.

`ObjectInfo` reports `Key`, `Size`, `ContentType`, `Metadata`, `ModTime`, and
`ETag`. `ETag` identifies this version of the bytes where the driver has one,
and is empty otherwise.

### URLs

`URL(ctx, key, storage.PublicURL())` asks for a permanent public URL.
`URL(ctx, key, storage.SignedURL(15*time.Minute))` asks for one that stops
working after 15 minutes. A driver without a URL scheme returns
`ErrUnsupported`. `URL` does not check that the object exists, and it does not
authorize anyone: decide who may have the URL before asking for it.
Visibility and signed URLs are specified fully in STORAGE-5.

## Keys

A key is a `/`-separated path of one or more segments. The same rule
(`storage.ValidateKey`) applies on every driver, so a key that works in
development works in production:

- 1 to 1024 bytes of valid UTF-8;
- no leading or trailing `/`, and no empty segment (`a//b`);
- no `.` or `..` segment, so a key can never climb out of a prefix or, on the
  local driver, out of the storage root;
- no backslash and no control character.

Spaces, Unicode and dots inside a segment (`..hidden`, `name..ext`) are fine.

A key is an address that your server builds, such as `avatars/<user id>.png`
or `uploads/<random id>`. Never use a filename a client sent as the key. Keep
the filename as metadata instead.

## Errors

Drivers wrap failures in `*storage.Error` (the operation, the key, and the
cause). Classify them with `errors.Is` against the sentinels, which mean the
same thing on every driver:

| Sentinel | Meaning | `MapError` result |
| --- | --- | --- |
| `ErrNotFound` | No object under the key | `404 not_found` (your message) |
| `ErrInvalidKey` | The key breaks the key rules | `422 validation` |
| `ErrInvalidOptions` | A malformed content type or metadata, or a negative size or expiry | `422 validation` |
| `ErrSizeMismatch` | The bytes read differ from the declared `Size` | `422 validation` |
| `ErrUnavailable` | The backend is unreachable or failed transiently | `503 dependency_unavailable` |
| `ErrUnsupported` | The driver cannot do this, for example a URL | `500 internal` |
| anything else | | `500 internal` (your message) |

`storage.MapError(ctx, err, notFound, internal)` does that mapping for a
handler, in the D10 envelope with the request id. The driver's own error text
never reaches the client.

## Writing a driver

A driver implements `storage.Storage` and must pass the conformance suite in
`storage/storagetest`:

```go
func TestConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Storage {
		return newMyDriver(t) // a fresh, empty store for each check
	})
}
```

The suite checks every guarantee above:
- round trips;
- the default content type;
- overwrites and idempotent deletes;
- every invalid key, on every method;
- an 8 MiB object streamed through a pipe in small pieces, which is never
  held in memory by the test;
- a failed or canceled `Put` leaving the previous object whole;
- size mismatches and invalid options;
- concurrent `Put`s to one key;
- `URL` returning either a URL or `ErrUnsupported`.

The suite's own tests prove it catches a driver that breaks each rule.

Helpers for drivers:
- `ValidateKey`, `ValidatePutOptions` and `ValidateURLOptions` apply the shared
  rules.
- `ExpectSize` enforces a declared `Size` on a reader.
- `Wrap` builds the `*storage.Error`.

The `storage` package imports only the standard library and Gombit's
`contract` package (a test enforces this), so an application that uses it
never pulls in a driver's dependencies.
