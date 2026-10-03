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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"strconv"
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

	"github.com/gombit-dev/gombit/internal/urlbase"
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
	// PublicPrefix makes the keys under it public (storage.IsPublic); empty
	// makes none public. The bucket must serve them to anyone: a bucket
	// policy allowing s3:GetObject on Bucket/Prefix+PublicPrefix*, or a
	// CDN in front of it.
	PublicPrefix string
	// PublicURL is the URL public objects are read from, standing for the
	// bucket's root: a public object's URL is PublicURL + "/" + Prefix +
	// key. A CDN ("https://cdn.example.com"), a custom domain, or the
	// bucket's own address. Empty: URL answers storage.ErrUnsupported for a
	// public URL (signed URLs still work).
	PublicURL string
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
	client    *awss3.Client
	presign   *awss3.PresignClient
	bucket    string
	prefix    string
	public    string // PublicPrefix
	publicURL string
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

// MaxUploadURLBytes is the largest direct upload UploadURL grants: a
// direct upload is one presigned PutObject, and S3's limit for a single
// PUT is 5 GiB. (Put has no such limit: it uploads larger objects in
// parts.)
const MaxUploadURLBytes = 5 << 30

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
	if err := storage.ValidatePublicPrefix(cfg.PublicPrefix); err != nil {
		return nil, fmt.Errorf("s3 storage: %v", err)
	}
	// The rule config.Validate applies to GOMBIT_STORAGE_S3_PUBLIC_URL too.
	if cfg.PublicURL != "" {
		if problem := urlbase.Root(cfg.PublicURL); problem != "" {
			return nil, fmt.Errorf("s3 storage: public URL %q: %s", cfg.PublicURL, problem)
		}
	}
	return &Store{
		client:    client,
		presign:   awss3.NewPresignClient(client),
		bucket:    cfg.Bucket,
		prefix:    cfg.Prefix,
		public:    cfg.PublicPrefix,
		publicURL: cfg.PublicURL,
	}, nil
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
	// IfAbsent is S3's conditional write: "If-None-Match: *" on the request
	// that publishes the object (PutObject, or CompleteMultipartUpload),
	// which S3 refuses with 412 where an object exists, atomically.
	var ifNoneMatch *string
	if opts.IfAbsent {
		ifNoneMatch = aws.String("*")
	}
	size, etag, err := s.upload(ctx, objKey, storage.PutReader(ctx, r, opts), opts.Size, contentType, encodeMetadata(opts.Metadata), ifNoneMatch)
	if err != nil {
		return storage.ObjectInfo{}, classifyPut(ctx, err, opts.IfAbsent)
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
func (s *Store) upload(ctx context.Context, objKey string, body io.Reader, declared *int64, contentType string, md map[string]string, ifNoneMatch *string) (int64, string, error) {
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
			IfNoneMatch:   ifNoneMatch,
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
	return s.multipart(ctx, objKey, body, buf, contentType, md, ifNoneMatch)
}

// multipart uploads buf (the first part, full) and the rest of body as
// PartSize parts, aborting the upload if anything fails.
func (s *Store) multipart(ctx context.Context, objKey string, body io.Reader, buf []byte, contentType string, md map[string]string, ifNoneMatch *string) (_ int64, _ string, err error) {
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
		IfNoneMatch:     ifNoneMatch,
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

// conditionFailed classifies the failure of a conditional write
// (IfAbsent): S3's 412 is storage.ErrExists (an object is there, and was
// left as it was); a 409 ConditionalRequestConflict, a conflicting write
// still in progress, is transient (storage.ErrUnavailable). It returns nil
// for any other failure.
func conditionFailed(err error) error {
	var respErr *smithyhttp.ResponseError
	if !errors.As(err, &respErr) {
		return nil
	}
	switch respErr.HTTPStatusCode() {
	case http.StatusPreconditionFailed:
		return fmt.Errorf("%w: %v", storage.ErrExists, err)
	case http.StatusConflict:
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && apiErr.ErrorCode() == "ConditionalRequestConflict" {
			return errors.Join(storage.ErrUnavailable, err)
		}
	}
	return nil
}

// classifyPut classifies an upload's failure: a conditional write's refusal
// (ifAbsent: storage.ErrExists, or a transient conflict) with
// conditionFailed, anything else with classify. It then adds what the
// upload knows: that the outcome is unknown (storage.ErrUnknownOutcome),
// and that a multipart upload could not be aborted (*AbortError), which a
// refusal keeps too. Neither changes the classification of the failure
// itself.
func classifyPut(ctx context.Context, err error, ifAbsent bool) error {
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
	var out error
	if ifAbsent {
		out = conditionFailed(err) // a conditional write's refusal, if it is one
	}
	if out == nil {
		out = classify(ctx, err)
	}
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

var _ storage.Publisher = (*Store)(nil)

// Copy parts: at most maxCopyPart bytes each (S3's limit for one
// UploadPartCopy), copyPartSize by default, and at most maxCopyParts of
// them, which covers S3's largest object (5 TiB). ETags are opaque, so the
// publication token's size is not assumed from the part count: it is
// checked as each ETag arrives (preparePublish).
const (
	maxCopyPart  = 5 << 30
	copyPartSize = 1 << 30
	maxCopyParts = 1100 // 1100 parts of 5 GiB cover 5 TiB
)

// publication is a prepared copy: the multipart upload of Key (the
// storage key) that Publish completes and Fence aborts, the ETag of each
// part as S3 returned it when the part was uploaded (the manifest
// CompleteMultipartUpload is sent; AWS forbids building it from a
// listing), and the object it will publish.
// A cleanup token (Cleanup) names only the key and upload ID: a prepared
// upload that can be fenced but not published, returned when preparing
// fails with something left to abort.
type publication struct {
	Key         string            `json:"k"`
	UploadID    string            `json:"u"`
	Cleanup     bool              `json:"c,omitempty"`
	ETags       []string          `json:"e"`
	Size        int64             `json:"s"`
	ContentType string            `json:"t"`
	Metadata    map[string]string `json:"m,omitempty"`
}

// cleanupToken is p's cleanup token: its key and upload ID only.
func (p publication) cleanupToken() (string, error) {
	return publication{Key: p.Key, UploadID: p.UploadID, Cleanup: true}.token()
}

func (p publication) token() (string, error) {
	b, err := json.Marshal(p)
	if err == nil && len(b) > storage.MaxPublicationToken {
		err = fmt.Errorf("s3 storage: a %d-byte publication token, more than %d", len(b), storage.MaxPublicationToken)
	}
	return string(b), err
}

func parsePublication(token string) (publication, error) {
	var p publication
	if err := json.Unmarshal([]byte(token), &p); err != nil || p.Key == "" || p.UploadID == "" {
		return publication{}, fmt.Errorf("%w: a malformed publication token", storage.ErrInvalidOptions)
	}
	return p, nil
}

// copyParts returns the part size a copy of size bytes uses: none (one
// whole-object part) up to maxCopyPart, else copyPartSize, grown to stay
// within maxCopyParts.
func copyParts(size int64) (int64, error) {
	if size <= maxCopyPart {
		return 0, nil
	}
	part := max(int64(copyPartSize), (size+maxCopyParts-1)/maxCopyParts)
	if part > maxCopyPart {
		return 0, fmt.Errorf("s3 storage: a %d-byte object is larger than %d parts of %d bytes", size, maxCopyParts, maxCopyPart)
	}
	return part, nil
}

// PreparePublish implements storage.Publisher: it creates a multipart
// upload of dst with src's content type and metadata, and copies src into
// its parts server-side (UploadPartCopy; the bytes stay in S3), keeping
// each part's ETag for the token. Nothing is published until Publish
// completes the upload. The token never exceeds
// storage.MaxPublicationToken: its size is checked as each (opaque) ETag
// arrives, and preparing fails as soon as the next would not fit. If
// preparing fails, the upload is aborted; if a part request went
// unanswered (S3 may still store the part after the abort) or the abort
// failed, the error is returned with a cleanup token (the key and upload
// ID only, checked to fit before any part is copied), for the caller to
// record and Fence.
func (s *Store) PreparePublish(ctx context.Context, src, dst string) (string, error) {
	token, err := s.preparePublish(ctx, src, dst)
	return token, storage.Wrap("publish", dst, err)
}

func (s *Store) preparePublish(ctx context.Context, src, dst string) (string, error) {
	srcKey, err := s.objectKey(src)
	if err != nil {
		return "", err
	}
	dstKey, err := s.objectKey(dst)
	if err != nil {
		return "", err
	}
	info, err := s.Stat(ctx, src)
	if err != nil {
		return "", err
	}
	partSize, err := copyParts(info.Size)
	if err != nil {
		return "", err
	}
	created, err := s.client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(dstKey),
		ContentType: aws.String(info.ContentType),
		Metadata:    encodeMetadata(info.Metadata),
	})
	if err != nil {
		return "", classify(ctx, err)
	}
	p := publication{Key: dst, UploadID: aws.ToString(created.UploadId), ETags: []string{}, Size: info.Size, ContentType: info.ContentType, Metadata: info.Metadata}
	// The cleanup token is what a failure returns: it must fit before
	// anything is copied, so that a prepared upload is always recordable.
	cleanup, cerr := p.cleanupToken()
	fail := func(err error, inFlight bool) (string, error) {
		// Nothing was published (only Publish completes the upload): abort
		// it. If that proves nothing is left, no token is needed.
		err = classifyPut(ctx, s.settle(ctx, dstKey, p.UploadID, err, inFlight), false)
		var abort *AbortError
		if !errors.As(err, &abort) {
			return "", err
		}
		if cerr != nil {
			// Unrecordable (an upload ID too long to fit any token): the
			// error says what was left; a lifecycle rule aborting
			// incomplete multipart uploads is the only backstop.
			return "", errors.Join(err, cerr)
		}
		return cleanup, err
	}
	if cerr != nil {
		return fail(cerr, false)
	}
	// used is the token's size so far; each ETag adds its JSON encoding and
	// a comma.
	base, err := json.Marshal(p)
	if err != nil {
		return fail(err, false)
	}
	used := len(base)
	addETag := func(etag string, number int32) error {
		enc, err := json.Marshal(etag)
		if err != nil {
			return err
		}
		if used+len(enc)+1 > storage.MaxPublicationToken {
			return fmt.Errorf("s3 storage: part %d's ETag would make the publication token larger than %d bytes", number, storage.MaxPublicationToken)
		}
		used += len(enc) + 1
		p.ETags = append(p.ETags, etag)
		return nil
	}
	if info.Size == 0 {
		// UploadPartCopy copies at least a byte; an empty object is one
		// empty part.
		partCtx, sent := counting(ctx)
		out, err := s.client.UploadPart(partCtx, &awss3.UploadPartInput{
			Bucket: aws.String(s.bucket), Key: aws.String(dstKey), UploadId: created.UploadId,
			PartNumber: aws.Int32(1), Body: bytes.NewReader(nil), ContentLength: aws.Int64(0),
		})
		if err != nil {
			return fail(err, sent.unanswered())
		}
		if err := addETag(aws.ToString(out.ETag), 1); err != nil {
			return fail(err, false)
		}
	}
	source := copySource(s.bucket, srcKey)
	for number, start := int32(1), int64(0); start < info.Size; number++ {
		in := &awss3.UploadPartCopyInput{
			Bucket: aws.String(s.bucket), Key: aws.String(dstKey), UploadId: created.UploadId,
			PartNumber: aws.Int32(number), CopySource: aws.String(source),
		}
		end := info.Size
		if partSize > 0 {
			end = min(start+partSize, info.Size)
			in.CopySourceRange = aws.String(fmt.Sprintf("bytes=%d-%d", start, end-1))
		}
		partCtx, sent := counting(ctx)
		out, err := s.client.UploadPartCopy(partCtx, in)
		if err != nil {
			return fail(err, sent.unanswered())
		}
		if out.CopyPartResult == nil || aws.ToString(out.CopyPartResult.ETag) == "" {
			return fail(fmt.Errorf("s3 storage: UploadPartCopy answered no ETag for part %d", number), false)
		}
		if err := addETag(aws.ToString(out.CopyPartResult.ETag), number); err != nil {
			return fail(err, false)
		}
		start = end
	}
	token, err := p.token()
	if err != nil {
		return fail(err, false) // cannot happen: the size was checked part by part
	}
	return token, nil
}

// copySource is CopySource for objKey in bucket: each segment escaped.
func copySource(bucket, objKey string) string {
	segments := strings.Split(objKey, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return url.PathEscape(bucket) + "/" + strings.Join(segments, "/")
}

// Publish implements storage.Publisher: it completes the prepared upload,
// once (a request sent without a definite answer is
// storage.ErrUnknownOutcome, which Fence settles), with
// "If-None-Match: *" where S3 honors it.
func (s *Store) Publish(ctx context.Context, token string) (storage.ObjectInfo, error) {
	p, err := parsePublication(token)
	if err != nil {
		return storage.ObjectInfo{}, storage.Wrap("publish", "", err)
	}
	info, err := s.publish(ctx, p)
	return info, storage.Wrap("publish", p.Key, err)
}

func (s *Store) publish(ctx context.Context, p publication) (storage.ObjectInfo, error) {
	objKey, err := s.objectKey(p.Key)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if p.Cleanup {
		return storage.ObjectInfo{}, fmt.Errorf("%w: a cleanup token can be fenced, not published", storage.ErrInvalidOptions)
	}
	// The manifest is the parts as S3 acknowledged them when they were
	// uploaded, recorded in the token.
	parts := make([]types.CompletedPart, len(p.ETags))
	for i, etag := range p.ETags {
		parts[i] = types.CompletedPart{ETag: aws.String(etag), PartNumber: aws.Int32(int32(i + 1))}
	}
	if len(parts) == 0 {
		return storage.ObjectInfo{}, fmt.Errorf("%w: a publication token without parts", storage.ErrInvalidOptions)
	}
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, err
	}
	callCtx, sent := counting(ctx)
	done, err := s.client.CompleteMultipartUpload(callCtx, &awss3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.bucket),
		Key:             aws.String(objKey),
		UploadId:        aws.String(p.UploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
		IfNoneMatch:     aws.String("*"),
	}, singleAttempt)
	if err != nil {
		return storage.ObjectInfo{}, classifyPut(ctx, publishFailed(err, sent), true)
	}
	return storage.ObjectInfo{
		Key:         p.Key,
		Size:        p.Size,
		ContentType: p.ContentType,
		ETag:        strings.Trim(aws.ToString(done.ETag), `"`),
		Metadata:    cloneOrNil(p.Metadata),
	}, nil
}

