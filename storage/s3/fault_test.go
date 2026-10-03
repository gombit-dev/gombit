package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/gombit-dev/gombit/storage"
)

// fakeS3 is just enough of the S3 API for the driver's failure paths: it
// can apply a request and then drop the connection without answering (a
// lost acknowledgement), refuse a request, or answer slowly.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string]int64 // key -> size
	uploads map[string]bool  // live multipart upload ids
	nextID  int

	bucketMissing bool
	bucketStatus  int // HeadBucket answers this status when set
	bucketDelay   time.Duration
	headBucket    atomic.Int32

	dropPut          bool   // apply a PutObject, then drop the connection
	refusePut        int    // answer a PutObject with this status, applying nothing
	dropComplete     bool   // apply a Complete, then drop the connection
	dropBeforeApply  bool   // drop a Complete without applying it
	refusePart       int    // answer an UploadPart with this status
	refuseAbort      int    // answer an Abort with this status
	goneBeforeAbort  bool   // the upload is gone by the time it is aborted
	refuseComplete   int    // answer a CompleteMultipartUpload with this status, applying nothing
	completeCode     string // and this error code
	dropPart         bool   // never answer an UploadPart; S3 stores it once the upload is aborted
	pendingParts     int    // parts received, still being processed
	lateParts        int    // parts stored after their upload was aborted
	requestsSeen     int    // every request
	abortedUploadIDs []string
	puts, completes  int                 // PutObject and CompleteMultipartUpload requests seen
	partCopies       int                 // UploadPartCopy requests seen
	copyRanges       []string            // their CopySourceRange headers
	dropPartCopy     bool                // never answer an UploadPartCopy; S3 stores it once the upload is aborted
	partsOf          map[string][]string // live upload id -> part ETags
	ghosts           map[string]int      // aborted upload id -> parts stored after the abort
	pendingCopies    map[string]int      // upload id -> part copies still being processed
	aborts           int                 // AbortMultipartUpload requests seen
	stickyGhosts     int                 // aborts that leave a ghost's parts listed (AWS: abort may need repeating)
	manifest         []string            // the ETags the last Complete was sent
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string]int64{}, uploads: map[string]bool{}, partsOf: map[string][]string{}, ghosts: map[string]int{}, pendingCopies: map[string]int{}}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	q := r.URL.Query()
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requestsSeen++
	switch {
	case r.Method == http.MethodHead && key == "": // HeadBucket
		f.headBucket.Add(1)
		f.mu.Unlock()
		time.Sleep(f.bucketDelay)
		f.mu.Lock()
		switch {
		case f.bucketStatus != 0:
			w.WriteHeader(f.bucketStatus)
		case f.bucketMissing:
			w.WriteHeader(http.StatusNotFound)
		}
	case r.Method == http.MethodHead: // HeadObject
		size, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(size))
	case r.Method == http.MethodPut && q.Has("partNumber") && r.Header.Get("X-Amz-Copy-Source") != "": // UploadPartCopy
		f.partCopies++
		f.copyRanges = append(f.copyRanges, r.Header.Get("X-Amz-Copy-Source-Range"))
		id := q.Get("uploadId")
		if f.dropPartCopy {
			f.pendingCopies[id]++
			drop(w)
			return
		}
		n, _ := strconv.Atoi(q.Get("partNumber"))
		etag := fmt.Sprintf(`"part-%d"`, n)
		f.partsOf[id] = append(f.partsOf[id], etag)
		_, _ = fmt.Fprintf(w, `<CopyPartResult><ETag>"part-%d"</ETag></CopyPartResult>`, n)
	case r.Method == http.MethodGet && q.Has("uploadId"): // ListParts
		id := q.Get("uploadId")
		n := len(f.partsOf[id])
		if !f.uploads[id] {
			if n = f.ghosts[id]; n == 0 {
				s3Error(w, http.StatusNotFound, "NoSuchUpload")
				return
			}
		}
		var b strings.Builder
		for i := 1; i <= n; i++ {
			fmt.Fprintf(&b, `<Part><PartNumber>%d</PartNumber><ETag>"part-%d"</ETag><Size>5</Size></Part>`, i, i)
		}
		_, _ = fmt.Fprintf(w, `<ListPartsResult>%s<IsTruncated>false</IsTruncated></ListPartsResult>`, b.String())
	case r.Method == http.MethodPut && q.Has("partNumber"): // UploadPart
		if f.dropPart {
			// Still processing when the client gives up: it lands after the
			// abort (AWS documents that an in-progress part may).
			f.pendingParts++
			drop(w)
			return
		}
		if f.refusePart != 0 {
			s3Error(w, f.refusePart, "InvalidRequest")
			return
		}
		w.Header().Set("ETag", `"part"`)
	case r.Method == http.MethodPut: // PutObject
		f.puts++
		if f.refusePut != 0 {
			s3Error(w, f.refusePut, "AccessDenied")
			return
		}
		f.objects[key] = int64(len(body))
		if f.dropPut {
			drop(w)
			return
		}
		w.Header().Set("ETag", `"etag"`)
	case r.Method == http.MethodPost && q.Has("uploads"): // CreateMultipartUpload
		f.nextID++
		id := fmt.Sprintf("upload-%d", f.nextID)
		f.uploads[id] = true
		_, _ = fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>b</Bucket><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, id)
	case r.Method == http.MethodPost && q.Has("uploadId"): // CompleteMultipartUpload
		f.completes++
		if !f.uploads[q.Get("uploadId")] {
			// Already completed (or aborted): what S3 answers a retry of a
			// Complete whose first attempt took effect.
			s3Error(w, http.StatusNotFound, "NoSuchUpload")
			return
		}
		if f.refuseComplete != 0 {
			s3Error(w, f.refuseComplete, f.completeCode)
			return
		}
		if f.dropBeforeApply {
			drop(w)
			return
		}
		f.manifest = f.manifest[:0]
		for _, m := range strings.Split(string(body), "<ETag>")[1:] {
			f.manifest = append(f.manifest, strings.NewReplacer("&#34;", `"`, "&quot;", `"`).Replace(strings.SplitN(m, "</ETag>", 2)[0]))
		}
		delete(f.uploads, q.Get("uploadId"))
		f.objects[key] = -1 // assembled
		if f.dropComplete {
			drop(w)
			return
		}
		_, _ = io.WriteString(w, `<CompleteMultipartUploadResult><Bucket>b</Bucket><ETag>"done"</ETag></CompleteMultipartUploadResult>`)
	case r.Method == http.MethodDelete && q.Has("uploadId"): // AbortMultipartUpload
		f.aborts++
		if id := q.Get("uploadId"); !f.uploads[id] && f.ghosts[id] > 0 {
			// Parts stored after an earlier abort: this one frees them, or
			// (stickyGhosts) not yet.
			if f.stickyGhosts > 0 {
				f.stickyGhosts--
			} else {
				delete(f.ghosts, id)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if f.refuseAbort != 0 {
			s3Error(w, f.refuseAbort, "AccessDenied")
			return
		}
		id := q.Get("uploadId")
		if f.goneBeforeAbort {
			delete(f.uploads, id)
		}
		if !f.uploads[id] {
			s3Error(w, http.StatusNotFound, "NoSuchUpload")
			return
		}
		delete(f.uploads, id)
		delete(f.partsOf, id)
		if f.pendingCopies[id] > 0 {
			// A part copy still being processed lands after the abort.
			f.ghosts[id], f.pendingCopies[id] = f.pendingCopies[id], 0
		}
		f.abortedUploadIDs = append(f.abortedUploadIDs, id)
		f.lateParts, f.pendingParts = f.pendingParts, 0
		w.WriteHeader(http.StatusNoContent)
	default:
		s3Error(w, http.StatusNotImplemented, "NotImplemented")
	}
}

