// Package s3 is a storage.Storage on S3-compatible object storage: AWS S3,
// Cloudflare R2, MinIO, and other services that speak the S3 API.
//
// Objects are stored under Config.Prefix + key in one bucket. Put reads
// the object in PartSize pieces into one buffer (sized to the object when
// its declared size is smaller): an object that fits in one piece is a
// single PutObject, a larger one a multipart upload of PartSize parts sent
// one after another. A Put holds at most PartSize in memory, whatever the
// object's size (the largest object is 10,000 parts, about 78 GiB). The
// object appears only when the upload completes (S3 writes are atomic).
//
// # Failed writes
//
// A Put that fails before sending the request that publishes the object
// (the PutObject, or the CompleteMultipartUpload) leaves the key as it was.
// "Sent" is observed, not assumed: the driver's HTTP client counts, per
// call, the requests written out in full and the responses that came back
// (httptrace). A call that failed before writing its request (no
// credentials, nothing signed, the connection refused, ctx ending first)
// sent nothing. Once that request is sent, only S3's answer says whether
// it took effect:
// a 4xx answer is a refusal, and the key is as it was; no answer (a dropped
// connection, a timeout, ctx ending), or a 5xx one, leaves the outcome
// unknown, and Put fails with storage.ErrUnknownOutcome. The SDK does not
// retry that request (every other one it does): a retry's answer says
// nothing about the attempt before it (a Complete that took effect and lost
// its answer makes its retry fail with NoSuchUpload), so only one
// attempt's answer is ever classified. The way to retry is to repeat the
// Put, which is safe. A multipart
// upload settles that by aborting the upload: an abort that succeeds proves
// the upload never completed, so the key is as it was; one that finds the
// upload gone means it completed, and the outcome stays unknown (the new
// object is most likely in place).
//
// A failed multipart upload is aborted, on a fresh context, which removes
// the parts S3 has stored. It cannot remove more: a part upload that never
// got an answer may still be in progress at S3 and be stored after the
// abort (AWS documents this), and the abort itself can fail (the network
// still down, s3:AbortMultipartUpload not granted). In either case Put's
// error also carries an *AbortError naming the upload, whose parts may stay
// (and be billed). So any failed multipart Put may leave parts behind for
// a while, and only a lifecycle rule on the bucket that aborts incomplete
// multipart uploads after a day or so (AbortIncompleteMultipartUpload)
// guarantees none lingers: give the bucket one.
//
// # Permissions
//
// The credentials need s3:GetObject, s3:PutObject, s3:DeleteObject, and
// s3:AbortMultipartUpload on the objects (arn:aws:s3:::BUCKET/PREFIX*), and
// s3:ListBucket on the bucket (arn:aws:s3:::BUCKET). Without ListBucket, S3
// answers a request for a missing object with 403 Access Denied rather than
// 404, and the driver cannot tell a missing object from a denied one.
//
// S3 carries user metadata as ASCII HTTP headers. A value that is not
// printable ASCII is sent RFC 2047 encoded (mime.BEncoding), as S3
// expects, and decoded on the way back; storage.ValidateMetadata measures
// exactly those headers against the 2 KB limit.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/gombit-dev/gombit/storage"
)

// Config configures a Store. It mirrors config.S3StorageConfig.
type Config struct {
	// Endpoint is the service's base URL; empty means AWS S3.
	Endpoint string
	// Region signs requests.
	Region string
	// Bucket holds the objects.
	Bucket string
	// Prefix is put before every key ("myapp/").
	Prefix string
	// AccessKeyID and SecretAccessKey are static credentials; both empty
	// uses the AWS default credential chain.
	AccessKeyID     string
	SecretAccessKey string
	// ForcePathStyle addresses the bucket in the URL path (MinIO and most
	// S3-compatible services), not the host name.
	ForcePathStyle bool
}