// fenceRounds is how many times Fence aborts an upload whose parts are
// still listed after an abort.
const fenceRounds = 3

// Fence implements storage.Publisher with AbortMultipartUpload. S3 orders
// an abort and a Complete of one upload: an abort that succeeds proves the
// upload never completed and never will; NoSuchUpload means it is over
// already (completed, or aborted before). Either way nothing more can be
// published: that is what nil proves. A part request still being processed
// may store its part after the abort, so, as AWS advises, Fence then lists
// the upload's parts and aborts again while any are listed. A listing is a
// snapshot, though: a part can still land after Fence returns. Such a part
// is never an object (it cannot be read or published); a bucket lifecycle
// rule aborting incomplete multipart uploads reclaims it. Any failure
// proves nothing, and is returned.
func (s *Store) Fence(ctx context.Context, token string) error {
	p, err := parsePublication(token)
	if err != nil {
		return storage.Wrap("fence", "", err)
	}
	objKey, err := s.objectKey(p.Key)
	if err != nil {
		return storage.Wrap("fence", p.Key, err)
	}
	for range fenceRounds {
		_, err := s.client.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{
			Bucket: aws.String(s.bucket), Key: aws.String(objKey), UploadId: aws.String(p.UploadID),
		})
		if err != nil && !isNoSuchUpload(err) {
			return storage.Wrap("fence", p.Key, classify(ctx, err))
		}
		out, err := s.client.ListParts(ctx, &awss3.ListPartsInput{
			Bucket: aws.String(s.bucket), Key: aws.String(objKey), UploadId: aws.String(p.UploadID),
		})
		switch {
		case isNoSuchUpload(err), err == nil && len(out.Parts) == 0:
			return nil
		case err != nil:
			return storage.Wrap("fence", p.Key, classify(ctx, err))
		}
	}
	return storage.Wrap("fence", p.Key, fmt.Errorf("%w: parts of upload %s remain after %d aborts", storage.ErrUnavailable, p.UploadID, fenceRounds))
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