func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}

// drop closes the connection without an answer: the request was applied,
// and the client never hears so.
func drop(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

// requests returns the PutObject and CompleteMultipartUpload counts.
func (f *fakeS3) requests() (puts, completes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts, f.completes
}

func (f *fakeS3) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok
}

// fakeStore is a Store on f, configured as in production: the SDK's
// default retryer (three attempts) included.
func fakeStore(t *testing.T, f *fakeS3) *Store {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	s, err := New(context.Background(), Config{Endpoint: srv.URL, Region: "us-east-1", Bucket: "b", AccessKeyID: "id", SecretAccessKey: "secret", ForcePathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// large is a multipart-sized object.
func large() io.Reader { return io.LimitReader(zeroReader{}, PartSize+1) }

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// TestLostPutAnswerIsUnknownOutcome: a PutObject that S3 applied but never
// answered is not reported as a failure that left the key as it was.
func TestLostPutAnswerIsUnknownOutcome(t *testing.T) {
	f := newFakeS3()
	f.dropPut = true
	_, err := fakeStore(t, f).Put(context.Background(), "k", strings.NewReader("new"), storage.PutOptions{})
	if !errors.Is(err, storage.ErrUnknownOutcome) {
		t.Fatalf("Put whose answer was lost = %v, want ErrUnknownOutcome", err)
	}
	if !f.has("k") {
		t.Fatal("the fake did not apply the Put: the test proves nothing")
	}
}

// TestRefusedPutLeavesTheKey: a PutObject S3 answered with a refusal is a
// definite failure, not an unknown outcome.
func TestRefusedPutLeavesTheKey(t *testing.T) {
	f := newFakeS3()
	f.refusePut = http.StatusForbidden
	_, err := fakeStore(t, f).Put(context.Background(), "k", strings.NewReader("new"), storage.PutOptions{})
	if err == nil || errors.Is(err, storage.ErrUnknownOutcome) {
		t.Fatalf("Put refused with 403 = %v, want a definite failure", err)
	}
}

// TestLostCompleteAnswer: a CompleteMultipartUpload whose answer was lost
// is settled by the abort. S3 had applied it (the abort finds no upload):
// the outcome is unknown. S3 had not (the abort succeeds): the Put failed
// and left the key as it was.
func TestLostCompleteAnswer(t *testing.T) {
	t.Run("applied", func(t *testing.T) {
		f := newFakeS3()
		f.dropComplete = true
		_, err := fakeStore(t, f).Put(context.Background(), "k", large(), storage.PutOptions{})
		if !errors.Is(err, storage.ErrUnknownOutcome) {
			t.Fatalf("Put whose Complete was applied but not answered = %v, want ErrUnknownOutcome", err)
		}
		if !f.has("k") {
			t.Fatal("the fake did not complete the upload")
		}
	})
	t.Run("not applied", func(t *testing.T) {
		f := newFakeS3()
		f.dropBeforeApply = true
		_, err := fakeStore(t, f).Put(context.Background(), "k", large(), storage.PutOptions{})
		if err == nil || errors.Is(err, storage.ErrUnknownOutcome) {
			t.Fatalf("Put whose Complete was lost before S3 applied it = %v, want a definite failure (the abort proved it)", err)
		}
		f.mu.Lock()
		aborted := len(f.abortedUploadIDs)
		f.mu.Unlock()
		if f.has("k") || aborted != 1 {
			t.Fatalf("object stored = %v, %d uploads aborted; want none stored, one aborted", f.has("k"), aborted)
		}
	})
}

// TestPublishingIsNotRetried: with the SDK's default retries, a publishing
// request whose answer is lost is not sent again. A retry's answer says
// nothing about the first attempt: a CompleteMultipartUpload that took
// effect and lost its answer makes its retry fail with NoSuchUpload, which
// would read as a refusal (and the settled abort as parts left behind)
// although the object was published.
func TestPublishingIsNotRetried(t *testing.T) {
	t.Run("complete", func(t *testing.T) {
		f := newFakeS3()
		f.dropComplete = true
		_, err := fakeStore(t, f).Put(context.Background(), "k", large(), storage.PutOptions{})
		var abort *AbortError
		if !errors.Is(err, storage.ErrUnknownOutcome) || errors.As(err, &abort) {
			t.Fatalf("Put whose first Complete took effect and lost its answer = %v; want ErrUnknownOutcome and no *AbortError", err)
		}
		if _, completes := f.requests(); completes != 1 || !f.has("k") {
			t.Fatalf("%d Complete requests (object stored: %v), want 1", completes, f.has("k"))
		}
	})
	t.Run("put", func(t *testing.T) {
		f := newFakeS3()
		f.dropPut = true
		_, err := fakeStore(t, f).Put(context.Background(), "k", strings.NewReader("new"), storage.PutOptions{})
		if puts, _ := f.requests(); !errors.Is(err, storage.ErrUnknownOutcome) || puts != 1 {
			t.Fatalf("Put = %v after %d PutObject requests, want ErrUnknownOutcome after 1", err, puts)
		}
	})
}

// TestAbortOfAGoneUploadLeavesNothing: an abort that finds the upload gone
// (a lifecycle rule removed it, say) is not reported as parts left behind.
func TestAbortOfAGoneUploadLeavesNothing(t *testing.T) {
	f := newFakeS3()
	f.refusePart = http.StatusBadRequest
	s := fakeStore(t, f)
	f.mu.Lock()
	f.goneBeforeAbort = true
	f.mu.Unlock()
	_, err := s.Put(context.Background(), "k", large(), storage.PutOptions{})
	var abort *AbortError
	if err == nil || errors.As(err, &abort) || errors.Is(err, storage.ErrUnknownOutcome) {
		t.Fatalf("Put = %v; want the part's failure, without an *AbortError (no upload remains)", err)
	}
}

// failingCredentials never yields credentials, as a default credential
// chain with nothing configured (or a failing refresh) does.
type failingCredentials struct{}

func (failingCredentials) Retrieve(context.Context) (aws.Credentials, error) {
	return aws.Credentials{}, errors.New("no credentials")
}

// TestNothingSentIsNotUnknown: a Put that fails before any request is
// written (the credentials cannot be had, so nothing is signed; or the
// endpoint refuses the connection) left the key as it was: it is not
// ErrUnknownOutcome.
func TestNothingSentIsNotUnknown(t *testing.T) {
	t.Run("credentials", func(t *testing.T) {
		f := newFakeS3()
		s := fakeStore(t, f)
		s.client = awss3.New(s.client.Options(), func(o *awss3.Options) { o.Credentials = failingCredentials{} })
		for _, body := range []io.Reader{strings.NewReader("small"), large()} {
			_, err := s.Put(context.Background(), "k", body, storage.PutOptions{})
			if err == nil || errors.Is(err, storage.ErrUnknownOutcome) {
				t.Fatalf("Put without credentials = %v, want a definite failure", err)
			}
		}
		f.mu.Lock()
		seen := f.requestsSeen
		f.mu.Unlock()
		if seen != 0 {
			t.Fatalf("the fake saw %d requests, want none: nothing could be signed", seen)
		}
	})
	t.Run("connection refused", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		endpoint := srv.URL
		srv.Close() // nothing listens there now
		s, err := New(context.Background(), Config{Endpoint: endpoint, Region: "us-east-1", Bucket: "b", AccessKeyID: "id", SecretAccessKey: "secret", ForcePathStyle: true})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Put(context.Background(), "k", strings.NewReader("small"), storage.PutOptions{})
		if !errors.Is(err, storage.ErrUnavailable) || errors.Is(err, storage.ErrUnknownOutcome) {
			t.Fatalf("Put to a refused connection = %v, want ErrUnavailable and not ErrUnknownOutcome", err)
		}
	})
}

// TestPartInFlightAtAbortIsReported: a part upload that never got an
// answer may be stored by S3 after the upload is aborted, so even a
// successful abort cannot vouch that no parts remain; the error says so
// (*AbortError), and the fake shows the part arriving late.
func TestPartInFlightAtAbortIsReported(t *testing.T) {
	f := newFakeS3()
	f.dropPart = true
	_, err := fakeStore(t, f).Put(context.Background(), "k", large(), storage.PutOptions{})
	var abort *AbortError
	if !errors.As(err, &abort) || abort.UploadID != "upload-1" {
		t.Fatalf("Put = %v, want an *AbortError for upload-1: a part was in flight when it was aborted", err)
	}
	if errors.Is(err, storage.ErrUnknownOutcome) {
		t.Fatalf("Put = %v: nothing was published, the outcome is known", err)
	}
	f.mu.Lock()
	late, aborted := f.lateParts, len(f.abortedUploadIDs)
	f.mu.Unlock()
	if aborted != 1 || late == 0 {
		t.Fatalf("%d aborts, %d parts stored after the abort; the test models nothing", aborted, late)
	}
}

// TestFailedAbortIsReported: when an upload fails and its abort fails too,
// the error names the upload left behind (*AbortError), and the failure is
// still classified by its own cause.
func TestFailedAbortIsReported(t *testing.T) {
	f := newFakeS3()
	f.refusePart = http.StatusBadRequest
	f.refuseAbort = http.StatusForbidden
	_, err := fakeStore(t, f).Put(context.Background(), "k", large(), storage.PutOptions{})
	var abort *AbortError
	if !errors.As(err, &abort) || abort.UploadID != "upload-1" || abort.Key != "k" {
		t.Fatalf("Put = %v, want an *AbortError for upload-1 of k", err)
	}
	if errors.Is(err, storage.ErrUnavailable) || errors.Is(err, storage.ErrUnknownOutcome) {
		t.Fatalf("Put = %v: the abort's failure changed how the upload's failure is classified", err)
	}
	if !strings.Contains(err.Error(), "upload-1") || !strings.Contains(err.Error(), "lifecycle") {
		t.Fatalf("Put's error %q does not say which upload was left behind", err)
	}
}

// TestBucketCheckIsSingleFlight: concurrent misses on a cold store send one
// HeadBucket, and its answer (the bucket exists, or does not) is kept for
// bucketRecheck; a transient failure is not kept.
func TestBucketCheckIsSingleFlight(t *testing.T) {
	stats := func(t *testing.T, s *Store, n int) []error {
		t.Helper()
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = s.Stat(context.Background(), fmt.Sprintf("missing-%d", i))
			}()
		}
		wg.Wait()
		return errs
	}
	t.Run("bucket exists", func(t *testing.T) {
		f := newFakeS3()
		f.bucketDelay = 100 * time.Millisecond
		s := fakeStore(t, f)
		for round := 0; round < 2; round++ {
			for _, err := range stats(t, s, 50) {
				if !errors.Is(err, storage.ErrNotFound) {
					t.Fatalf("Stat of a missing object = %v, want ErrNotFound", err)
				}
			}
		}
		if n := f.headBucket.Load(); n != 1 {
			t.Fatalf("100 misses sent %d HeadBucket requests, want 1", n)
		}
	})
	t.Run("bucket missing", func(t *testing.T) {
		f := newFakeS3()
		f.bucketMissing = true
		f.bucketDelay = 100 * time.Millisecond
		s := fakeStore(t, f)
		for round := 0; round < 2; round++ {
			for _, err := range stats(t, s, 50) {
				if err == nil || errors.Is(err, storage.ErrNotFound) || !strings.Contains(err.Error(), "does not exist") {
					t.Fatalf("Stat in a missing bucket = %v, want the configuration error", err)
				}
			}
		}
		if n := f.headBucket.Load(); n != 1 {
			t.Fatalf("100 misses in a missing bucket sent %d HeadBucket requests, want 1", n)
		}
	})
	t.Run("transient failure is not kept", func(t *testing.T) {
		// One attempt per HeadBucket, so the count below is of checks, not
		// of the SDK's retries of each.
		t.Setenv("AWS_MAX_ATTEMPTS", "1")
		f := newFakeS3()
		f.bucketStatus = http.StatusServiceUnavailable
		s := fakeStore(t, f)
		for i := 0; i < 2; i++ {
			if _, err := s.Stat(context.Background(), "missing"); !errors.Is(err, storage.ErrUnavailable) {
				t.Fatalf("Stat while HeadBucket fails = %v, want ErrUnavailable", err)
			}
		}
		if n := f.headBucket.Load(); n != 2 {
			t.Fatalf("two misses during an outage sent %d HeadBucket requests, want 2 (a transient failure is not cached)", n)
		}
	})
}