// String describes the store without its credentials.
func (c Config) String() string {
	return fmt.Sprintf("s3 bucket %q prefix %q at %s", c.Bucket, c.Prefix, c.endpointName())
}

func (c Config) endpointName() string {
	if c.Endpoint == "" {
		return "AWS"
	}
	return c.Endpoint
}

// Store is an S3-backed storage.Storage. It is safe for concurrent use.
type Store struct {
	client *awss3.Client
	bucket string
	prefix string
	// bucketState is what a HEAD request's 404 needs: it does not say
	// whether the object or the bucket is missing (see checkBucket).
	bucketState bucketCheck
}

// bucketCheck is the bucket's last known state. One HeadBucket runs at a
// time; callers that miss the cache meanwhile wait for its answer. A
// definite answer (the bucket exists, is missing, or is denied) is kept
// for bucketRecheck; a transient failure is not kept.
type bucketCheck struct {
	mu      sync.Mutex
	at      time.Time // when result was found; zero for none
	result  error     // nil: the bucket exists
	pending *bucketCall
}

// bucketCall is one HeadBucket in flight; done is closed once state is set.
type bucketCall struct {
	done  chan struct{}
	state bucketState
}

// bucketState is a HeadBucket's answer: the state to report (nil: the
// bucket exists), and whether it is definite enough to keep.
type bucketState struct {
	err  error
	keep bool
}

// PartSize is the size of each multipart upload part, and the most a Put
// holds in memory.
const PartSize = 8 << 20

// bucketRecheck is how long a checked bucket state (it exists, it does not,
// or access to it is denied) is taken to still hold.
const bucketRecheck = time.Minute

// bucketProbeTimeout bounds a HeadBucket, which runs apart from any one
// caller's context (the callers waiting on it may outlive it).
const bucketProbeTimeout = 10 * time.Second

// AbortError is a multipart upload that a failed Put may have left parts
// of: its abort failed, or a part upload was still unanswered when it was
// aborted (S3 may store such a part afterwards). Err says which. The parts
// stay stored, and billed, until the bucket's lifecycle rule for incomplete
// uploads removes them, or someone aborts UploadID again. Put's error
// carries it (errors.As), alongside the failure it reports; it does not
// change how that failure is classified.
type AbortError struct {
	Key      string // the S3 object key
	UploadID string
	Err      error // the abort's failure, or errPartInFlight
}

func (e *AbortError) Error() string {
	return fmt.Sprintf("s3 storage: multipart upload %s of %q may have left parts stored until the bucket's lifecycle rule removes them: %v", e.UploadID, e.Key, e.Err)
}

// errPartInFlight is AbortError.Err for an upload aborted with a part
// upload unanswered.
var errPartInFlight = errors.New("a part upload was still unanswered when the upload was aborted, and S3 may store it afterwards")

// attempts counts, for one SDK call, the HTTP requests written out in full
// and the responses that came back. A request never written in full
// cannot have taken effect; one whose response never came may have.
type attempts struct{ written, answered atomic.Int32 }

// sent reports whether any request of the call was written in full.
func (a *attempts) sent() bool { return a.written.Load() > 0 }

// unanswered reports whether a request was written and got no response.
func (a *attempts) unanswered() bool { return a.written.Load() > a.answered.Load() }

type attemptsKey struct{}

// counting returns ctx carrying a fresh attempts for one SDK call.
func counting(ctx context.Context) (context.Context, *attempts) {
	a := new(attempts)
	return context.WithValue(ctx, attemptsKey{}, a), a
}

// countingClient is the S3 client's HTTP client: for a call made with
// counting, it records each request written in full (httptrace) and each
// response (a final one, as Do returns it: an interim "100 Continue",
// which S3 sends before reading a large body, is not an answer).
type countingClient struct{ next awss3.HTTPClient }

