//go:build integration

package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/storagetest"
	"github.com/gombit-dev/gombit/storage/upload"
)

// An S3-compatible integration target, for example a local MinIO:
//
//	go test -tags integration ./storage/s3 -s3.endpoint http://127.0.0.1:9000 \
//	  -s3.access-key minioadmin -s3.secret-key minioadmin
var (
	s3Endpoint  = flag.String("s3.endpoint", "", "S3-compatible endpoint to run the integration tests against")
	s3Region    = flag.String("s3.region", "us-east-1", "region")
	s3Bucket    = flag.String("s3.bucket", "gombit-storage-test", "bucket (created if missing)")
	s3AccessKey = flag.String("s3.access-key", "", "access key id")
	s3SecretKey = flag.String("s3.secret-key", "", "secret access key")
)

// testStore returns a store on the integration bucket under a fresh random
// prefix, so each test (and each conformance check) starts empty.
func testStore(t *testing.T) *Store {
	t.Helper()
	return testStoreWith(t, func(*Config) {})
}

// testStoreWith is testStore with its Config changed by edit.
func testStoreWith(t *testing.T, edit func(*Config)) *Store {
	t.Helper()
	if *s3Endpoint == "" {
		t.Skip("set -s3.endpoint to run the S3 integration tests")
	}
	ctx := context.Background()
	var b [6]byte
	_, _ = rand.Read(b[:])
	cfg := Config{
		Endpoint:        *s3Endpoint,
		Region:          *s3Region,
		Bucket:          *s3Bucket,
		Prefix:          "test-" + hex.EncodeToString(b[:]) + "/",
		AccessKeyID:     *s3AccessKey,
		SecretAccessKey: *s3SecretKey,
		ForcePathStyle:  true,
	}
	edit(&cfg)
	s, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(*s3Bucket)})
	var owned *types.BucketAlreadyOwnedByYou
	var exists *types.BucketAlreadyExists
	if err != nil && !errors.As(err, &owned) && !errors.As(err, &exists) {
		t.Fatalf("create bucket: %v", err)
	}
	return s
}

func TestConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Storage { return testStore(t) })
}

// TestLargeUploadIsBounded: a multipart upload's memory does not grow with
// the object: 256 MiB of unknown length allocates about what 32 MiB does
// (the multipart part buffer), not the object.
func TestLargeUploadIsBounded(t *testing.T) {
	s := testStore(t)
	alloc := func(size int64) uint64 {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		info, err := s.Put(context.Background(), "big", io.LimitReader(zeros{}, size), storage.PutOptions{})
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size != size {
			t.Fatalf("size = %d, want %d", info.Size, size)
		}
		return after.TotalAlloc - before.TotalAlloc
	}
	small, large := alloc(32<<20), alloc(256<<20)
	t.Logf("32 MiB allocated %d MiB; 256 MiB allocated %d MiB", small>>20, large>>20)
	if large > small+32<<20 {
		t.Fatalf("224 more MiB of object allocated %d more MiB: the upload buffers the object", (large-small)>>20)
	}
	stat, err := s.Stat(context.Background(), "big")
	if err != nil || stat.Size != 256<<20 {
		t.Fatalf("Stat = %+v, %v", stat, err)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// TestSameCodeOnEveryDriver: the handler-side code a local-driver test uses
// works unchanged on S3 (the issue's "application code remains unchanged").
func TestSameCodeOnEveryDriver(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "avatars/1.png", strings.NewReader("png"), storage.PutOptions{ContentType: "image/png", Metadata: map[string]string{"original-name": "mé.png"}}); err != nil {
		t.Fatal(err)
	}
	body, info, err := s.Open(ctx, "avatars/1.png")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	got, _ := io.ReadAll(body)
	if string(got) != "png" || info.ContentType != "image/png" || info.Metadata["original-name"] != "mé.png" || info.ETag == "" || info.ModTime.IsZero() {
		t.Fatalf("read %q, info %+v", got, info)
	}
	if err := s.Delete(ctx, "avatars/1.png"); err != nil {
		t.Fatal(err)
	}
	if ok, err := storage.Exists(ctx, s, "avatars/1.png"); ok || err != nil {
		t.Fatalf("Exists after Delete = %v, %v", ok, err)
	}
}

// TestMissingBucketIsNotNotFound: a misconfigured bucket is an error, not a
// missing object (which a handler would answer with 404).
func TestMissingBucketIsNotNotFound(t *testing.T) {
	s := testStore(t)
	s.bucket = "gombit-no-such-bucket-" + strings.ToLower(t.Name())
	_, err := s.Stat(context.Background(), "k")
	if err == nil || errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Stat in a missing bucket = %v, want an error that is not ErrNotFound", err)
	}
}

