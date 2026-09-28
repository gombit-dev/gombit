package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/internal/faulttest"
)

// Fault tests for `gombit openapi generate`'s fetch of a live
// /openapi.json: every way the dependency can fail is an error, promptly,
// with the response body closed and nothing written.

// trackOpenAPIClient points the openapi fetch at dep through a body
// tracker, for the test's duration.
func trackOpenAPIClient(t *testing.T, dep *faulttest.HTTPDependency) *faulttest.BodyTracker {
	t.Helper()
	tracker := faulttest.TrackBodies(dep.Client().Transport)
	prev := openAPIHTTPClient
	openAPIHTTPClient = &http.Client{Transport: tracker, Timeout: prev.Timeout}
	t.Cleanup(func() { openAPIHTTPClient = prev })
	return tracker
}

// generateOpenAPI runs `gombit openapi generate` against url into a temp
// file and reports the error and whether the file was written.
func generateOpenAPI(t *testing.T, ctx context.Context, url string) (wrote bool, err error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "openapi.json")
	var stdout, stderr bytes.Buffer
	cmd := newOpenAPICommand(&stdout, &stderr)
	cmd.SetArgs([]string{"generate", "--url", url, "--out", out})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err = cmd.ExecuteContext(ctx)
	_, statErr := os.Stat(out)
	return statErr == nil, err
}

// assertCleanFailure fails t unless err is a failure mentioning want, the
// dependency saw exactly calls calls (no retry), and every response body was
// closed.
func assertCleanFailure(t *testing.T, err error, wrote bool, want string, dep *faulttest.HTTPDependency, tracker *faulttest.BodyTracker, calls int) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("generate = %v, want an error mentioning %q", err, want)
	}
	if wrote {
		t.Fatal("generate wrote a document after a failed fetch")
	}
	if dep.Calls() != calls {
		t.Fatalf("dependency calls = %d, want %d", dep.Calls(), calls)
	}
	if tracker.Open() != 0 {
		t.Fatalf("%d response bodies left open", tracker.Open())
	}
	dep.WaitIdle(t)
}

func TestFault_HTTP_ServerError(t *testing.T) {
	dep := faulttest.NewHTTPDependency(t, faulttest.ServerError())
	tracker := trackOpenAPIClient(t, dep)
	wrote, err := generateOpenAPI(t, context.Background(), dep.URL())
	assertCleanFailure(t, err, wrote, "status 500", dep, tracker, 1)
}

// TestFault_HTTP_TooManyRequests: a 429 is a failure reported at once; the
// one-shot CLI fetch does not retry (Retry-After is the caller's to honor by
// running it again).
func TestFault_HTTP_TooManyRequests(t *testing.T) {
	dep := faulttest.NewHTTPDependency(t, faulttest.TooManyRequests(30*time.Second))
	tracker := trackOpenAPIClient(t, dep)
	wrote, err := generateOpenAPI(t, context.Background(), dep.URL())
	assertCleanFailure(t, err, wrote, "status 429", dep, tracker, 1)
}

// TestFault_HTTP_Timeout: a dependency slower than the caller's deadline
// ends the fetch at the deadline (INV-2), and the abandoned call does not
// keep running on the dependency.
func TestFault_HTTP_Timeout(t *testing.T) {
	dep := faulttest.NewHTTPDependency(t, faulttest.Hang(make(chan struct{})))
	tracker := trackOpenAPIClient(t, dep)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	wrote, err := generateOpenAPI(t, ctx, dep.URL())
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("generate took %s against a hung dependency, want it to end at the 100ms deadline", took)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("generate = %v, want context.DeadlineExceeded", err)
	}
	assertCleanFailure(t, err, wrote, "fetch", dep, tracker, 1)
	if dep.Abandoned() != 1 {
		t.Fatalf("dependency saw %d abandoned calls, want the timed-out one", dep.Abandoned())
	}
}

func TestFault_HTTP_ConnectionReset(t *testing.T) {
	t.Run("before a response", func(t *testing.T) {
		dep := faulttest.NewHTTPDependency(t, faulttest.ResetConnection())
		tracker := trackOpenAPIClient(t, dep)
		wrote, err := generateOpenAPI(t, context.Background(), dep.URL())
		assertCleanFailure(t, err, wrote, "fetch", dep, tracker, 1)
	})
	t.Run("mid-body", func(t *testing.T) {
		dep := faulttest.NewHTTPDependency(t, faulttest.CutBody(`{"openapi":"3.1.0","paths":`))
		tracker := trackOpenAPIClient(t, dep)
		wrote, err := generateOpenAPI(t, context.Background(), dep.URL())
		assertCleanFailure(t, err, wrote, "read response", dep, tracker, 1)
	})
}

// TestFault_HTTP_MalformedResponse: a 200 whose body is not JSON, or JSON
// that is not an OpenAPI 3.1 document, is rejected, never written.
func TestFault_HTTP_MalformedResponse(t *testing.T) {
	for name, body := range map[string]string{
		"syntactically invalid": `{"openapi":"3.1.0","paths":{`,
		"not OpenAPI 3.1":       `{"openapi":"3.0.3","info":{"title":"x","version":"1"},"paths":{}}`,
		"not a document":        `["openapi"]`,
	} {
		t.Run(name, func(t *testing.T) {
			dep := faulttest.NewHTTPDependency(t, faulttest.Respond(http.StatusOK, body))
			tracker := trackOpenAPIClient(t, dep)
			wrote, err := generateOpenAPI(t, context.Background(), dep.URL())
			assertCleanFailure(t, err, wrote, "OpenAPI document", dep, tracker, 1)
		})
	}
}