func (c countingClient) Do(req *http.Request) (*http.Response, error) {
	a, ok := req.Context().Value(attemptsKey{}).(*attempts)
	if !ok {
		return c.next.Do(req)
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				a.written.Add(1)
			}
		},
	}))
	resp, err := c.next.Do(req)
	if resp != nil {
		a.answered.Add(1)
	}
	return resp, err
}

var _ storage.Storage = (*Store)(nil)

// New returns a store for cfg. It makes no request; the first operation
// finds out whether the bucket and credentials work.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("s3 storage: empty bucket")
	}
	if cfg.Region == "" {
		return nil, errors.New("s3 storage: empty region")
	}
	if (cfg.AccessKeyID == "") != (cfg.SecretAccessKey == "") {
		return nil, errors.New("s3 storage: set both the access key id and the secret access key, or neither")
	}
	// The prefix starts every object key: config.Validate applies this same
	// rule, so a configuration it accepts is one New accepts.
	if err := storage.ValidatePrefix(cfg.Prefix); err != nil {
		return nil, fmt.Errorf("s3 storage: %w", err)
	}
	loadOpts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(cfg.Region)}
	if cfg.AccessKeyID != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("s3 storage: %w", err)
	}
	client := awss3.NewFromConfig(awsCfg, func(o *awss3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.ForcePathStyle
		// Observe what was sent: see attempts.
		o.HTTPClient = countingClient{next: o.HTTPClient}
		// Checksums only where S3 requires them: several S3-compatible
		// services reject the newer default trailing checksums.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	return &Store{client: client, bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

// objectKey is key's S3 object key, validated and prefixed.
func (s *Store) objectKey(key string) (string, error) {
	if err := storage.ValidateKey(key); err != nil {
		return "", err
	}
	full := s.prefix + key
	if len(full) > storage.MaxKeyBytes {
		return "", fmt.Errorf("%w: %d bytes with the store's prefix, more than %d", storage.ErrInvalidKey, len(full), storage.MaxKeyBytes)
	}
	return full, nil
}

// Put implements storage.Storage.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.ObjectInfo, error) {
	info, err := s.put(ctx, key, r, opts)
	return info, storage.Wrap("put", key, err)
}

func (s *Store) put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.ObjectInfo, error) {
	objKey, err := s.objectKey(key)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if err := storage.ValidatePutOptions(opts); err != nil {
		return storage.ObjectInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, err
	}
	contentType := opts.ContentType
	if contentType == "" {
		contentType = storage.DefaultContentType
	}
	size, etag, err := s.upload(ctx, objKey, storage.PutReader(ctx, r, opts), opts.Size, contentType, encodeMetadata(opts.Metadata))
	if err != nil {
		return storage.ObjectInfo{}, classifyPut(ctx, err)
	}
	return storage.ObjectInfo{
		Key:         key,
		Size:        size,
		ContentType: contentType,
		ETag:        strings.Trim(etag, `"`),
		Metadata:    cloneOrNil(opts.Metadata),
	}, nil
}

// upload stores body under objKey: one PutObject when it fits in a part, a
// multipart upload otherwise. It returns the bytes stored and the ETag.
func (s *Store) upload(ctx context.Context, objKey string, body io.Reader, declared *int64, contentType string, md map[string]string) (int64, string, error) {
	bufSize := int64(PartSize)
	if declared != nil && *declared < bufSize {
		bufSize = *declared + 1 // room to see that the source ends there
	}
	buf := make([]byte, bufSize)
	n, err := io.ReadFull(body, buf)
	switch {
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		// Nothing has been sent yet: a context that has ended is a failure
		// that leaves the key as it was.
		if err := ctx.Err(); err != nil {
			return 0, "", err
		}
		callCtx, sent := counting(ctx)
		out, err := s.client.PutObject(callCtx, &awss3.PutObjectInput{
			Bucket:        aws.String(s.bucket),
			Key:           aws.String(objKey),
			Body:          bytes.NewReader(buf[:n]),
			ContentLength: aws.Int64(int64(n)),
			ContentType:   aws.String(contentType),
			Metadata:      md,
		}, singleAttempt)
		if err != nil {
			return 0, "", publishFailed(err, sent)
		}
		return int64(n), aws.ToString(out.ETag), nil
	case err != nil:
		return 0, "", err
	}
	// The buffer is full, so there may be more: go multipart. (A buffer
	// sized to a declared size cannot fill: the size-checking reader fails
	// the read that would put a byte past the size into it.)
	return s.multipart(ctx, objKey, body, buf, contentType, md)
}