// TestLargeMetadataRoundTrips: a non-ASCII value near the 2 KiB limit
// (counted in UTF-8 bytes, as S3 does) stores and reads back exactly.
func TestLargeMetadataRoundTrips(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	md := map[string]string{"original-name": strings.Repeat("é", 500), "note": "plain ascii note"}
	if err := storage.ValidateMetadata(md); err != nil {
		t.Fatalf("the test metadata is over the limit: %v", err)
	}
	if _, err := s.Put(ctx, "meta", strings.NewReader("x"), storage.PutOptions{Metadata: md}); err != nil {
		t.Fatal(err)
	}
	info, err := s.Stat(ctx, "meta")
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range md {
		if info.Metadata[k] != v {
			t.Fatalf("%s read back as %q (%d bytes), want %d bytes", k, info.Metadata[k], len(info.Metadata[k]), len(v))
		}
	}
}

// TestMetadataAtTheContractsLimit: metadata exactly at the contract's
// limit, ASCII or encoded, is accepted by the S3 service, so the contract's
// measure is not looser than S3's.
func TestMetadataAtTheContractsLimit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	maxRunes := func(r string) int {
		n := 0
		for storage.ValidateMetadata(map[string]string{"n": strings.Repeat(r, n+1)}) == nil {
			n++
		}
		return n
	}
	for _, md := range []map[string]string{
		{"n": strings.Repeat("a", maxRunes("a"))}, // "x-amz-meta-n" + value = 2048
		{"n": strings.Repeat("é", maxRunes("é"))}, // the largest RFC 2047-encoded value
	} {
		if err := storage.ValidateMetadata(md); err != nil {
			t.Fatalf("%d-byte value: %v", len(md["n"]), err)
		}
		if _, err := s.Put(ctx, "limit", strings.NewReader("x"), storage.PutOptions{Metadata: md}); err != nil {
			t.Fatalf("a %d-byte value at the contract's limit was refused: %v", len(md["n"]), err)
		}
		info, err := s.Stat(ctx, "limit")
		if err != nil || info.Metadata["n"] != md["n"] {
			t.Fatalf("read back %d bytes, %v", len(info.Metadata["n"]), err)
		}
	}
}

// TestSmallPutAllocatesItsSize: a Put of a small object with a declared
// size buffers about that size, not a whole part.
func TestSmallPutAllocatesItsSize(t *testing.T) {
	s := testStore(t)
	const size = 10 << 10
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if _, err := s.Put(context.Background(), "small", io.LimitReader(zeros{}, size), storage.PutOptions{Size: storage.KnownSize(size)}); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("a %d KiB Put allocated %d KiB", size>>10, allocated>>10)
	if allocated > 1<<20 {
		t.Fatalf("a %d KiB Put allocated %d KiB: it buffers a whole part", size>>10, allocated>>10)
	}
}

// TestFailedMultipartIsAborted: a multipart Put whose reader fails after
// the first parts leaves no incomplete upload behind (it would be billed).
func TestFailedMultipartIsAborted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	boom := errors.New("the source failed")
	src := io.MultiReader(io.LimitReader(zeros{}, 20<<20), failing{boom})
	if _, err := s.Put(ctx, "aborted", src, storage.PutOptions{}); !errors.Is(err, boom) {
		t.Fatalf("Put = %v, want the source's error", err)
	}
	out, err := s.client.ListMultipartUploads(ctx, &awss3.ListMultipartUploadsInput{Bucket: aws.String(*s3Bucket), Prefix: aws.String(s.prefix + "aborted")}) // MinIO lists uploads for an exact key
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Uploads) != 0 {
		t.Fatalf("%d incomplete multipart uploads left behind", len(out.Uploads))
	}
	if ok, err := storage.Exists(ctx, s, "aborted"); ok || err != nil {
		t.Fatalf("Exists = %v, %v; want nothing stored", ok, err)
	}
}

type failing struct{ err error }

func (f failing) Read([]byte) (int, error) { return 0, f.err }