// TestBucketCheckHonorsEveryCallersContext: a caller whose ctx ends stops
// waiting for the HeadBucket in flight, whether it started it or not; the
// check still completes for the others.
func TestBucketCheckHonorsEveryCallersContext(t *testing.T) {
	f := newFakeS3()
	f.bucketDelay = 300 * time.Millisecond
	s := fakeStore(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.Stat(ctx, "missing") // starts the probe
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stat with a 50ms deadline = %v, want its deadline", err)
	}
	if waited := time.Since(start); waited > 200*time.Millisecond {
		t.Fatalf("the caller that started the probe waited %v past its deadline", waited)
	}
	if _, err := s.Stat(context.Background(), "missing"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a later Stat = %v, want ErrNotFound", err)
	}
	if n := f.headBucket.Load(); n != 1 {
		t.Fatalf("%d HeadBucket requests, want 1 (the later Stat joins the probe in flight)", n)
	}
}

// TestConditionalRefusalKeepsTheAbortError: a multipart IfAbsent Put whose
// CompleteMultipartUpload S3 refuses (412: an object is there; 409: a
// conflicting conditional write) and whose abort then fails reports both:
// the refusal's classification (ErrExists, ErrUnavailable) and the
// *AbortError naming the upload whose parts may remain.
func TestConditionalRefusalKeepsTheAbortError(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		code   string
		want   error
	}{
		"412": {http.StatusPreconditionFailed, "PreconditionFailed", storage.ErrExists},
		"409": {http.StatusConflict, "ConditionalRequestConflict", storage.ErrUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeS3()
			f.refuseComplete, f.completeCode = tc.status, tc.code
			f.refuseAbort = http.StatusForbidden
			_, err := fakeStore(t, f).Put(context.Background(), "k", large(), storage.PutOptions{IfAbsent: true})
			var abort *AbortError
			if !errors.Is(err, tc.want) || !errors.As(err, &abort) || abort.UploadID != "upload-1" {
				t.Fatalf("Put = %v; want %v and an *AbortError for upload-1", err, tc.want)
			}
		})
	}
}