// multipart uploads buf (the first part, full) and the rest of body as
// PartSize parts, aborting the upload if anything fails.
func (s *Store) multipart(ctx context.Context, objKey string, body io.Reader, buf []byte, contentType string, md map[string]string) (_ int64, _ string, err error) {
	created, err := s.client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(objKey),
		ContentType: aws.String(contentType),
		Metadata:    md,
	})
	if err != nil {
		return 0, "", err
	}
	partInFlight := false // a part upload went unanswered
	defer func() {
		if err != nil {
			err = s.settle(ctx, objKey, aws.ToString(created.UploadId), err, partInFlight)
		}
	}()
	var parts []types.CompletedPart
	var total int64
	n := len(buf)
	for number := int32(1); n > 0; number++ {
		if number > 10000 {
			return 0, "", fmt.Errorf("s3 storage: object larger than 10000 parts of %d bytes", PartSize)
		}
		partCtx, part := counting(ctx)
		out, err := s.client.UploadPart(partCtx, &awss3.UploadPartInput{
			Bucket:        aws.String(s.bucket),
			Key:           aws.String(objKey),
			UploadId:      created.UploadId,
			PartNumber:    aws.Int32(number),
			Body:          bytes.NewReader(buf[:n]),
			ContentLength: aws.Int64(int64(n)),
		})
		if err != nil {
			partInFlight = part.unanswered()
			return 0, "", err
		}
		parts = append(parts, types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(number)})
		total += int64(n)
		var rerr error
		n, rerr = io.ReadFull(body, buf)
		if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
			return 0, "", rerr
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	callCtx, sent := counting(ctx)
	done, err := s.client.CompleteMultipartUpload(callCtx, &awss3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.bucket),
		Key:             aws.String(objKey),
		UploadId:        created.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	}, singleAttempt)
	if err != nil {
		return 0, "", publishFailed(err, sent)
	}
	return total, aws.ToString(done.ETag), nil
}

// singleAttempt sends a request once, without the SDK's automatic
// retries: the requests that publish an object use it, so that the answer
// publishFailed classifies is the answer to the only attempt.
func singleAttempt(o *awss3.Options) { o.RetryMaxAttempts = 1 }

// settle aborts the failed multipart upload uploadID of objKey, on a fresh
// context (the caller's may be what ended the upload), and returns the
// Put's error as the abort's result settles it: an abort that succeeds
// proves the upload never completed, so even a Complete whose answer was
// lost left the key as it was; an abort that finds the upload gone after
// such a Complete means it completed, and the outcome stays unknown; after
// any other failure, an upload already gone leaves nothing behind. A
// failed abort, or a part upload that went unanswered (partInFlight: S3
// may store it after the abort), may leave parts, which the error reports.
func (s *Store) settle(ctx context.Context, objKey, uploadID string, err error, partInFlight bool) error {
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_, abortErr := s.client.AbortMultipartUpload(abortCtx, &awss3.AbortMultipartUploadInput{
		Bucket: aws.String(s.bucket), Key: aws.String(objKey), UploadId: aws.String(uploadID),
	})
	var unknown *unknownOutcome
	switch {
	case abortErr == nil && partInFlight:
		// Aborted, but a part S3 never answered may still be stored.
		return &abortFailed{err: err, abort: &AbortError{Key: objKey, UploadID: uploadID, Err: errPartInFlight}}
	case abortErr == nil:
		if errors.As(err, &unknown) {
			return unknown.err // the Complete did not take effect
		}
		return err
	case isNoSuchUpload(abortErr):
		// No upload remains: either the ambiguous Complete took effect (the
		// outcome stays unknown), or something else ended the upload (a
		// lifecycle rule), and the Put's failure stands, nothing left behind.
		return err
	default:
		return &abortFailed{err: err, abort: &AbortError{Key: objKey, UploadID: uploadID, Err: abortErr}}
	}
}