// fetch GETs u without credentials: the status and the body.
func fetch(t *testing.T, u string) (int, string) {
	t.Helper()
	resp, err := http.Get(u) // #nosec G107 -- the test's own presigned URL.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestSignedURLs: a presigned URL reads a private object without
// credentials until it expires; the object is not readable without one.
func TestSignedURLs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := "private/ré sumé+1.txt"
	if _, err := s.Put(ctx, key, strings.NewReader("secret bytes"), storage.PutOptions{ContentType: "text/plain"}); err != nil {
		t.Fatal(err)
	}
	u, err := s.URL(ctx, key, storage.SignedURL(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if code, body := fetch(t, u); code != http.StatusOK || body != "secret bytes" {
		t.Fatalf("GET the signed URL = %d %q", code, body)
	}
	unsigned := strings.SplitN(u, "?", 2)[0]
	if code, _ := fetch(t, unsigned); code != http.StatusForbidden {
		t.Fatalf("GET without the signature = %d, want 403: the object is private", code)
	}
	tampered := strings.Replace(u, "X-Amz-Expires=2", "X-Amz-Expires=3600", 1)
	if code, _ := fetch(t, tampered); code != http.StatusForbidden {
		t.Fatalf("GET with a longer expiry = %d, want 403", code)
	}
	// A lifetime finer than S3's whole seconds is refused, not sent as an
	// X-Amz-Expires S3 would count differently from the local driver.
	if _, err := s.URL(ctx, key, storage.SignedURL(time.Millisecond)); !errors.Is(err, storage.ErrInvalidOptions) {
		t.Fatalf("a 1ms signed URL = %v, want ErrInvalidOptions", err)
	}
	time.Sleep(3 * time.Second)
	if code, _ := fetch(t, u); code != http.StatusForbidden {
		t.Fatalf("GET after the expiry = %d, want 403", code)
	}
	if _, err := s.URL(ctx, key, storage.PublicURL()); !errors.Is(err, storage.ErrNotPublic) {
		t.Fatalf("a public URL for a private key = %v, want ErrNotPublic", err)
	}
}

// TestPublicURLs: with a bucket policy that lets anyone read the public
// prefix (what the operator configures), a public URL reads a public
// object; a private object in the same bucket stays private.
func TestPublicURLs(t *testing.T) {
	s := testStoreWith(t, func(c *Config) {
		c.PublicPrefix = "public/"
		c.PublicURL = strings.TrimSuffix(*s3Endpoint, "/") + "/" + *s3Bucket
	})
	ctx := context.Background()
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:GetObject"],` +
		`"Resource":["arn:aws:s3:::` + *s3Bucket + `/` + s.prefix + `public/*"]}]}`
	if _, err := s.client.PutBucketPolicy(ctx, &awss3.PutBucketPolicyInput{Bucket: aws.String(*s3Bucket), Policy: aws.String(policy)}); err != nil {
		t.Fatalf("set the bucket policy: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.client.DeleteBucketPolicy(ctx, &awss3.DeleteBucketPolicyInput{Bucket: aws.String(*s3Bucket)})
	})
	for _, key := range []string{"public/logo & co.png", "private/x.txt"} {
		if _, err := s.Put(ctx, key, strings.NewReader("bytes of "+key), storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	u, err := s.URL(ctx, "public/logo & co.png", storage.PublicURL())
	if err != nil {
		t.Fatal(err)
	}
	if code, body := fetch(t, u); code != http.StatusOK || body != "bytes of public/logo & co.png" {
		t.Fatalf("GET %s = %d %q", u, code, body)
	}
	private := s.publicURL + "/" + storage.EscapeKey(s.prefix+"private/x.txt")
	if code, _ := fetch(t, private); code != http.StatusForbidden {
		t.Fatalf("GET a private object's would-be public URL = %d, want 403", code)
	}
}

// TestDirectUploads: a grant from upload.Authorize uploads straight to the
// bucket; S3 refuses any other length, type, or metadata; upload.Confirm
// accepts what the grant allowed.
func TestDirectUploads(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	png := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), make([]byte, 100)...)
	policy := upload.Policy{MaxBytes: 1 << 20, Types: []string{"image/png"}, Prefix: "avatars/"}
	g, err := upload.Authorize(ctx, s, policy, int64(len(png)), "image/png", "résumé.png")
	if err != nil {
		t.Fatal(err)
	}
	put := func(body []byte, edit func(*http.Request)) int {
		r, _ := http.NewRequest(g.Request.Method, g.Request.URL, bytes.NewReader(body))
		for k, v := range g.Request.Header {
			r.Header.Set(k, v)
		}
		if edit != nil {
			edit(r)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := put(append(png, 0), nil); code != http.StatusForbidden {
		t.Fatalf("a longer body = %d, want 403", code)
	}
	if code := put(png, func(r *http.Request) { r.Header.Set("Content-Type", "text/html") }); code != http.StatusForbidden {
		t.Fatalf("another type = %d, want 403", code)
	}
	if code := put(png, func(r *http.Request) { r.Header.Set("X-Amz-Meta-Filename", "other.png") }); code != http.StatusForbidden {
		t.Fatalf("other metadata = %d, want 403", code)
	}
	if ok, _ := storage.Exists(ctx, s, g.Key); ok {
		t.Fatal("a refused upload was stored")
	}
	if code := put(png, nil); code != http.StatusOK {
		t.Fatalf("the granted upload = %d", code)
	}
	f, err := upload.Confirm(ctx, s, g.Key, policy)
	if err != nil {
		t.Fatal(err)
	}
	if f.Size != int64(len(png)) || f.ContentType != "image/png" || f.Filename != "résumé.png" {
		t.Fatalf("confirmed %+v", f)
	}
	// The grant is spent: other bytes of the same length and type cannot
	// replace the confirmed file.
	other := bytes.Repeat([]byte("<"), len(png))
	if code := put(other, nil); code != http.StatusPreconditionFailed {
		t.Fatalf("replaying the grant = %d, want 412", code)
	}
	body, _, err := s.Open(ctx, g.Key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(body)
	_ = body.Close()
	if !bytes.Equal(got, png) {
		t.Fatal("the confirmed file was replaced")
	}
	if !g.Request.Expires.After(time.Now()) || g.Request.Header["If-None-Match"] != "*" {
		t.Fatalf("grant %+v", g.Request)
	}
}

// stagingClaims is a minimal upload.Claimer: every key staged, promoted
// once, held after.
type stagingClaims struct{ staged, promoted map[string]bool }

func (c *stagingClaims) Pending(context.Context, string, time.Time) error { return nil }
func (c *stagingClaims) Stage(_ context.Context, key string, _ time.Time) error {
	c.staged[key] = true
	return nil
}
func (c *stagingClaims) Promote(_ context.Context, key string, _ time.Time) (bool, error) {
	if !c.staged[key] || c.promoted[key] {
		return false, nil
	}
	c.promoted[key] = true
	return true, nil
}
func (c *stagingClaims) Unpromote(_ context.Context, key string) error {
	delete(c.promoted, key)
	return nil
}
func (c *stagingClaims) Abandon(context.Context, string) (bool, error) { return false, nil }

// TestStagedDirectUploads: under Policy.Claims, the presigned PUT goes to
// the staging key; Confirm promotes it with CopyObject (the bytes, type and
// metadata), and deletes the staged copy. Replaying the grant afterwards,
// as a late PUT would land, only ever writes the staging key again.
func TestStagedDirectUploads(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	png := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), make([]byte, 100)...)
	policy := upload.Policy{MaxBytes: 1 << 20, Types: []string{"image/png"}, Prefix: "avatars/", Claims: &stagingClaims{staged: map[string]bool{}, promoted: map[string]bool{}}}
	g, err := upload.Authorize(ctx, s, policy, int64(len(png)), "image/png", "résumé.png")
	if err != nil {
		t.Fatal(err)
	}
	put := func(body []byte) int {
		r, _ := http.NewRequest(g.Request.Method, g.Request.URL, bytes.NewReader(body))
		for k, v := range g.Request.Header {
			r.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	exists := func(key string) bool {
		ok, err := storage.Exists(ctx, s, key)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if code := put(png); code != http.StatusOK {
		t.Fatalf("the granted upload = %d", code)
	}
	if exists(g.Key) || !exists(upload.StagingKey(g.Key)) {
		t.Fatal("the grant did not upload to the staging key only")
	}
	f, err := upload.Confirm(ctx, s, g.Key, policy)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Stat(ctx, g.Key)
	if err != nil || f.Key != g.Key || info.Size != int64(len(png)) || info.ContentType != "image/png" || info.StoredFilename() != "résumé.png" {
		t.Fatalf("promoted %+v (%+v, %v)", f, info, err)
	}
	if exists(upload.StagingKey(g.Key)) {
		t.Fatal("the staged copy was left")
	}
	if code := put(bytes.Repeat([]byte("<"), len(png))); code != http.StatusOK {
		t.Fatalf("replaying the grant = %d", code)
	}
	body, _, err := s.Open(ctx, g.Key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(body)
	_ = body.Close()
	if !bytes.Equal(got, png) {
		t.Fatal("a PUT with the grant changed the claimed key")
	}
}

// TestListSkipsForeignKeys: an object under the store's prefix that is not
// a valid storage key (written by another tool) is not listed, since no
// method could act on it.
func TestListSkipsForeignKeys(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "mine/a", strings.NewReader("x"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.client.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String(*s3Bucket), Key: aws.String(s.prefix + "mine/dot./x"), Body: strings.NewReader("y")}); err != nil {
		t.Fatal(err)
	}
	var keys []string
	if err := s.List(ctx, "mine/", func(o storage.ObjectInfo) error { keys = append(keys, o.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "mine/a" {
		t.Fatalf("List = %v, want only mine/a", keys)
	}
}

// TestIfAbsentMultipart: IfAbsent holds for a multipart upload too: S3
// refuses its CompleteMultipartUpload ("If-None-Match: *") where an object
// exists, the Put fails with ErrExists, and the object stays as it was.
func TestIfAbsentMultipart(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := "if-absent/large"
	if _, err := s.Put(ctx, key, strings.NewReader("small original"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	large := io.LimitReader(rand.Reader, PartSize+1)
	_, err := s.Put(ctx, key, large, storage.PutOptions{IfAbsent: true})
	if !errors.Is(err, storage.ErrExists) || errors.Is(err, storage.ErrUnknownOutcome) {
		t.Fatalf("a multipart IfAbsent Put on a held key = %v, want ErrExists", err)
	}
	body, info, err := s.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	got, _ := io.ReadAll(body)
	if string(got) != "small original" || info.Size != int64(len("small original")) {
		t.Fatalf("the key holds %d bytes after the refused multipart Put, want the original", info.Size)
	}
	fresh := "if-absent/large-fresh"
	if _, err := s.Put(ctx, fresh, io.LimitReader(rand.Reader, PartSize+1), storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatalf("a multipart IfAbsent Put on an empty key = %v", err)
	}
}

// TestConfirmRefusesHeadersTheGrantDidNotSet: SigV4 leaves standard
// headers outside X-Amz-SignedHeaders unauthenticated, and S3 keeps five of
// them with the object and serves them back. A grant holder who adds one to
// an otherwise valid upload gets it stored; Confirm then refuses the
// upload (ErrMalformed) and deletes it, so a confirmed object carries only
// what its grant described.
func TestConfirmRefusesHeadersTheGrantDidNotSet(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	png := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), make([]byte, 100)...)
	policy := upload.Policy{MaxBytes: 1 << 20, Types: []string{"image/png"}, Prefix: "avatars/"}
	for header, value := range map[string]string{
		"Cache-Control":       "public, max-age=31536000",
		"Content-Disposition": "attachment; filename=evil.html",
		"Content-Encoding":    "gzip",
		"Content-Language":    "fr",
		"Expires":             "Wed, 21 Oct 2037 07:28:00 GMT",
	} {
		t.Run(header, func(t *testing.T) {
			g, err := upload.Authorize(ctx, s, policy, int64(len(png)), "image/png", "a.png")
			if err != nil {
				t.Fatal(err)
			}
			r, _ := http.NewRequest(g.Request.Method, g.Request.URL, bytes.NewReader(png))
			for k, v := range g.Request.Header {
				r.Header.Set(k, v)
			}
			r.Header.Set(header, value)
			resp, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Skipf("S3 refused the unsigned %s (%d): nothing to confirm", header, resp.StatusCode)
			}
			head, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.prefix + g.Key)})
			if err != nil {
				t.Fatal(err)
			}
			if aws.ToString(head.CacheControl)+aws.ToString(head.ContentDisposition)+aws.ToString(head.ContentEncoding)+aws.ToString(head.ContentLanguage)+aws.ToString(head.ExpiresString) == "" {
				t.Fatalf("S3 did not keep the %s header: the test proves nothing", header)
			}
			if _, err := upload.Confirm(ctx, s, g.Key, policy); !errors.Is(err, upload.ErrMalformed) {
				t.Fatalf("Confirm of an upload with an unsigned %s = %v, want ErrMalformed", header, err)
			}
			if ok, _ := storage.Exists(ctx, s, g.Key); ok {
				t.Fatalf("the refused upload with %s was not deleted", header)
			}
		})
	}
}