// presignedExpiry is when the presigned URL rawURL stops working, as S3
// computes it: X-Amz-Date (whole seconds, the signing time) plus
// X-Amz-Expires. Read from the URL itself, it is exactly what S3 enforces,
// not a second clock reading that could fall in another second.
func presignedExpiry(rawURL string) (time.Time, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return time.Time{}, err
	}
	q := u.Query()
	date, err := time.Parse("20060102T150405Z", q.Get("X-Amz-Date"))
	if err != nil {
		return time.Time{}, fmt.Errorf("s3 storage: presigned URL without a valid X-Amz-Date: %w", err)
	}
	secs, err := strconv.ParseInt(q.Get("X-Amz-Expires"), 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}, fmt.Errorf("s3 storage: presigned URL without a valid X-Amz-Expires (%q)", q.Get("X-Amz-Expires"))
	}
	return date.Add(time.Duration(secs) * time.Second), nil
}

// URL implements storage.Storage. A public URL is PublicURL + "/" + the
// escaped object key, for a key under PublicPrefix (storage.ErrNotPublic
// otherwise; storage.ErrUnsupported without a PublicURL). A signed URL is a
// presigned GetObject request (SigV4 query parameters) valid for
// opts.Expires counted from its X-Amz-Date, the signing second (see
// storage.SignedURL); it carries the credentials' authority, so it works for a
// private object, and stops working when it expires or the credentials
// are revoked (a URL signed with temporary credentials, such as an IAM
// role's, also ends when they do).
func (s *Store) URL(ctx context.Context, key string, opts storage.URLOptions) (string, error) {
	full, err := s.objectKey(key)
	if err != nil {
		return "", storage.Wrap("url", key, err)
	}
	if err := storage.ValidateURLOptions(opts); err != nil {
		return "", storage.Wrap("url", key, err)
	}
	if err := ctx.Err(); err != nil {
		return "", storage.Wrap("url", key, err)
	}
	if !opts.Signed {
		switch {
		case !storage.IsPublic(s.public, key):
			return "", storage.Wrap("url", key, storage.ErrNotPublic)
		case s.publicURL == "":
			return "", storage.Wrap("url", key, fmt.Errorf("%w: no public URL is configured", storage.ErrUnsupported))
		}
		return s.publicURL + "/" + storage.EscapeKey(full), nil
	}
	req, err := s.presign.PresignGetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(full),
	}, awss3.WithPresignExpires(opts.Expires)) // whole seconds (URLOptions): X-Amz-Expires
	if err != nil {
		return "", storage.Wrap("url", key, classify(ctx, err))
	}
	return req.URL, nil
}