func isNoSuchUpload(err error) bool {
	var noUpload *types.NoSuchUpload
	var apiErr smithy.APIError
	return errors.As(err, &noUpload) || (errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchUpload")
}

// unknownOutcome is a failure of the request that publishes an object when
// S3 did not answer it with a refusal: it may have taken effect.
type unknownOutcome struct{ err error }

func (u *unknownOutcome) Error() string { return u.err.Error() }
func (u *unknownOutcome) Unwrap() error { return u.err }

// abortFailed is a failed Put whose multipart upload could not be aborted.
type abortFailed struct {
	err   error
	abort *AbortError
}

func (a *abortFailed) Error() string { return a.err.Error() }
func (a *abortFailed) Unwrap() error { return a.err }

// publishFailed classifies a failure of the request that publishes an
// object, made with sent counting its attempt: a request never written in
// full (no credentials to sign it, the connection refused, ctx ending
// first) cannot have taken effect; an HTTP answer in the 4xx range is S3
// refusing it (the key is as it was); anything else (no answer, a 5xx
// one, ctx ending while waiting) may have taken effect.
func publishFailed(err error, sent *attempts) error {
	if !sent.sent() {
		return err
	}
	var respErr *smithyhttp.ResponseError
	if errors.As(err, &respErr) {
		if status := respErr.HTTPStatusCode(); status >= 400 && status < 500 {
			return err
		}
	}
	return &unknownOutcome{err: err}
}

// classifyPut classifies an upload's failure with classify, then adds what
// the upload knows: that the outcome is unknown (storage.ErrUnknownOutcome),
// and that a multipart upload could not be aborted (*AbortError). Neither
// changes the classification of the failure itself.
func classifyPut(ctx context.Context, err error) error {
	var failed *abortFailed
	var abort *AbortError
	if errors.As(err, &failed) {
		abort, err = failed.abort, failed.err
	}
	var unknown *unknownOutcome
	isUnknown := errors.As(err, &unknown)
	if isUnknown {
		err = unknown.err
	}
	out := classify(ctx, err)
	if isUnknown {
		out = errors.Join(storage.ErrUnknownOutcome, out)
	}
	if abort != nil {
		out = errors.Join(out, abort)
	}
	return out
}

// Open implements storage.Storage. The object streams from S3 as the
// caller reads it.
func (s *Store) Open(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	objKey, err := s.objectKey(key)
	if err != nil {
		return nil, storage.ObjectInfo{}, storage.Wrap("open", key, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, storage.ObjectInfo{}, storage.Wrap("open", key, err)
	}
	out, err := s.client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(objKey)})
	if err != nil {
		return nil, storage.ObjectInfo{}, storage.Wrap("open", key, classify(ctx, err))
	}
	info, err := objectInfo(key, aws.ToInt64(out.ContentLength), out.ContentType, out.ETag, out.LastModified, out.Metadata)
	if err != nil {
		_ = out.Body.Close()
		return nil, storage.ObjectInfo{}, storage.Wrap("open", key, err)
	}
	// The body follows the request context already; the wrapper makes it
	// exact (no buffered bytes after ctx ends), as the contract asks.
	return storage.ContextReadCloser(ctx, out.Body), info, nil
}

