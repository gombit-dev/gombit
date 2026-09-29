# Storage

Package `storage` is Gombit's object storage contract: one driver-neutral
interface for files such as uploads, avatars, reports and exports. Application
code depends on `storage.Storage`, never on a filesystem path or an S3 SDK
call, so moving from local development to S3-compatible storage in
production changes configuration, not code.

> **Status:** the contract and the local, in-memory, and S3-compatible
> drivers. Upload helpers, signed URLs, direct uploads, and admin fields
> follow
> ([epic #279](https://github.com/gombit-dev/gombit/issues/279)).

Every app has a store: `app.Storage()` returns the driver `GOMBIT_STORAGE_DRIVER`
names, which is local files under `./storage` by default. A runnable example
is in [`examples/storage`](../examples/storage/main.go). It serves downloads
with `Content-Disposition: attachment`, because an uploaded `text/html` or
`image/svg+xml` object rendered inline would run as a page in the app's
origin.

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
| **Atomic writes** | A `Put` that fails leaves the key as it was: the previous object whole, or still absent. That covers the reader returning an error, the context ending, and a length that differs from the declared `Size`. A reader during a `Put` reads the previous object whole, never part of the new one; for a key stored for the first time, it finds no object until the `Put` completes. Concurrent `Put`s to one key leave one of them whole. |
| **Overwrite** | `Put` replaces an existing object, including its content type and metadata. |
| **Idempotent delete** | Deleting a missing object is not an error, so a retried delete is safe. |
| **Portable keys** | Every method validates its key with the same rule (below) and fails with `ErrInvalidKey` before touching the backend. Every valid key is its own object on every driver: keys that differ only in case or in Unicode normalization (`café` NFC and NFD), a key that is a prefix of another (`a` and `a/b`), segments up to 255 bytes, and names Windows reserves (`CON`, `c:`) all store and read back as themselves. |
| **Context** | Every method honors `ctx`: one that has already ended fails the call with the context's error. A `Put` canceled mid-stream fails the same way and stores nothing. The reader `Open` returns stays bound to its `ctx` until it is closed: once the context ends, reading fails with its error and a read in progress is interrupted, the way an HTTP response body does (`storage.ContextReadCloser` gives other drivers that behavior). Keep the context alive while the stream is read. |
| **Owned metadata** | `Put` copies the metadata map it is given, and every `ObjectInfo` returned has a map of its own. Changing one changes neither the stored object nor any other result. |
| **Errors** | Every failure is a `*storage.Error` carrying the operation and the key, around a sentinel for `errors.Is` (see [Errors](#errors)). The outermost envelope always names the call that failed, even when the cause is another call's error (a `Put` source that failed reading another object). |
| **Versions** | A driver that reports an `ETag` reports the same one from `Put` and `Stat`, and a new one when different bytes replace the object. An empty `ETag` means the driver has none. |

### `PutOptions` and `ObjectInfo`

- **`ContentType`** is the object's media type. It must be a
  `type/subtype` media type; parameters are fine
  (`text/plain; charset=utf-8`). Empty stores
  `application/octet-stream`.
- **`Size`**, when set, is the exact length the reader will produce (zero
  for an empty object). `Put` then fails with `ErrSizeMismatch`, storing
  nothing, if the reader ends early or runs long. `nil` means unknown. Build
  it with `storage.KnownSize(n)`. For an HTTP body, use
  `storage.SizeFromContentLength(r.ContentLength)`: `-1` (unknown) becomes
  `nil`, and a request that claims `Content-Length: 0` can't store a
  non-empty body.
- **`Metadata`** is small user metadata returned in `ObjectInfo` exactly as
  given. Names are lower-case ASCII letters, digits and `-`. Values are UTF-8
  without control characters, without leading or trailing spaces (HTTP drops
  them from a header value, so S3 could not return them), and without `=?`,
  which starts an RFC 2047 encoded word. The limit is 2 KiB, measured as the headers of an S3 request:
  each header name (`x-amz-meta-` plus the name) and its value as sent. A
  printable-ASCII value counts as is. Any other value counts as its RFC 2047
  encoding (`mime.BEncoding`), S3's own for non-ASCII metadata
  (`storage.MetadataValueWireLen`). That is the strictest measure among S3
  services: MinIO counts exactly this, AWS the decoded UTF-8. Keep a
  client's original filename here, never in the key.

`ObjectInfo` reports `Key`, `Size`, `ContentType`, `Metadata`, `ModTime`, and
`ETag`. `Stat` and `Open` always report `ModTime`. The `ObjectInfo` that `Put`
returns may leave it zero where the backend doesn't say, as with S3's
`PutObject`, rather than cost a second request. `ETag` identifies this version
of the bytes where the driver has one, and is empty otherwise.

### URLs

`URL(ctx, key, storage.PublicURL())` asks for a permanent public URL.
`URL(ctx, key, storage.SignedURL(15*time.Minute))` asks for one that stops
working after 15 minutes. A signed URL needs a positive lifetime:
`SignedURL(0)` is refused with `ErrInvalidOptions`, never quietly turned into
a permanent URL. A driver without a URL scheme returns `ErrUnsupported`.

`URL` doesn't check that the object exists, since a URL for an upload names
a key that isn't stored yet, and the suite checks that. It also doesn't
authorize anyone: decide who may have the URL before asking for it.
Visibility and signed URLs are specified fully in STORAGE-5.

## Keys

A key is a `/`-separated path of one or more segments. The same rule
(`storage.ValidateKey`) applies on every driver, so a key that works in
development works in production:

- 1 to 1024 bytes of valid UTF-8, with no segment longer than 255 bytes.
  AWS allows longer segments, but MinIO, which stores objects as files,
  refuses them;
- no leading or trailing `/`, and no empty segment (`a//b`);
- no segment that is `.` or `..`, or that ends with `.` or a space. Windows
  strips a trailing dot or space from a path component, so `...` or `.. `
  would become `..` on a filesystem there, and `a.` would collide with `a`;
- no backslash, no control character (including the C1 range), and no
  Unicode line separator or bidirectional control. Those can split a log line
  or disguise a name in a listing, as in `invoice\u202efdp.exe`.

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
| `ErrInvalidKey` | The key breaks the key rules | `500 internal`: keys are built by the server, so an invalid one is a bug. Validate a key taken from a request with `ValidateKey` first, and answer 404. |
| `ErrInvalidOptions` | A malformed content type or metadata, or a negative size or expiry | `422 validation` |
| `ErrSizeMismatch` | The bytes read differ from the declared `Size` | `422 validation` |
| `ErrUnavailable` | The backend is unreachable or failed transiently | `503 dependency_unavailable` |
| `context.DeadlineExceeded` / `Canceled` | The request timed out, or the client went away | `503 dependency_unavailable` |
| `ErrUnsupported` | The driver cannot do this, for example a URL | `500 internal` |
| anything else | | `500 internal` (your message) |

`storage.MapError(ctx, err, notFound, internal)` does that mapping for a
handler, in the D10 envelope with the request id. The driver's own error text
never reaches the client.

## Drivers

| `GOMBIT_STORAGE_DRIVER` | Package | For |
| --- | --- | --- |
| `local` (default) | `storage/local` | Development and single-host deployments: files on disk |
| `memory` | `storage/memory` | Tests: objects in the process, nothing on disk |
| `s3` | `storage/s3` | Production: AWS S3, Cloudflare R2, MinIO, or any S3-compatible service |

`framework.New` opens the configured driver into `app.Storage()`.
`framework.WithStorage(s)` attaches one you opened yourself, for example a
`memory.New()` in a test. Either way, handlers use the same `storage.Storage`.

### Local

`GOMBIT_STORAGE_LOCAL_ROOT` (default `storage`, relative to the working
directory) is the root. It is resolved to an absolute path when the app starts
and created on the first write, so an app that never stores a file never grows
the directory. `gombit new` gitignores `/storage/`.

- **Layout.** A key is not a path. Each object is one file at
  `<root>/objects/<h[0:2]>/<h[2:4]>/<h>`, where `h` is the SHA-256 of the key.
  The file holds the object's bytes followed by a small trailer: a format
  version, the key, content type, metadata, modification time, and a SHA-256
  `ETag`. Readers ignore trailer fields they don't know, so a newer version
  can add fields and still share a root with an older one, as in a rolling
  deploy. Hashing is
  what keeps every key its own object on any filesystem. Case variants,
  `a` alongside `a/b`, long segments and names Windows reserves all just
  work, and no key can reach outside the root. The files are not meant to
  be browsed or edited by hand; go through the store.
- **Streaming, atomic, durable writes.** `Put` streams into a temporary file
  under `<root>/tmp` in 32 KiB pieces, never holding the object in memory.
  It then flushes the file, and flushes the entry of every directory on the
  path that this process hasn't yet seen flushed (the root and its new
  ancestors included). Only then does it rename the file into place and
  flush that directory. A reader sees the old object or the new one, never
  part of one. On Linux and macOS, a `Put` or `Delete` that returned
  survives a crash. If a directory flush before the rename fails, `Put`
  fails with the object still a temporary file, and the next `Put` flushes
  again. If the flush after the rename (or a delete's) fails even on a
  retry, the change has already happened: the call reports success, and
  `app.Storage()` logs a warning that it may not survive a crash. A failed
  `Put` removes its temporary file. A process killed mid-`Put` can't, so a
  store's first `Put` removes temporary files nothing has written to for an
  hour. `Open` reads from disk as you read.
- **Sharing.** Several processes can share one root (an app and its worker).
  Several *hosts* need a shared filesystem, or an S3-compatible store
  (STORAGE-3).
- **Windows.** An object that is open for reading cannot be replaced or
  deleted until its reader is closed. Windows offers no directory flush a
  process can request, so writes and deletes there are atomic, but whether
  they survive a crash is up to the filesystem.
- **URLs.** `URL` returns `ErrUnsupported` for now. Serving local files needs
  the public/private rules of STORAGE-5; serving every object would expose
  private ones.

In production, point `GOMBIT_STORAGE_LOCAL_ROOT` at a persistent volume. A
container's working directory is lost when the container is replaced. A
production app whose root is relative to the working directory logs a warning
on its first write. An app that never stores a file is never warned.

### S3

```bash
GOMBIT_STORAGE_DRIVER=s3
GOMBIT_STORAGE_S3_BUCKET=my-app-uploads
GOMBIT_STORAGE_S3_REGION=us-east-1            # "auto" for Cloudflare R2
GOMBIT_STORAGE_S3_ENDPOINT=                   # empty for AWS; https://<account>.r2.cloudflarestorage.com, http://127.0.0.1:9000 (MinIO)
GOMBIT_STORAGE_S3_PREFIX=                     # optional, for example "myapp/prod/", to share a bucket
GOMBIT_STORAGE_S3_ACCESS_KEY_ID=              # both empty: the AWS default credential chain
GOMBIT_STORAGE_S3_SECRET_ACCESS_KEY=
GOMBIT_STORAGE_S3_FORCE_PATH_STYLE=false      # true for MinIO and most S3-compatible services
```

- **Credentials.** Leave both keys empty to use the AWS default credential
  chain: the `AWS_*` environment variables, shared config, or an IAM role on
  ECS, Fargate or EC2. The secret key is redacted from `gombit config show`
  and never appears in errors.
- **Streaming.** `Put` reads the object into one buffer of at most 8 MiB. An
  object that fits is a single `PutObject`, and the buffer is sized to it
  when the size is declared: a 10 KiB upload allocates about its own size.
  A larger object is a multipart upload of 8 MiB parts sent one after
  another, so a `Put` holds at most 8 MiB whatever the size. The largest
  object is 10,000 parts, about 78 GiB. `Open` streams the response body.
- **Atomic writes.** An object appears only when its upload completes. A
  failed or canceled `Put` aborts its multipart upload, on a fresh context,
  so no incomplete upload lingers to be billed.
- **Permissions.** The credentials need `s3:GetObject`, `s3:PutObject`,
  `s3:DeleteObject` and `s3:AbortMultipartUpload` on the objects
  (`arn:aws:s3:::BUCKET/PREFIX*`), and `s3:ListBucket` on the bucket.
  Without `ListBucket`, S3 answers a request for a missing object with
  `403 Access Denied` instead of 404, and a missing object would become a 500
  rather than a 404.
- **Prefix.** `GOMBIT_STORAGE_S3_PREFIX` must end with `/` (`myapp/prod/`) and
  follow the key rules. It is put before every key.
- **Metadata** values that aren't printable ASCII (or that contain `=?`) are
  sent RFC 2047 encoded, which is S3's own encoding for non-ASCII metadata,
  and decoded on the way back, so they round-trip exactly. The contract's
  size limit counts the encoded headers, which is exactly how MinIO measures
  them.
- **Errors.** A missing object is `ErrNotFound`. Throttling, a 5xx or a
  network failure is `ErrUnavailable`, so `MapError` answers 503. A missing
  bucket or denied credentials is a plain error, so `MapError` answers 500
  rather than a misleading 404. A `HEAD` that returns 404 checks the bucket (at most
  once a minute), because a `HEAD` response can't tell a missing object from a
  missing bucket.
- `ModTime` comes from S3 on `Stat` and `Open`. The `ObjectInfo` that `Put`
  returns leaves it zero, since S3 doesn't return one.
- **Checksums** are sent only where S3 requires them, because several
  S3-compatible services reject the SDK's newer default checksums.
- **`URL`** returns `ErrUnsupported` until STORAGE-5 adds public and signed
  URLs.

The driver runs the full conformance suite against MinIO in CI (the
`Storage (s3)` job):

```bash
go test -tags integration ./storage/s3 -s3.endpoint http://127.0.0.1:9000 \
  -s3.access-key minioadmin -s3.secret-key minioadmin
```

### Memory

`memory.New()` keeps objects in a map, deterministically, with no disk. It
keeps the whole contract, but it holds each object in memory. `Keys()` lists
what was stored, for assertions. `WithClock` fixes `ModTime`, in both
drivers.

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
- every failure wrapped in a `*storage.Error` with its operation and key;
- metadata owned by each result, and `Open`'s reader following its context;
- ETags that change with the bytes;
- portable keys (case and Unicode-normalization variants, prefix keys,
  segments up to 255 bytes, reserved names);
- a reader during a `Put` sees the previous object, or none for a first write;
- every method on an already-canceled context;
- an 8 MiB object streamed through a pipe in small pieces, which is never
  held in memory by the test;
- a failed, canceled, mismatched or refused `Put` leaving the previous
  object whole;
- size mismatches and invalid options;
- concurrent `Put`s to one key;
- `URL` returning either a URL or `ErrUnsupported`.

The suite's own tests prove that every check fails for a driver broken the
way it guards against. A new check can't land without such a driver.

Helpers for drivers:
- `ValidateKey`, `ValidatePutOptions` and `ValidateURLOptions` apply the shared
  rules.
- A backend that carries only ASCII metadata (S3 headers) must send a value
  that isn't printable ASCII as `mime.BEncoding.Encode("UTF-8", value)`,
  exactly what `storage.MetadataValueWireLen` measures.
- A filesystem driver cannot just join the key to a directory. That would
  conflate case variants, and it can't hold both `a` and `a/b`. Map keys to
  paths in a way that keeps every key distinct.
- `ExpectSize` enforces a declared `Size` on a reader. It reports excess on
  the read that reaches the size, but read it to EOF (`io.Copy`), because
  `io.ReadFull` and `io.CopyN` drop an error returned with the last bytes.
- `Wrap` builds the `*storage.Error`: it keeps an error that is already this
  call's envelope and wraps any other, including another call's.
- `ContextReadCloser` binds an `Open` reader to its context (keeping `Seek`
  when the underlying reader has it).

The `storage` package imports only the standard library and Gombit's
`contract` package (a test enforces this), so an application that uses it
never pulls in a driver's dependencies.
