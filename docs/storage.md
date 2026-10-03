# Storage

Package `storage` is Gombit's object storage contract: one driver-neutral
interface for files such as uploads, avatars, reports and exports. Application
code depends on `storage.Storage`, never on a filesystem path or an S3 SDK
call, so moving from local development to S3-compatible storage in
production changes configuration, not code.

> **Status:** the contract, the local, in-memory, and S3-compatible
> drivers, the upload helpers, public and signed URLs, direct uploads, and
> metadata and cleanup semantics. Admin fields follow
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
| **Atomic writes** | A `Put` that fails leaves the key as it was: the previous object whole, or still absent. That covers the reader returning an error, the context ending, and a length that differs from the declared `Size`. A reader during a `Put` reads the previous object whole, never part of the new one; for a key stored for the first time, it finds no object until the `Put` completes. Concurrent `Put`s to one key leave one of them whole. The one exception is a failure that matches `ErrUnknownOutcome`: a remote backend (S3) was sent the object and never said whether it stored it, because the answer was lost, the connection dropped, or the service failed. The key then holds the previous object or the new one, whole; which is unknown. `Stat` the key, or `Put` again (repeating a `Put` is safe). The local and memory drivers never return it. |
| **Overwrite** | `Put` replaces an existing object, including its content type and metadata. With `PutOptions{IfAbsent: true}` it stores only where nothing is stored, and otherwise fails with `ErrExists` and leaves the object. The check and the store are one atomic step on every driver, against other processes sharing the backend too: the local driver publishes with a rename that refuses to replace, and S3 with a conditional write (`If-None-Match: *`). Of two `IfAbsent` `Put`s to one empty key, exactly one succeeds. |
| **Idempotent delete** | Deleting a missing object is not an error, so a retried delete is safe. |
| **Portable keys** | Every method validates its key with the same rule (below) and fails with `ErrInvalidKey` before touching the backend. Every valid key is its own object on every driver: keys that differ only in case or in Unicode normalization (`café` NFC and NFD), a key that is a prefix of another (`a` and `a/b`), segments up to 255 bytes, and names Windows reserves (`CON`, `c:`) all store and read back as themselves. |
| **Context** | Every method honors `ctx`: one that has already ended fails the call with the context's error. A `Put` canceled mid-stream fails the same way and stores nothing. `Put` observes the context between reads of its source: it cannot interrupt a source `Read` already blocked, since an `io.Reader` has no way to be interrupted. The source's owner makes it return, as an HTTP server closes a request body when the client goes away. Once the context has ended, the `Put` fails even if the source then reaches EOF. The reader `Open` returns stays bound to its `ctx` until it is closed: once the context ends, reading fails with its error and a read blocked in the backend returns, the way an HTTP response body does. `storage.ContextReadCloser` gives other drivers that behavior over a reader whose `Close` interrupts a `Read` (a file, an HTTP body, a pipe). Keep the context alive while the stream is read. |
| **Owned metadata** | `Put` copies the metadata map it is given, and every `ObjectInfo` returned has a map of its own. Changing one changes neither the stored object nor any other result. |
| **Errors** | Every failure is a `*storage.Error` carrying the operation and the key, around a sentinel for `errors.Is` (see [Errors](#errors)). The outermost envelope always names the call that failed, even when the cause is another call's error (a `Put` source that failed reading another object). |
| **Versions** | ETags are one capability: a driver reports the same `ETag` from `Put`, `Open`, and `Stat` for one version of an object, or none from any of them. A driver that reports them gives different bytes a different `ETag`. |

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

### Visibility and URLs

An object is **public** when its key starts with the store's public prefix,
`GOMBIT_STORAGE_PUBLIC_PREFIX` (`public/` by default; empty makes nothing
public). Every other object is **private**. Visibility follows the key rather
than a flag stored with the object, because a key prefix is what a bucket
policy or a CDN can serve. To make an object public, store it under the
prefix: `public/avatars/<id>.png`.

```go
// A public object: a permanent URL, for <img src>, a CDN, or a feed.
u, err := app.Storage().URL(ctx, "public/logos/acme.png", storage.PublicURL())

// A private object: a URL that stops working within 15 minutes (15 minutes
// after the start of the second it is signed in), so the
// browser downloads straight from storage without the app proxying the bytes.
if !canRead(user, doc) { // authorization stays in the application
	return contract.Authorization("")
}
u, err := app.Storage().URL(ctx, doc.FileKey, storage.SignedURL(15*time.Minute))
```

- `PublicURL()` for a private key fails with `ErrNotPublic`, never a URL.
- `SignedURL(ttl)` works for any key, public or private. `ttl` must be
  positive and at most `storage.MaxURLExpiry` (7 days, S3's limit): anything
  else is `ErrInvalidOptions`, so a zero or negative lifetime never becomes a
  permanent URL. The lifetime is a whole number of seconds (`SignedURL(time.Millisecond)`
  is `ErrInvalidOptions`), and every driver counts it the way S3 does: from
  the start of the second the URL is signed in, since SigV4's `X-Amz-Date` has
  whole-second precision. A URL from `SignedURL(ttl)` is valid for more than
  `ttl` minus a second and at most `ttl` after `URL` returns, on every driver.
  A signed URL expires even when its key is public. Past its
  expiry, or with an altered signature, it is refused. The same key without
  the query still reads the object, because the key itself is public: a
  signed URL doesn't make a public object private.
- `URL` doesn't check that the object exists (a URL may name a key that isn't
  stored yet), and it doesn't authorize anyone. Decide who may have the URL
  before asking for it. Anyone holding a signed URL can use it until it
  expires.

Per driver:

| Driver | Public URL | Signed URL |
| --- | --- | --- |
| `s3` | `GOMBIT_STORAGE_S3_PUBLIC_URL` + `/` + the object key. Configure the bucket to serve the public prefix (below). Without a public URL: `ErrUnsupported`. | A presigned `GetObject` (SigV4 query parameters). It stops working at its expiry, or earlier if the credentials that signed it are revoked or expire (temporary IAM-role credentials last hours). |
| `local`, `memory` | `GOMBIT_STORAGE_LOCAL_URL` (`/_storage`) + `/` + the key, served by the app. | The same URL plus `expires` and an HMAC-SHA256 `signature`, checked by the app. |

**S3.** Let anyone read the public prefix with a bucket policy, and point
`GOMBIT_STORAGE_S3_PUBLIC_URL` at the bucket or at a CDN in front of it. The
URL stands for the bucket's root, and `GOMBIT_STORAGE_S3_PREFIX` is part of
the object key:

```json
{"Version": "2012-10-17", "Statement": [{
  "Effect": "Allow", "Principal": "*", "Action": "s3:GetObject",
  "Resource": "arn:aws:s3:::my-bucket/myapp/public/*"
}]}
```

Private objects need nothing: they stay unreadable without credentials, and
a signed URL carries them.

**Local and memory.** `framework.New` mounts `GET` and `HEAD` at
`GOMBIT_STORAGE_LOCAL_URL`. That is a plain path (`/_storage`, without
percent-escapes), or an absolute http(s) URL with one, when the files are
served from another address. `gombit config` and the driver check it with the
same rule. Its behavior:

- A public key is served to anyone without a signature. A request that does
  carry one (an `expires` or `signature` parameter) is checked like any signed
  URL, public key or not.
- A private key needs a signed URL that hasn't expired. Anything else is a
  D10 `403`: a missing, wrong or altered signature, or another key.
- A missing object is `404`.
- Objects are served inline with their stored type, plus
  `X-Content-Type-Options: nosniff` and `Content-Security-Policy: sandbox`.
  An uploaded HTML or SVG file therefore cannot run script in the app's
  origin.
- Byte ranges and conditional requests (`ETag`, `If-None-Match`) work as
  they do on S3, so media seeks and downloads resume.
- The path must be the route's own: one that overlaps the API prefix,
  `/admin`, the health or metrics routes, the docs, or the OpenAPI documents
  fails `framework.New`.

URLs are signed with `GOMBIT_STORAGE_URL_SECRET` (at least 32 bytes). When
that is unset, the key is derived from `GOMBIT_JWT_SECRET`. With neither
secret, or with `GOMBIT_STORAGE_LOCAL_URL` empty, `URL` returns
`ErrUnsupported`. A signature also covers the app's name and environment and
the URL's path, so staging and production never accept each other's URLs,
even with the same JWT secret. Changing the secret, the name, or the
environment invalidates every URL signed before.
An app that passes its own store with `framework.WithStorage` builds a
`presign.Signer`, passes it to `local.WithURLs` or `memory.WithURLs`, and
mounts `presign.Handler` itself.

## Metadata and lifecycle

### What an object says about itself

`Stat` (and `Open`) return an `ObjectInfo`:

| Field | Meaning |
| --- | --- |
| `Key` | The object's key. |
| `Size` | Its length in bytes. |
| `ContentType` | Its media type (for an upload, the detected or confirmed type). |
| `ETag` | An opaque version identifier, which changes when the bytes change. Compare ETags; never compute or interpret one. On S3 its form depends on how the object was uploaded and on the bucket's encryption (SSE-KMS, say), so it is not a checksum of the bytes. |
| `ModTime` | When it was last stored. For an upload under a generated key, which is written once, that is when it was created. |
| `Metadata`, `StoredFilename()` | The user metadata. `StoredFilename()` is the client's cleaned filename (the `filename` metadata) that `storage/upload` stores; an `upload.File` has it as its `Filename` field. |

### Cleanup

A file and the database record that refers to it are two writes in two
systems, with no shared transaction. Neither "the insert returned an error"
nor "no record refers to it right now" proves a file is unreferenced: an
insert can commit and lose its answer (a dropped connection), and a record
can be written a moment after the check. Deleting on either evidence can
delete the file of a live record.

`storage/claims` closes that gap by making the database the authority on
who owns a file. It keeps a table, `storage_claims`, with one row per key
under the protocol, in one of three states:

| State | Meaning |
| --- | --- |
| `pending` | Stored (or about to be), and no record refers to it yet. |
| `held` | A record refers to it. A held file is never deleted. |
| `deleting` | Being deleted. No record can take it. |

Every transition is one conditional `UPDATE`, so two that race on a key
(a record taking it and a sweep abandoning it, say) cannot both win, in
any number of processes. Storage drivers know nothing of claims:
`storage.Storage` stays objects only.

**Setup.** Add the table to the application's migrations
(`claims.Models()`; apps made by `gombit new` have it), build a `Claims`
over the database and the store, and give it to upload policies:

```go
cl := claims.New(db, app.Storage(), claims.WithWarn(logWarning))
avatars.Claims = cl // upload.Policy: each generated key is claimed first
```

`upload.Receive` and `upload.Authorize` then claim each key (`pending`)
before anything is stored under it. A key that cannot be claimed fails the
upload, and nothing is stored. A claim holds keys of at most
`claims.MaxKeyLen` (512) bytes, so keep `Policy.Prefix` under 480. Under
`Policy.Claims`, a file that fails (an invalid request, a refused
`Confirm`) is deleted through the protocol: only while its key is
`pending`. A refused confirmation of a key a record holds, or of a key
never claimed, deletes nothing.

**Recording an upload.** Write the record with `CreateWith`. It runs your
insert in a transaction that also moves the key from `pending` to `held`,
so the record and the hold commit together or not at all:

```go
f, err := upload.Receive(store, r, avatars)
// ...
err = cl.CreateWith(ctx, []string{f.Key}, func(tx *gorm.DB) error {
	return tx.Create(&Avatar{UserID: user.ID, FileKey: f.Key}).Error
})
```

When the transaction fails, `CreateWith` abandons the upload: it moves the
key from `pending` to `deleting`, deletes the file, then the claim. If the
transaction in fact committed although it returned an error, or another
request already recorded the key (a retried confirmation), the key is
`held`. Abandoning it then does nothing, and the file is kept. A key that
is not `pending` fails the transaction with `claims.ErrNotPending`.

A record with several files passes all their keys; empty keys (an
optional file left out) are skipped.

**Replacing a file.** Use `Update(ctx, hold, release, fn)`. Your change,
the new keys' moves to `held`, and the old keys' moves to `deleting`
commit in one transaction. If it fails, the new uploads are abandoned and
the old files kept; once it commits, the old files are deleted. A key in
both lists (a file kept) is left alone.

**Deleting a record.** Use `DeleteWith`. Your delete and the keys' moves
from `held` to `deleting` commit in one transaction, and then the files are
deleted. If deleting the file fails, the record is still gone: the warning
hook is told, and the next sweep finishes the delete. A key with no claim
(a file stored before the application adopted claims, or no file at all)
is outside the protocol: the record is deleted and the file is left alone.
A claim that is `pending` or `deleting` is not the record's to release:
`DeleteWith` fails with `claims.ErrNotHeld` and the record is kept.

**Abandoned uploads.** Some uploads are never recorded: a direct upload
granted but never confirmed, or a crash between storing a file and writing
its record. Their claims stay `pending`. Run `cl.Sweep(ctx, grace)`
periodically, as a job. It abandons every `pending` claim older than
`grace` and finishes every `deleting` one. Set `grace` longer than an
upload can take to be recorded: more than `Policy.GrantExpiry` plus the
confirmation, for direct uploads (the example uses twice
`upload.DefaultGrantExpiry`).

**Uploads still writing.** A claim comes before its file, so a sweep can
abandon the claim of an upload that is still streaming, and the upload can
then publish its file. To keep that file from becoming an orphan no sweep
can find, every claim has a **lease**: the time after which no writer the
application controls publishes under its key any more.

- `upload.Save` and `upload.Receive` lease the key for
  `Policy.UploadTimeout` (`upload.DefaultUploadTimeout`, an hour, when zero)
  and run the `Put` under that deadline. A body read already in progress is
  not interrupted, but the drivers check the deadline just before they
  publish, so a `Put` still running then never stores anything.
- A `deleting` claim (abandoned, swept, or released by a record's delete)
  stays as a **tombstone** until its lease, plus `claims.LeaseMargin` (5
  minutes, for clocks that disagree and a publish already sent), has ended.
  Each sweep deletes its file again, so a file published late is still
  deleted, and no record can hold the key. Only then is the row removed.

**Direct uploads are staged.** The application cannot end a direct upload:
S3 checks a presigned URL's expiry when the `PUT` starts, and a `PUT`
started in time can publish whenever it ends. So under claims a client
never writes a claimed key:

1. `upload.Authorize` claims the key as staged (`claims.Stage`) and grants
   a `PUT` to its staging key, `upload.StagingKey(key)`: `_staging/` and the
   key. The grant's `Key` is still the key, which the client submits.
2. `upload.Confirm` moves the claim to `promoting` (`claims.Promote`, which
   one confirmation wins), checks the staged object, publishes a copy of it
   at the key, and deletes the staged copy. Only then can a record hold the
   key; a staged claim that was never promoted cannot be held.
3. A refused staged object is deleted and the claim goes back to `pending`,
   so the grant can upload again.

**Promotion is fenced.** On S3 the copy to the key is a remote request
too: once S3 has accepted it, no deadline on the application's side proves
it will not complete later. So it is published in two steps
(`storage.Publisher`):

1. `storage.PreparePublish` creates a multipart upload of the key and
   copies the staged object into its parts server-side (`UploadPartCopy`,
   in ranges for objects over 5 GiB, at most 1,100 parts). This publishes
   nothing. The token records each part's ETag as S3 returned it: the
   manifest the upload is completed with (AWS forbids completing from a
   listing). ETags are opaque, so the token's size is checked as each one
   arrives: preparing fails as soon as the next would not fit
   (`storage.MaxPublicationToken`).
2. The copy's token is recorded on the claim (`claims.Publishing`).
3. `storage.Publish` completes the upload with that manifest.

If preparing fails with something left to abort (a part request that went
unanswered, or an abort that failed), it returns a cleanup token with the
error: the key and upload ID only, checked to fit before any part is
copied. The claim keeps it, `promoting`, to be fenced.

If the answer is lost, the claim stays `promoting` with its token: the
copy may exist, and a retried `Confirm` checks the key as it is. No claim
with a recorded copy is ever forgotten until `storage.Fence` has proven the
copy can no longer publish. On S3 it aborts the multipart upload, which S3
orders against completing it: a successful abort means it never completes,
and `NoSuchUpload` means it is over. That, and only that, is what a
successful fence proves: the copy can never be published, so it can never
become an object without a claim. The fence needs only
`s3:AbortMultipartUpload`. As AWS advises, it then lists the upload's parts
and aborts again while any are listed, and every sweep fences again while
the claim's tombstone lasts. That part only frees storage: it uses the
optional `s3:ListMultipartUploadParts`, and a failure there is reported
through the driver's warning hook (`s3.Config.Warn`, the app's logger) and
never fails the fence. A listing is a snapshot,
though: a part still being processed can land after the last fence. Such a
part is never an object (it cannot be read or published), only storage
billed to an aborted upload; a bucket lifecycle rule aborting incomplete
multipart uploads after a day reclaims it, and is recommended on S3. A
fence that fails keeps the claim for the next sweep. A sweep abandons a `promoting` claim only once the
promotion's lease (`Policy.UploadTimeout`) has ended. With the local and
memory drivers the copy runs in the application process and publishes
nothing after its call returns.

**Which stores can be owned.** The protocol needs proof about every write
it lets near a claimed key, so a store must be one of:

- a `storage.Publisher` (S3): its copies can be fenced;
- a `storage.BoundedWriter` (local, memory): a `Put` publishes, if at all,
  before it returns, and never once its context has ended.

Any other store, a third-party remote driver for one, is refused:
`claims.Pending` and `claims.Stage`, `upload.Save`/`Receive`/`Authorize`
under claims, and `storage.PreparePublish`/`Publish`/`Fence` fail with
`storage.ErrUnsupported` (`storage.CheckOwnable`). A write such a store sent
could complete after its claim was forgotten, and nothing could prove
otherwise.

On a remote store (a `storage.Publisher`: S3), `upload.Save` and
`upload.Receive` under claims stage too: they put the file at the staging
key and promote it the same way, since an unanswered `PutObject` cannot be
fenced. There, only a fenced copy ever writes a claimed key.

**The staging namespace is reserved.** `_staging/` at the root of the
store belongs to `storage/upload` and `storage/claims`. A late `PUT` can
only ever publish a staging object, which nothing refers to. A `deleting`
staged claim deletes its staging object too, and `cl.SweepStaging(ctx)`
deletes every object under `_staging/` whose key has no live claim (or is
held: a leftover after promotion), **whoever stored it**. Never store other
objects there; `upload.Policy` refuses prefixes under it.

Run `SweepStaging` periodically where clients upload straight to the
backend (S3); it lists `_staging/`. With the local and memory drivers the
app's own route aborts a signed `PUT` at `storage.SignedUploadTimeout` (an
hour) after its URL expired, within the staged claim's lease, so `Sweep`
alone suffices. On S3, a bucket lifecycle rule expiring `_staging/` after a
day, and aborting incomplete multipart uploads after a day, is a useful
backstop, but it is not part of the ownership protocol: lifecycle expiry
is coarse and asynchronous. (It is what reclaims a part that lands after
its upload was fenced, which is never an object.)

**Ownership.** `Sweep` reads claims, never the store. Outside `_staging/`,
a file without a claim is outside the protocol: nothing in `storage/claims`
ever deletes it. That covers a file from a shared library, a key the
application chose, and files stored before the application adopted claims.
Inside `_staging/`, the reserved namespace above, the protocol owns
everything. A claim is one key held by one record: if two records can refer
to one file (a copied reference), keep that file out of the protocol and
delete it yourself when the last reference goes.

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
the filename as metadata instead. [`storage/upload`](#uploads) does both.

## Uploads

Package `storage/upload` receives a file from an HTTP request into a store,
with safe defaults:

```go
var avatars = upload.Policy{
	MaxBytes: 5 << 20,                                // required
	Types:    []string{"image/png", "image/jpeg"},    // required; "*/*" for any
	Prefix:   "avatars/",
}

r.POST("/avatars", func(c *gin.Context) {
	f, err := upload.Receive(app.Storage(), c.Request, avatars)
	if err != nil {
		fail(c, upload.MapError(c.Request.Context(), err))
		return
	}
	// f.Key is "avatars/<32 hex digits>"; f.Filename is the client's name.
})
```

- `upload.Receive` reads a `multipart/form-data` request: the part in
  `Policy.Field` (`file` by default) that has a filename. An empty file
  input sends none, which is `ErrNoFile`. Other fields are read and
  discarded, and a second file is refused.
- `upload.ReceiveBody` stores a request whose body is the file (`PUT` or
  `POST` with the bytes). The filename, if any, comes from the request's
  `Content-Disposition` header.
- `upload.Save(ctx, store, reader, filename, policy)` applies the same
  policy to any reader.

What the policy guarantees:

- **Size.** The file streams into the store and is never buffered whole. A
  declared length over `MaxBytes` is refused before a byte is read.
  Otherwise the upload fails at the byte past the limit. A multipart request
  may carry at most `MaxFormBytes` (64 KiB) beyond the file, for part
  headers, boundaries and other fields, and that is a bound of its own: a
  file smaller than `MaxBytes` doesn't make room for more. Other fields fail
  with `ErrTooLarge` as soon as they pass it, and the whole request is
  checked when its body ends: bytes after the closing boundary (a MIME
  epilogue) count too. The framework's default body limit covers JSON
  only, so this is the bound for uploads.
- **Type.** The media type is detected from the file's first 512 bytes
  (`http.DetectContentType`) and checked against `Types` (`image/png`,
  `image/*`, `*/*`). Each entry must be a lowercase media type without
  parameters, as `mime.ParseMediaType` reads it, or one of those two
  wildcards; anything else is refused with the policy (a 500, the server's
  mistake) rather than rejecting every upload. The client's `Content-Type` and the filename's extension
  are ignored, and the detected type is what is stored. The default detector
  never reports `image/svg+xml` or `application/json`. For those, or other
  types it does not recognize, set `Policy.Detect`. A detector gets a copy of
  the file's first bytes, its own to keep or change; nothing it does to it
  reaches the stored file.
- **Key.** Generated by the server: `Prefix` plus 128 random bits (32 hex
  characters). The policy checks that shape, so a `Prefix` too long to leave
  room for the id within the 1024-byte key limit is refused up front. The
  client's filename is cleaned (`upload.CleanFilename`) and kept only as
  the `filename` metadata. Cleaning keeps the last path element and drops
  control and bidirectional characters. The result is for display and
  `Content-Disposition`, never a path.
- **Cleanup.** A refused file is never written. A failed `Put` stores
  nothing, including the local driver's temporary file and S3's multipart
  upload (the storage contract). A `Put` whose outcome is unknown
  (`ErrUnknownOutcome`: S3 may have stored it) has its generated key
  deleted, since nothing else will ever use that key. The error then
  reports the failure as `ErrUnavailable` (or the context's error), since
  nothing is stored. A file stored before the rest of the request turned
  out invalid is deleted, even if the client went away, and so is one whose
  request body then failed, even in the read that delivered the form's
  closing boundary (a body cut short). When one of those
  deletes fails, the error carries an `*upload.CleanupError` naming the key
  that may still hold a file.

`upload.MapError` maps the errors for a handler:

| Error | Response |
| --- | --- |
| `ErrTooLarge` (or an `http.MaxBytesError`) | `413 payload_too_large` |
| `ErrType`, `ErrNoFile`, `ErrMalformed` | `422 validation` |
| An invalid `Policy` | `500 internal` (the server's mistake) |
| The request's context ended (the client went away, or it timed out), at any point of the request: during the file, a later field, or after the closing boundary | `503 dependency_unavailable`, through `storage.MapError` |
| Anything else | `storage.MapError` |

Serve an upload as an attachment, with the stored filename:
`mime.FormatMediaType("attachment", map[string]string{"filename": name})`.
[`examples/storage`](../examples/storage/main.go) has `POST /uploads` and
`GET /uploads/:id`.

### Direct uploads

A client can upload a file on its own with a short-lived signed grant, and
the app checks the result. On S3 the grant is a presigned `PUT` to the bucket,
so a large file never passes through the application. With the local and
memory drivers it is a `PUT` to the app's storage route (`/_storage`), which
streams the bytes into the store: they pass through the app process, but not
through your handlers or the framework's request parsing.

```go
// 1. The client declares the file; the app decides it may upload, and grants.
g, err := upload.Authorize(ctx, app.Storage(), avatars, in.Size, in.ContentType, in.Filename)
// Remember g.Key with the user who asked; send g.Request to the client.

// 2. The client sends the bytes: g.Request.Method to g.Request.URL, with the
//    headers in g.Request.Header and the file as the body.

// 3. The client says it is done; the app checks what arrived.
f, err := upload.Confirm(ctx, app.Storage(), key, avatars)
```

What a grant enforces:

- **Scope.** A grant is one signed `PUT`:
  - to one generated key (`Prefix` plus a random id);
  - of exactly the declared size (at most `MaxBytes`, else `ErrTooLarge`
    before any grant);
  - with the declared `Content-Type` (one `Types` accepts) and the metadata
    (the cleaned filename).

  The backend refuses a request that differs in any of these with `403`:
  another length, type, metadata or key, or a chunked body. S3 refuses it
  because they are in the SigV4 signature; the local and memory drivers
  because `presign.Handler` checks them.
- **Lifetime and single use.** `Policy.GrantExpiry` is 15 minutes by
  default. A grant stores only where nothing is stored yet. S3 enforces this
  with a signed `If-None-Match: *` (412 otherwise); the app's route stores
  with `PutOptions{IfAbsent: true}` and answers a D10 409. Either way the check
  and the store are one atomic step, so of two clients using one grant at the
  same instant, through any number of app processes, exactly one stores. Once
  the file has been uploaded and confirmed, the grant cannot replace it with
  other bytes. If `Confirm` refuses a file, it deletes it, and an unexpired
  grant can then upload again. A `PUT` may start until the grant expires;
  the app's route aborts one still running `storage.SignedUploadTimeout`
  later (S3 cannot: under claims, uploads are staged, see
  [Cleanup](#cleanup)). An S3-compatible service must support
  conditional writes (`If-None-Match` on `PutObject` and
  `CompleteMultipartUpload`); AWS S3 and MinIO do.
- **Headers the grant does not sign.** SigV4 authenticates only the headers
  a grant signs (length, type, metadata, `If-None-Match`). S3 also keeps five
  standard headers a client can add unsigned: `Cache-Control`,
  `Content-Disposition`, `Content-Encoding`, `Content-Language` and `Expires`.
  It serves them back with the object. A grant never sets them, so on S3
  `Confirm` refuses (and deletes) an upload that carries any of them
  (`storage.UploadVerifier`): a confirmed object has only what its grant
  described. The local and memory drivers keep none of those headers anyway.
- **Size.** On S3 a direct upload is one presigned `PutObject`, so it is at
  most 5 GiB (`s3.MaxUploadURLBytes`, S3's single-`PUT` limit); a larger
  `Size` is `ErrInvalidOptions` rather than a grant that could never succeed.
  (`Put` has no such limit: it uploads larger objects in parts.)
- **Expiry and credentials.** A grant signed with temporary credentials (an
  IAM role's, say) stops working when they expire, whatever its own lifetime.
  The `Expires` it reports is the earlier of the two.
- **Confirmation.** The declared type is only a claim, and it is what the
  store will serve the object as. `Confirm` reads the object's first bytes.
  It fails, and deletes the object, in any of these cases:
  - the detected type isn't accepted by the policy (`ErrType`);
  - the detected type isn't the declared one: a "PNG" whose bytes are HTML,
    or a JPEG declared as PNG (`ErrType`);
  - the object is over `MaxBytes` (`ErrTooLarge`).

  Under `Policy.Claims`, it deletes the object only while its key is
  `pending` (see [Cleanup](#cleanup)). Nothing uploaded is `ErrNoFile`. Confirm only keys you granted, to the
  user who asked. `Confirm` checks the prefix but not ownership.
- **Stores.** Direct uploads are an optional interface,
  `storage.DirectUploader`. The local, memory and S3 drivers implement it.
  `storage.UploadURL` and `upload.Authorize` return `ErrUnsupported` for a
  store that doesn't; use `Receive` for those.

A grant that is never used expires. An upload that is never confirmed stays
stored, with a `pending` claim, until the claims sweep removes it (see
[Cleanup](#cleanup)).

**Browsers and S3.** A browser's `PUT` to the bucket is cross-origin, so the
bucket needs a CORS rule allowing it from the app's origin, with every header
the grant asks for that a browser doesn't send freely: `Content-Type` (with a
type other than a form's), `If-None-Match` (the single-use condition), and the
`x-amz-meta-*` metadata. A browser checks them in a preflight before the `PUT`,
and S3 refuses the preflight if one is missing:

```json
[{
  "AllowedOrigins": ["https://app.example.com"],
  "AllowedMethods": ["PUT"],
  "AllowedHeaders": ["content-type", "if-none-match", "x-amz-meta-*"],
  "ExposeHeaders": ["ETag"],
  "MaxAgeSeconds": 3600
}]
```

With the local and memory drivers the grant is a `PUT` to the app's own
`GOMBIT_STORAGE_LOCAL_URL`. The storage route `framework.New` mounts there is
exempt from cookie-mode CSRF (the signed URL is the authorization, and no
cookie is involved, as with S3), the JSON body limit (the signed length bounds
it) and input sanitization (the file is stored byte for byte). The exemption
covers that mounted route and nothing else. With `WithStorage` the framework
mounts no route, so a route your app registers under that path keeps every
protection.
[`examples/storage`](../examples/storage/main.go) has `POST /uploads/direct`
and `POST /uploads/direct/:id/confirm`.

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
| `ErrUnknownOutcome` | A `Put` sent the object to a remote backend and never learned whether it was stored; the key holds the old object or the new one | `503 dependency_unavailable` |
| `ErrExists` | A `Put` with `IfAbsent` found an object already stored under the key | `409 conflict` |
| `context.DeadlineExceeded` / `Canceled` | The request timed out, or the client went away | `503 dependency_unavailable` |
| `ErrUnsupported` | The driver cannot do this, for example a URL | `500 internal` |
| `ErrNotPublic` | A public URL was asked for a private object | `500 internal`: the server chose the key and the kind of URL |
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
  It then flushes the file, and makes every directory entry on the
  object's path durable. Only then does it rename the file into place and
  flush that directory. A directory that exists is not taken to be durable,
  since another process sharing the root may have created it and not
  flushed it yet. Each store flushes the entry of every directory under the
  root on the path once, whoever created it. The root itself is created in
  a parent directory that must already exist (the store never creates
  directories above its root). Its entry there is flushed before a
  `.durable` marker is written in the root; a store that finds no marker
  flushes the parent itself, which needs read access to the parent. A reader sees the old object or the new one, never
  part of one. On Linux and macOS, a `Put` or `Delete` that returned
  survives a crash. If a directory flush before the rename fails, `Put`
  fails with the object still a temporary file, and the next `Put` flushes
  again. If the flush after the rename (or a delete's) fails even on a
  retry, the change has already happened: the call reports success, and
  `app.Storage()` logs a warning that it may not survive a crash. `Open`
  reads from disk as you read.
- **Temporary files.** A failed `Put` removes its temporary file. A process
  killed mid-`Put` can't, so each store writes its temporary files in its
  own work directory (`<root>/tmp/w-*`) and holds a lock on that
  directory's `owner` file while it exists. The file is locked before it
  gets that name. A store's first `Put` removes the work directories whose
  owner file it can lock: those of processes that are gone. A live store's
  directory is never removed, however long its `Put`s wait on their
  sources, and neither is a directory with no owner file (a store setting
  it up, or one that died doing so and left it empty). No age or clock is
  involved. The locks are `flock` (Linux, macOS, the BSDs) and `LockFileEx`
  (Windows). On any other platform nothing is swept.
- **Sharing.** Several processes can share one root (an app and its worker).
  Several *hosts* need a shared filesystem, or an S3-compatible store
  (STORAGE-3).
- **Windows.** An open reader keeps the version it opened while a `Put`
  replaces the object or a `Delete` removes it, as everywhere. The store
  opens objects with delete sharing and replaces and deletes them with POSIX
  semantics, which need NTFS on Windows 10 1709 or later. On a filesystem
  without them (FAT, say), an open reader blocks a replace or delete of its
  object until it is closed. Windows offers no directory flush a process can
  request, so writes and deletes there are atomic, but whether they survive
  a crash is up to the filesystem. Long paths work as they do for the `os`
  package: paths the store passes to Win32 directly get the extended
  `\\?\` form when they are long. CI runs the storage packages on Windows.
- **URLs** are served by the app at `GOMBIT_STORAGE_LOCAL_URL`: public
  objects to anyone, private ones through signed URLs only (see
  [Visibility and URLs](#visibility-and-urls)).

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
  `Put` that fails before sending the request that publishes the object
  (the `PutObject`, or the multipart upload's `CompleteMultipartUpload`)
  leaves the key as it was. "Sent" is observed, not assumed: the driver's
  HTTP client counts the requests written out in full, so a call that
  failed first (no credentials to sign with, the connection refused, the
  context ending) sent nothing. Once that request is sent, only S3's answer
  says whether it took effect. A 4xx answer is a refusal, and the key is as
  it was. No answer (a dropped connection, a timeout, the context ending) or
  a 5xx one leaves the outcome unknown, and `Put` fails with
  `ErrUnknownOutcome`. A multipart upload settles it by aborting: an abort
  that succeeds proves the upload never completed, so the key is as it was.
  The SDK retries every request but that one: a retry's answer says nothing
  about the attempt before it (a `CompleteMultipartUpload` that took effect
  and lost its answer makes its retry fail with `NoSuchUpload`), so only a
  single attempt's answer is classified. To retry, repeat the `Put`; that
  is always safe.
- **Incomplete uploads.** A failed multipart upload is aborted, on a fresh
  context, which removes the parts S3 has stored. It can't remove more. A
  part upload that never got an answer may still be in progress at S3 and
  be stored after the abort (AWS documents this), and the abort itself can
  fail (the network is still down, or `s3:AbortMultipartUpload` isn't
  granted). In either case `Put`'s error also carries an `*s3.AbortError`
  naming the upload, whose parts may stay stored, and billed. So any failed
  multipart `Put` may leave parts behind for a while. **Give the bucket a
  lifecycle rule that aborts incomplete multipart uploads**
  (`AbortIncompleteMultipartUpload`, after a day or so): that rule, not the
  driver, is what guarantees none lingers.
- **Permissions.** The credentials need `s3:GetObject`, `s3:PutObject`,
  `s3:DeleteObject` and `s3:AbortMultipartUpload` on the objects
  (`arn:aws:s3:::BUCKET/PREFIX*`), and `s3:ListBucket` on the bucket.
  Without `ListBucket`, S3 answers a request for a missing object with
  `403 Access Denied` instead of 404, and a missing object would become a 500
  rather than a 404. `s3:ListMultipartUploadParts` on the objects is
  optional: a fence (see [Cleanup](#cleanup)) uses it to free parts stored
  after an abort, and without it leaves them to the lifecycle rule. The
  fence itself needs only `s3:AbortMultipartUpload`.
- **Prefix.** `GOMBIT_STORAGE_S3_PREFIX` is a valid key followed by `/`
  (`myapp/prod/`), short enough to leave room for a key: the prefix and key
  together are held to the 1024-byte key limit. It is put before every key.
  `gombit config` and the driver check it with the same rule
  (`storage.ValidatePrefix`), so a configuration that validates is one the
  driver accepts.
- **Metadata** values that aren't printable ASCII (or that contain `=?`) are
  sent RFC 2047 encoded, which is S3's own encoding for non-ASCII metadata,
  and decoded on the way back, so they round-trip exactly. The contract's
  size limit counts the encoded headers, which is exactly how MinIO measures
  them.
- **Errors.** A missing object is `ErrNotFound`. Throttling, a 5xx or a
  network failure is `ErrUnavailable`, so `MapError` answers 503. A missing
  bucket or denied credentials is a plain error, so `MapError` answers 500
  rather than a misleading 404. A `HEAD` that returns 404 checks the
  bucket, because a `HEAD` response can't tell a missing object from a
  missing bucket. One `HeadBucket` runs at a time, however many misses
  arrive together, and its answer (the bucket exists, is missing, or is
  denied) is kept for a minute. A transient failure isn't kept.
- `ModTime` comes from S3 on `Stat` and `Open`. The `ObjectInfo` that `Put`
  returns leaves it zero, since S3 doesn't return one.
- **Checksums** are sent only where S3 requires them, because several
  S3-compatible services reject the SDK's newer default checksums.
- **`URL`** gives presigned `GetObject` URLs, and public URLs under
  `GOMBIT_STORAGE_S3_PUBLIC_URL` (see [Visibility and URLs](#visibility-and-urls)).

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
- `URL` returning either a URL or `ErrUnsupported` (or `ErrNotPublic` for a
  public URL; never for a signed one), and refusing a lifetime over
  `MaxURLExpiry`;
- for a store with direct uploads (`storage.DirectUploader`): invalid keys
  and options refused, and a grant that stores nothing by itself;
- for a store that lists (`storage.Lister`): exactly the objects under the
  prefix, each once, with its size and time; stopping at the callback's
  error; deleting as it goes; an ended context;
- publication (`storage.PreparePublish` / `Publish` / `Fence`): preparing
  publishes nothing; publishing copies the bytes, type and metadata (an
  empty object too) and refuses an occupied destination; for a
  `storage.Publisher`, a fenced copy is never published, and fencing is
  idempotent.

A driver that can take part in `storage/claims` declares how: implement
`storage.Publisher` if a write it sends can complete after the call
returns (a remote service), or `storage.BoundedWriter` if it cannot (its
`Put` publishes within the call, after checking its context). Declaring
`BoundedWriter` for a remote driver breaks the ownership guarantee.

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
- `IsPublic` and `ValidatePublicPrefix` apply the visibility rule, and
  `EscapeKey` turns a key into a URL path, with every byte that could mean
  something else in a URL encoded.
- A backend with no URL scheme of its own can take a `presign.Signer`
  (`storage/presign`) for its `URL`, and have the app serve
  `presign.Handler`, as the local and memory drivers do.
- `ContextReadCloser` binds an `Open` reader to its context (keeping `Seek`
  when the underlying reader has it).

The `storage` package imports only the standard library and Gombit's
`contract` package (a test enforces this), so an application that uses it
never pulls in a driver's dependencies.