// Stat implements storage.Storage.
func (s *Store) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	objKey, err := s.objectKey(key)
	if err != nil {
		return storage.ObjectInfo{}, storage.Wrap("stat", key, err)
	}
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, storage.Wrap("stat", key, err)
	}
	out, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(objKey)})
	if err != nil {
		err = classify(ctx, err)
		if errors.Is(err, storage.ErrNotFound) {
			err = s.checkBucket(ctx, err)
		}
		return storage.ObjectInfo{}, storage.Wrap("stat", key, err)
	}
	info, err := objectInfo(key, aws.ToInt64(out.ContentLength), out.ContentType, out.ETag, out.LastModified, out.Metadata)
	return info, storage.Wrap("stat", key, err)
}

// Delete implements storage.Storage. S3 deletes are idempotent: deleting a
// missing object succeeds.
func (s *Store) Delete(ctx context.Context, key string) error {
	objKey, err := s.objectKey(key)
	if err != nil {
		return storage.Wrap("delete", key, err)
	}
	if err := ctx.Err(); err != nil {
		return storage.Wrap("delete", key, err)
	}
	_, err = s.client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(objKey)})
	return storage.Wrap("delete", key, classify(ctx, err))
}

// URL implements storage.Storage. Public and signed URLs, and the
// visibility rules they depend on, come with STORAGE-5; until then it
// returns storage.ErrUnsupported (after validating its arguments).
func (s *Store) URL(ctx context.Context, key string, opts storage.URLOptions) (string, error) {
	if _, err := s.objectKey(key); err != nil {
		return "", storage.Wrap("url", key, err)
	}
	if err := storage.ValidateURLOptions(opts); err != nil {
		return "", storage.Wrap("url", key, err)
	}
	if err := ctx.Err(); err != nil {
		return "", storage.Wrap("url", key, err)
	}
	return "", storage.Wrap("url", key, storage.ErrUnsupported)
}

// checkBucket returns notFound when the bucket exists, and a
// configuration error when it does not: a HEAD request's 404 carries no
// error code to tell them apart. The bucket's state is checked at most
// once per bucketRecheck however many misses arrive together: one
// HeadBucket runs, and the others wait for its answer (or for their own
// ctx to end).
func (s *Store) checkBucket(ctx context.Context, notFound error) error {
	result, err := s.bucketState.get(ctx, s.probeBucket)
	if err != nil {
		return err // ctx ended while waiting
	}
	if result == nil {
		return notFound
	}
	return result
}

// probeBucket asks S3 whether the bucket exists.
func (s *Store) probeBucket(ctx context.Context) bucketState {
	_, err := s.client.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	if err == nil {
		return bucketState{keep: true}
	}
	classified := classify(ctx, err)
	switch {
	case errors.Is(classified, storage.ErrNotFound):
		// Not the sentinel: a missing bucket is a configuration error, not
		// a missing object a handler would answer with 404.
		return bucketState{err: fmt.Errorf("s3 storage: bucket %q does not exist: %v", s.bucket, err), keep: true}
	case errors.Is(classified, storage.ErrUnavailable), ctx.Err() != nil:
		return bucketState{err: classified} // transient: check again next time
	default:
		return bucketState{err: classified, keep: true} // denied, or otherwise refused
	}
}

