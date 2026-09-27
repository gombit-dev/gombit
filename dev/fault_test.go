package dev

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/internal/faulttest"
)

// Fault tests for the dev server's /openapi.json fetch (client
// regeneration): a failing app server is an error, promptly, with the body
// closed; the watcher logs it and polls again.

func trackSpecClient(t *testing.T, dep *faulttest.HTTPDependency) *faulttest.BodyTracker {
	t.Helper()
	tracker := faulttest.TrackBodies(dep.Client().Transport)
	prev := specHTTPClient
	specHTTPClient = &http.Client{Transport: tracker, Timeout: prev.Timeout}
	t.Cleanup(func() { specHTTPClient = prev })
	return tracker
}

func TestFault_HTTP_DevSpecFetch(t *testing.T) {
	for _, tc := range []struct {
		name string
		step faulttest.HTTPStep
		want string
	}{
		{"server error", faulttest.ServerError(), "status 500"},
		{"too many requests", faulttest.TooManyRequests(time.Second), "status 429"},
		{"connection reset", faulttest.ResetConnection(), ""},
		{"body cut", faulttest.CutBody(`{"openapi":`), "unexpected EOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dep := faulttest.NewHTTPDependency(t, tc.step)
			tracker := trackSpecClient(t, dep)
			body, err := defaultHTTPGet(context.Background(), dep.URL())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("fetch = %q, %v; want an error mentioning %q", body, err, tc.want)
			}
			if tracker.Open() != 0 {
				t.Fatalf("%d response bodies left open", tracker.Open())
			}
			dep.WaitIdle(t)
		})
	}
}

// TestFault_HTTP_DevSpecFetchTimeout: a hung app server ends the fetch at
// the caller's deadline, and the call does not linger on the server.
func TestFault_HTTP_DevSpecFetchTimeout(t *testing.T) {
	dep := faulttest.NewHTTPDependency(t, faulttest.Hang(make(chan struct{})))
	tracker := trackSpecClient(t, dep)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := defaultHTTPGet(ctx, dep.URL()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fetch = %v, want context.DeadlineExceeded", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("fetch took %s, want it to end at the 100ms deadline", took)
	}
	if tracker.Open() != 0 {
		t.Fatalf("%d response bodies left open", tracker.Open())
	}
	dep.WaitIdle(t)
	if dep.Abandoned() != 1 {
		t.Fatalf("abandoned = %d, want 1", dep.Abandoned())
	}
}
