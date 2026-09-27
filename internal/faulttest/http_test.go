package faulttest_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/internal/faulttest"
)

func get(t *testing.T, ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return client.Do(req)
}

func TestHTTPDependencyAnswersItsScriptInOrder(t *testing.T) {
	dep := faulttest.NewHTTPDependency(t,
		faulttest.ServerError(),
		faulttest.TooManyRequests(3*time.Second),
		faulttest.Respond(http.StatusOK, `{"ok":true}`),
	).Otherwise(faulttest.Respond(http.StatusTeapot, ""))
	ctx := context.Background()
	for i, want := range []int{500, 429, 200, 418, 418} {
		resp, err := get(t, ctx, dep.Client(), dep.URL())
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("call %d = %d, want %d", i+1, resp.StatusCode, want)
		}
		if want == 429 && resp.Header.Get("Retry-After") != "3" {
			t.Fatalf("429 Retry-After = %q, want 3", resp.Header.Get("Retry-After"))
		}
		if want == 200 && string(body) != `{"ok":true}` {
			t.Fatalf("200 body = %q", body)
		}
	}
	if dep.Calls() != 5 {
		t.Fatalf("Calls = %d, want 5", dep.Calls())
	}
}

func TestHangIsReleasedOrAbandoned(t *testing.T) {
	release := make(chan struct{})
	dep := faulttest.NewHTTPDependency(t, faulttest.Hang(release), faulttest.Hang(make(chan struct{})))

	done := make(chan error, 1)
	go func() {
		resp, err := get(t, context.Background(), dep.Client(), dep.URL())
		if err == nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()
	<-dep.Reached(1)
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("released call = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_, err := get(t, ctx, dep.Client(), dep.URL())
		done <- err
	}()
	<-dep.Reached(2)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled call = %v, want context.Canceled", err)
	}
	dep.WaitIdle(t)
	if dep.Abandoned() != 1 {
		t.Fatalf("Abandoned = %d, want the canceled call", dep.Abandoned())
	}
}

func TestCutBodyFailsTheRead(t *testing.T) {
	dep := faulttest.NewHTTPDependency(t, faulttest.CutBody(`{"openapi":`))
	resp, err := get(t, context.Background(), dep.Client(), dep.URL())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if !errors.Is(err, io.ErrUnexpectedEOF) || string(body) != `{"openapi":` {
		t.Fatalf("read = %q, %v; want the partial body and io.ErrUnexpectedEOF", body, err)
	}
}

func TestResetConnectionGivesNoResponse(t *testing.T) {
	dep := faulttest.NewHTTPDependency(t, faulttest.ResetConnection())
	resp, err := get(t, context.Background(), dep.Client(), dep.URL())
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a reset connection produced a response")
	}
	if !strings.Contains(err.Error(), "reset") && !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("reset = %v, want a connection reset or EOF", err)
	}
}

func TestCloseReleasesAHangingCall(t *testing.T) {
	dep := faulttest.NewHTTPDependency(t, faulttest.Hang(make(chan struct{})))
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := get(t, context.Background(), dep.Client(), dep.URL()); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-dep.Reached(1)
	dep.Close() // must not wait forever on the hanging handler
	<-done
	if dep.InFlight() != 0 {
		t.Fatalf("InFlight = %d after Close", dep.InFlight())
	}
}

func TestBodyTrackerCountsUnclosedBodies(t *testing.T) {
	dep := faulttest.NewHTTPDependency(t)
	tracker := faulttest.TrackBodies(dep.Client().Transport)
	client := &http.Client{Transport: tracker}
	resp, err := get(t, context.Background(), client, dep.URL())
	if err != nil {
		t.Fatal(err)
	}
	if tracker.Open() != 1 {
		t.Fatalf("Open = %d with a body unread, want 1", tracker.Open())
	}
	_ = resp.Body.Close()
	_ = resp.Body.Close() // closing twice counts once
	if tracker.Open() != 0 {
		t.Fatalf("Open = %d after Close, want 0", tracker.Open())
	}
}

func TestRetryAfterRoundsUp(t *testing.T) {
	dep := faulttest.NewHTTPDependency(t, faulttest.TooManyRequests(500*time.Millisecond))
	resp, err := get(t, context.Background(), dep.Client(), dep.URL())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := resp.Header.Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After for 500ms = %q, want 1 (never 0)", got)
	}
}

// TestCloseIsNotAbandonment: a hanging call ended by Close is not counted
// as the client giving up.
func TestCloseIsNotAbandonment(t *testing.T) {
	dep := faulttest.NewHTTPDependency(t, faulttest.Hang(make(chan struct{})))
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := get(t, context.Background(), dep.Client(), dep.URL()); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-dep.Reached(1)
	dep.Close()
	<-done
	if dep.Abandoned() != 0 {
		t.Fatalf("Abandoned = %d after Close, want 0", dep.Abandoned())
	}
}