// get returns the cached state, or the answer of the probe in flight,
// starting one if none is (on a context of its own, bounded by
// bucketProbeTimeout, so it serves every caller waiting on it). Every
// caller, the one that started it included, stops waiting when its own ctx
// ends: err is then ctx's error.
func (b *bucketCheck) get(ctx context.Context, probe func(context.Context) bucketState) (result, err error) {
	b.mu.Lock()
	if !b.at.IsZero() && time.Since(b.at) < bucketRecheck {
		result := b.result
		b.mu.Unlock()
		return result, nil
	}
	call := b.pending
	if call == nil {
		call = &bucketCall{done: make(chan struct{})}
		b.pending = call
		go b.run(context.WithoutCancel(ctx), call, probe)
	}
	b.mu.Unlock()
	select {
	case <-call.done:
		return call.state.err, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// run performs call's probe and publishes its answer.
func (b *bucketCheck) run(ctx context.Context, call *bucketCall, probe func(context.Context) bucketState) {
	probeCtx, cancel := context.WithTimeout(ctx, bucketProbeTimeout)
	call.state = probe(probeCtx)
	cancel()
	b.mu.Lock()
	if call.state.keep {
		b.at, b.result = time.Now(), call.state.err
	}
	b.pending = nil
	b.mu.Unlock()
	close(call.done)
}

func objectInfo(key string, size int64, contentType, etag *string, modTime *time.Time, md map[string]string) (storage.ObjectInfo, error) {
	decoded, err := decodeMetadata(md)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	info := storage.ObjectInfo{
		Key:         key,
		Size:        size,
		ContentType: aws.ToString(contentType),
		ETag:        strings.Trim(aws.ToString(etag), `"`),
		ModTime:     aws.ToTime(modTime),
		Metadata:    decoded,
	}
	if info.ContentType == "" {
		info.ContentType = storage.DefaultContentType
	}
	return info, nil
}

// encodeMetadata RFC 2047-encodes each value that is not printable ASCII
// with mime.BEncoding (UTF-8, words of at most 75 characters), exactly
// what storage.MetadataValueWireLen measures. The contract refuses values
// containing "=?", so a printable-ASCII value never reads as encoded.
func encodeMetadata(md map[string]string) map[string]string {
	if len(md) == 0 {
		return nil
	}
	out := make(map[string]string, len(md))
	for name, value := range md {
		out[name] = mime.BEncoding.Encode("UTF-8", value)
	}
	return out
}

var wordDecoder = new(mime.WordDecoder)

// decodeMetadata reverses encodeMetadata, and decodes any RFC 2047 words S3
// itself produced.
func decodeMetadata(md map[string]string) (map[string]string, error) {
	if len(md) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(md))
	for name, value := range md {
		decoded, err := wordDecoder.DecodeHeader(value)
		if err != nil {
			return nil, fmt.Errorf("s3 storage: metadata %q: %w", name, err)
		}
		// S3 may return names in another case; storage metadata names are
		// lower-case.
		out[strings.ToLower(name)] = decoded
	}
	return out, nil
}

func cloneOrNil(md map[string]string) map[string]string {
	if len(md) == 0 {
		return nil
	}
	return maps.Clone(md)
}

// classify maps an SDK error to the storage sentinels: a missing object is
// ErrNotFound; throttling, a server error, or a network failure is
// ErrUnavailable; the caller's context ending is returned as the context's
// error. Anything else (a bad bucket name, denied credentials) keeps the
// SDK's error.
func classify(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return ctxErr
	}
	// A reader's own error (the caller's source failed, or ran past its
	// declared size) comes back through the upload manager unchanged.
	if errors.Is(err, storage.ErrSizeMismatch) {
		return err
	}
	var noKey *types.NoSuchKey
	var notFound *types.NotFound
	if errors.As(err, &noKey) || errors.As(err, &notFound) {
		return fmt.Errorf("%w: %v", storage.ErrNotFound, err)
	}
	var respErr *smithyhttp.ResponseError
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() != 0 { // 0: no response; see the network check below
		switch status := respErr.HTTPStatusCode(); {
		case status == http.StatusNotFound:
			var apiErr smithy.APIError
			if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchBucket" {
				return err // a configuration problem, not a missing object
			}
			return fmt.Errorf("%w: %v", storage.ErrNotFound, err)
		case status == http.StatusTooManyRequests || status >= 500:
			return errors.Join(storage.ErrUnavailable, err)
		}
		return err
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return errors.Join(storage.ErrUnavailable, err)
	}
	return err
}