// prepared puts src and prepares its publication to dst.
func prepared(t *testing.T, f *fakeS3) (*Store, string) {
	t.Helper()
	s := fakeStore(t, f)
	if _, err := s.Put(context.Background(), "src", strings.NewReader("bytes"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	token, err := s.PreparePublish(context.Background(), "src", "dst")
	if err != nil {
		t.Fatal(err)
	}
	if f.has("dst") || f.partCopies != 1 {
		t.Fatalf("PreparePublish: dst exists = %v after %d part copies; want nothing published, one copy", f.has("dst"), f.partCopies)
	}
	return s, token
}

// TestLostPublishAnswerIsSettledByFence: a Complete that S3 applied but
// never answered is ErrUnknownOutcome, sent once; Fence then proves it is
// over (the abort finds no upload), and nothing more can be published.
func TestLostPublishAnswerIsSettledByFence(t *testing.T) {
	f := newFakeS3()
	s, token := prepared(t, f)
	f.dropComplete = true
	if _, err := s.Publish(context.Background(), token); !errors.Is(err, storage.ErrUnknownOutcome) {
		t.Fatalf("Publish whose answer was lost = %v, want ErrUnknownOutcome", err)
	}
	if _, completes := f.requests(); !f.has("dst") || completes != 1 {
		t.Fatalf("published = %v after %d Completes; want it applied, once", f.has("dst"), completes)
	}
	if err := s.Fence(context.Background(), token); err != nil {
		t.Fatalf("Fence after a completed upload = %v, want nil (NoSuchUpload: it is over)", err)
	}
}

// TestFenceStopsAPublication: a Complete lost before S3 applied it is
// ErrUnknownOutcome too; Fence aborts the upload, after which it can never
// complete: a late Complete (here, a replay) fails, and nothing appears.
func TestFenceStopsAPublication(t *testing.T) {
	f := newFakeS3()
	s, token := prepared(t, f)
	f.dropBeforeApply = true
	if _, err := s.Publish(context.Background(), token); !errors.Is(err, storage.ErrUnknownOutcome) {
		t.Fatalf("Publish whose request was lost = %v, want ErrUnknownOutcome", err)
	}
	f.mu.Lock()
	f.dropBeforeApply = false
	f.mu.Unlock()
	if err := s.Fence(context.Background(), token); err != nil {
		t.Fatalf("Fence = %v", err)
	}
	if _, err := s.Publish(context.Background(), token); err == nil || f.has("dst") {
		t.Fatalf("Publish after the fence = %v (published %v); want it refused, nothing published", err, f.has("dst"))
	}
	if err := s.Fence(context.Background(), token); err != nil {
		t.Fatalf("Fence again = %v; want it idempotent", err)
	}
}

// TestFailedFenceProvesNothing: an abort S3 refused proves nothing, and
// Fence says so.
func TestFailedFenceProvesNothing(t *testing.T) {
	f := newFakeS3()
	s, token := prepared(t, f)
	f.refuseAbort = http.StatusInternalServerError
	if err := s.Fence(context.Background(), token); err == nil {
		t.Fatal("Fence whose abort failed = nil, want an error")
	}
	if err := s.Fence(context.Background(), "not a token"); err == nil {
		t.Fatal("Fence of a malformed token = nil")
	}
}

// TestManifestIsWhatPreparationRecorded: a source over 5 GiB is copied in
// ranged parts, and Complete is sent exactly the parts and ETags S3
// returned when they were copied, in order (never a listing).
func TestManifestIsWhatPreparationRecorded(t *testing.T) {
	f := newFakeS3()
	s := fakeStore(t, f)
	const size = 6<<30 + 7
	f.mu.Lock()
	f.objects["big"] = size
	f.mu.Unlock()
	token, err := s.PreparePublish(context.Background(), "big", "dst")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bytes=0-1073741823", "bytes=1073741824-2147483647", "bytes=2147483648-3221225471", "bytes=3221225472-4294967295", "bytes=4294967296-5368709119", "bytes=5368709120-6442450943", "bytes=6442450944-6442450950"}
	if fmt.Sprint(f.copyRanges) != fmt.Sprint(want) {
		t.Fatalf("copied ranges %v, want %v", f.copyRanges, want)
	}
	if _, err := s.Publish(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if len(f.manifest) != len(want) || f.manifest[0] != `"part-1"` || f.manifest[len(want)-1] != fmt.Sprintf(`"part-%d"`, len(want)) {
		t.Fatalf("Complete was sent %v; want the %d parts as copied", f.manifest, len(want))
	}
}

// TestCopyPartsStayWithinTheToken: however large the object, the parts
// number at most maxCopyParts, each within S3's limit.
func TestCopyPartsStayWithinTheToken(t *testing.T) {
	for _, size := range []int64{1, maxCopyPart, maxCopyPart + 1, 2 << 40, 5 << 40} {
		part, err := copyParts(size)
		if err != nil {
			t.Fatalf("copyParts(%d) = %v", size, err)
		}
		if part == 0 {
			if size > maxCopyPart {
				t.Fatalf("copyParts(%d): one part over S3's limit", size)
			}
			continue
		}
		if part > maxCopyPart || (size+part-1)/part > maxCopyParts {
			t.Fatalf("copyParts(%d) = %d: %d parts", size, part, (size+part-1)/part)
		}
	}
	// A token with the most parts, and the most metadata, still fits.
	etags := make([]string, maxCopyParts)
	for i := range etags {
		etags[i] = `"0123456789abcdef0123456789abcdef"`
	}
	md := map[string]string{"filename": strings.Repeat("é", storage.MaxMetadataBytes/2)}
	if _, err := (publication{Key: strings.Repeat("k", 1024), UploadID: strings.Repeat("u", 300), ETags: etags, Metadata: md}).token(); err != nil {
		t.Fatalf("the largest token = %v", err)
	}
	if _, err := copyParts(maxCopyParts*maxCopyPart + 1); err == nil {
		t.Fatal("an object beyond maxCopyParts parts of maxCopyPart was accepted")
	}
}

// TestLostPartCopyIsFencedUntilNoPartRemains: a part copy whose answer was
// lost may store its part after the abort. Preparing then fails with the
// token, and Fence aborts again until the upload lists no part.
func TestLostPartCopyIsFencedUntilNoPartRemains(t *testing.T) {
	f := newFakeS3()
	s := fakeStore(t, f)
	if _, err := s.Put(context.Background(), "src", strings.NewReader("bytes"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	f.dropPartCopy = true
	token, err := s.PreparePublish(context.Background(), "src", "dst")
	var abort *AbortError
	if err == nil || token == "" || !errors.As(err, &abort) {
		t.Fatalf("PreparePublish with a lost part copy = %q, %v; want the token and an *AbortError", token, err)
	}
	f.mu.Lock()
	ghosts := len(f.ghosts)
	f.mu.Unlock()
	if ghosts != 1 {
		t.Fatal("the fake stored no part after the abort: the test proves nothing")
	}
	f.mu.Lock()
	f.stickyGhosts = 1 // the first abort of the fence does not free them yet
	f.mu.Unlock()
	if err := s.Fence(context.Background(), token); err != nil {
		t.Fatalf("Fence = %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ghosts) != 0 || f.aborts < 3 {
		t.Fatalf("after Fence: %d uploads with parts, %d aborts; want none left", len(f.ghosts), f.aborts)
	}
}