var _ storage.Lister = (*Store)(nil)

// List implements storage.Lister with ListObjectsV2, in key order, a page
// (up to 1000 objects) at a time. ContentType and Metadata are empty: a
// listing does not carry them. Objects under the store's prefix whose keys
// are not valid storage keys (written by something else) are skipped.
func (s *Store) List(ctx context.Context, prefix string, fn func(storage.ObjectInfo) error) error {
	if err := ctx.Err(); err != nil {
		return storage.Wrap("list", prefix, err)
	}
	pages := awss3.NewListObjectsV2Paginator(s.client, &awss3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(s.prefix + prefix),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return storage.Wrap("list", prefix, classify(ctx, err))
		}
		for _, o := range page.Contents {
			key, ok := strings.CutPrefix(aws.ToString(o.Key), s.prefix)
			if !ok || storage.ValidateKey(key) != nil {
				continue
			}
			if err := fn(storage.ObjectInfo{
				Key:     key,
				Size:    aws.ToInt64(o.Size),
				ETag:    strings.Trim(aws.ToString(o.ETag), `"`),
				ModTime: aws.ToTime(o.LastModified),
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

var _ storage.DirectUploader = (*Store)(nil)

var _ storage.UploadVerifier = (*Store)(nil)

// VerifyUpload implements storage.UploadVerifier. A grant signs the length,
// type, metadata and If-None-Match of the PUT; SigV4 leaves other standard
// headers unauthenticated, and S3 keeps five of them with the object and
// serves them back: Cache-Control, Content-Disposition, Content-Encoding,
// Content-Language and Expires. A grant never sets them, so an object that
// has one was given it by the client: VerifyUpload fails with
// storage.ErrInvalidOptions, naming them.
func (s *Store) VerifyUpload(ctx context.Context, key string) error {
	objKey, err := s.objectKey(key)
	if err != nil {
		return storage.Wrap("verify upload", key, err)
	}
	out, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(objKey)})
	if err != nil {
		return storage.Wrap("verify upload", key, classify(ctx, err))
	}
	var set []string
	for name, value := range map[string]*string{
		"Cache-Control":       out.CacheControl,
		"Content-Disposition": out.ContentDisposition,
		"Content-Encoding":    out.ContentEncoding,
		"Content-Language":    out.ContentLanguage,
		"Expires":             out.ExpiresString,
	} {
		if aws.ToString(value) != "" {
			set = append(set, name)
		}
	}
	if len(set) > 0 {
		slices.Sort(set)
		return storage.Wrap("verify upload", key, fmt.Errorf("%w: the upload set %s, which its grant does not allow", storage.ErrInvalidOptions, strings.Join(set, ", ")))
	}
	return nil
}

// UploadURL implements storage.DirectUploader: a presigned PutObject whose
// signature covers the length, the content type, the metadata headers,
// and "If-None-Match: *", so S3 refuses (403) a request that differs in
// any of them, and (412) one to a key that already holds an object. The
// client sends the returned headers; the bucket needs a CORS rule allowing
// PUT from the app's origin for a browser to do so.
func (s *Store) UploadURL(ctx context.Context, key string, opts storage.UploadURLOptions) (storage.UploadRequest, error) {
	full, err := s.objectKey(key)
	if err != nil {
		return storage.UploadRequest{}, storage.Wrap("upload url", key, err)
	}
	if err := storage.ValidateUploadURLOptions(opts); err != nil {
		return storage.UploadRequest{}, storage.Wrap("upload url", key, err)
	}
	if opts.Size > MaxUploadURLBytes {
		// A direct upload is one presigned PutObject, and S3 refuses one
		// over 5 GiB: a grant for more could never succeed.
		return storage.UploadRequest{}, storage.Wrap("upload url", key, fmt.Errorf("%w: a direct upload to S3 is one PutObject, at most %d bytes (5 GiB), not %d", storage.ErrInvalidOptions, MaxUploadURLBytes, opts.Size))
	}
	if err := ctx.Err(); err != nil {
		return storage.UploadRequest{}, storage.Wrap("upload url", key, err)
	}
	contentType := opts.ContentType
	if contentType == "" {
		contentType = storage.DefaultContentType
	}
	// The signature is dated after now (second precision): an expiry
	// counted from now, truncated, is never later than S3's.
	// The credentials the grant is signed with: temporary ones (an IAM
	// role's, STS) end the URL when they expire, whatever its own expiry.
	// Read before signing: if the cache refreshes them in between, the
	// signature carries newer, later-expiring ones, so the Expires
	// reported is never later than the real end.
	var credsExpire time.Time
	if provider := s.client.Options().Credentials; provider != nil {
		creds, err := provider.Retrieve(ctx)
		if err != nil {
			return storage.UploadRequest{}, storage.Wrap("upload url", key, classify(ctx, err))
		}
		if creds.CanExpire {
			credsExpire = creds.Expires
		}
	}
	req, err := s.presign.PresignPutObject(ctx, &awss3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(full),
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(opts.Size),
		Metadata:      encodeMetadata(opts.Metadata),
		// Single use: S3 stores it only where no object is (412
		// otherwise), so an upload checked after it cannot be replaced.
		IfNoneMatch: aws.String("*"),
	}, awss3.WithPresignExpires(opts.Expires)) // whole seconds (URLOptions): X-Amz-Expires
	if err != nil {
		return storage.UploadRequest{}, storage.Wrap("upload url", key, classify(ctx, err))
	}
	expires, err := presignedExpiry(req.URL)
	if err != nil {
		return storage.UploadRequest{}, storage.Wrap("upload url", key, err)
	}
	if !credsExpire.IsZero() && credsExpire.Before(expires) {
		expires = credsExpire // the credentials end first, and the URL with them
	}
	header := map[string]string{}
	for name, values := range req.SignedHeader {
		switch http.CanonicalHeaderKey(name) {
		case "Host", "Content-Length": // set by the client's HTTP library
			continue
		}
		header[http.CanonicalHeaderKey(name)] = strings.Join(values, ",")
	}
	return storage.UploadRequest{
		Method:  req.Method,
		URL:     req.URL,
		Header:  header,
		Expires: expires,
	}, nil
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
