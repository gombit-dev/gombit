package faulttest

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// HTTPStep is how the dependency answers one call.
type HTTPStep struct {
	name    string
	respond func(w http.ResponseWriter, r *http.Request, done <-chan struct{})
}

func (s HTTPStep) String() string { return s.name }

// Respond answers with status and body (Content-Type application/json).
func Respond(status int, body string) HTTPStep {
	return HTTPStep{name: fmt.Sprintf("respond %d", status), respond: func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}}
}

// ServerError answers 500.
func ServerError() HTTPStep {
	return Respond(http.StatusInternalServerError, `{"error":{"code":"internal","message":"dependency failed"}}`)
}

// TooManyRequests answers 429 with Retry-After, which HTTP gives in whole
// seconds: retryAfter is rounded up, so a sub-second wait is 1, never 0
// ("retry now").
func TooManyRequests(retryAfter time.Duration) HTTPStep {
	secs := int((retryAfter + time.Second - 1) / time.Second)
	return HTTPStep{name: "respond 429", respond: func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		w.WriteHeader(http.StatusTooManyRequests)
	}}
}

// Hang holds the call without answering until release is closed (then
// answers 200 with an empty JSON object), the client goes away, or the
// dependency is closed: a dependency slower than any deadline, driven by a
// channel rather than a sleep.
func Hang(release <-chan struct{}) HTTPStep {
	return HTTPStep{name: "hang", respond: func(w http.ResponseWriter, r *http.Request, done <-chan struct{}) {
		select {
		case <-release:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		case <-r.Context().Done():
		case <-done:
		}
	}}
}

// CutBody sends the status line, headers promising a longer body, and
// partial of it, then closes the connection: the client's read fails
// mid-body.
func CutBody(partial string) HTTPStep {
	return HTTPStep{name: "cut body", respond: func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			hijackFailed(w, "CutBody", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(partial)+1024, partial)
		_ = buf.Flush()
	}}
}

// ResetConnection closes the connection with a TCP reset before answering:
// the client sees "connection reset by peer" (or an EOF), never a response.
func ResetConnection() HTTPStep {
	return HTTPStep{name: "reset", respond: func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			hijackFailed(w, "ResetConnection", err)
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0) // close with RST, not FIN
		}
		_ = conn.Close()
	}}
}

// hijackFailed answers a step that needs the raw connection (HTTP/1.1)
// with a 501 naming the problem, so a test sees a clear setup error rather
// than a reset caused by a recovered panic.
func hijackFailed(w http.ResponseWriter, step string, err error) {
	http.Error(w, fmt.Sprintf("faulttest: %s needs an HTTP/1.1 connection: %v", step, err), http.StatusNotImplemented)
}

// HTTPDependency is a loopback HTTP server standing in for a dependency.
// Each call gets the next scripted step; calls after the script get the
// fallback (200 with an empty JSON object unless set with Otherwise). It
// counts calls, reports when a call arrives (Reached), and when a handler
// returns (InFlight), so a test can prove none is left running.
type HTTPDependency struct {
	srv      *httptest.Server
	inj      *Injector // numbers the calls
	done     chan struct{}
	mu       sync.Mutex
	steps    []HTTPStep
	fallback HTTPStep
	inFlight int
	gone     int // calls whose client went away before an answer
}

// NewHTTPDependency starts a dependency answering with steps, one per call,
// and closes it when t ends.
func NewHTTPDependency(t testing.TB, steps ...HTTPStep) *HTTPDependency {
	t.Helper()
	d := &HTTPDependency{inj: Sequence(), done: make(chan struct{}), steps: steps, fallback: Respond(http.StatusOK, `{}`)}
	d.srv = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.Close)
	return d
}

// Otherwise sets the answer for calls past the script.
func (d *HTTPDependency) Otherwise(step HTTPStep) *HTTPDependency {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fallback = step
	return d
}

func (d *HTTPDependency) serve(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.inFlight++
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.inFlight--
		if r.Context().Err() != nil && !d.closing() {
			d.gone++ // the client went away, not the dependency
		}
		d.mu.Unlock()
	}()
	n, _ := d.inj.hit(r.Context()) // numbers the call; Sequence() never fails
	d.mu.Lock()
	step := d.fallback
	if n <= len(d.steps) {
		step = d.steps[n-1]
	}
	d.mu.Unlock()
	step.respond(w, r, d.done)
}

// closing reports whether Close has begun.
func (d *HTTPDependency) closing() bool {
	select {
	case <-d.done:
		return true
	default:
		return false
	}
}

// URL is the dependency's base URL.
func (d *HTTPDependency) URL() string { return d.srv.URL }

// Client is an HTTP client for the dependency (loopback only).
func (d *HTTPDependency) Client() *http.Client { return d.srv.Client() }

// Calls reports how many calls reached the dependency.
func (d *HTTPDependency) Calls() int { return d.inj.Calls() }

// Reached returns a channel closed once call n has arrived.
func (d *HTTPDependency) Reached(n int) <-chan struct{} { return d.inj.Reached(n) }

// InFlight reports how many calls are being answered now.
func (d *HTTPDependency) InFlight() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.inFlight
}

// Abandoned reports how many calls the client gave up on (canceled, timed
// out, disconnected) before they were answered; calls ended by Close do not
// count.
func (d *HTTPDependency) Abandoned() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.gone
}

// WaitIdle waits until no call is being answered (the server notices a
// client that went away on its own goroutine), and fails t if one still is
// after 5s: a handler left running is a leak.
func (d *HTTPDependency) WaitIdle(t testing.TB) {
	t.Helper()
	deadline := time.Now().Add(idleTimeout)
	for d.InFlight() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d dependency call(s) still being answered", d.InFlight())
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// Close releases any hanging call and shuts the dependency down, waiting
// for its handlers to return.
func (d *HTTPDependency) Close() {
	d.mu.Lock()
	select {
	case <-d.done:
		d.mu.Unlock()
		return
	default:
		close(d.done)
	}
	d.mu.Unlock()
	d.srv.CloseClientConnections()
	d.srv.Close()
}

// BodyTracker is an http.RoundTripper that counts response bodies opened
// and not yet closed, to prove a client closes every body, failures
// included.
type BodyTracker struct {
	base http.RoundTripper
	mu   sync.Mutex
	open int
}

// TrackBodies wraps base (http.DefaultTransport when nil).
func TrackBodies(base http.RoundTripper) *BodyTracker {
	if base == nil {
		base = http.DefaultTransport
	}
	return &BodyTracker{base: base}
}

// RoundTrip implements http.RoundTripper.
func (b *BodyTracker) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := b.base.RoundTrip(r)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	b.mu.Lock()
	b.open++
	b.mu.Unlock()
	resp.Body = &trackedBody{ReadCloser: resp.Body, tracker: b}
	return resp, nil
}

// Open reports the bodies returned and not closed.
func (b *BodyTracker) Open() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.open
}

type trackedBody struct {
	io.ReadCloser
	tracker *BodyTracker
	once    sync.Once
}

func (t *trackedBody) Close() error {
	t.once.Do(func() {
		t.tracker.mu.Lock()
		t.tracker.open--
		t.tracker.mu.Unlock()
	})
	return t.ReadCloser.Close()
}
