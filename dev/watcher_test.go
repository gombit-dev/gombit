package dev

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWatchOpenAPIGeneratesOnChange(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	n := 0
	var generated []string
	get := func(ctx context.Context, rawURL string) ([]byte, error) {
		if rawURL != "http://127.0.0.1:8080/openapi.json" {
			t.Errorf("unexpected URL %q", rawURL)
		}
		mu.Lock()
		defer mu.Unlock()
		n++
		if n == 1 {
			return []byte(`{"openapi":"3.1.0","info":{"title":"one"}}`), nil
		}
		return []byte(`{"openapi":"3.1.0","info":{"title":"two"}}`), nil
	}
	generate := func(ctx context.Context, spec []byte) error {
		mu.Lock()
		generated = append(generated, string(spec))
		mu.Unlock()
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- watchOpenAPI(ctx, Options{
			PollInterval: 10 * time.Millisecond,
			HTTPGet:      get,
			Generate:     generate,
			Stderr:       &bytes.Buffer{},
		}, "http://127.0.0.1:8080/openapi.json")
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(generated)
		mu.Unlock()
		if count >= 2 {
			cancel()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("watchOpenAPI() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchOpenAPI() did not return after cancel")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(generated) < 2 {
		t.Fatalf("generated %d times, want at least 2: %v", len(generated), generated)
	}
	if generated[0] == generated[1] {
		t.Fatal("expected different spec bytes to trigger a second generate")
	}
}

func TestWatchOpenAPISkipsUnchangedSpec(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	calls := 0
	generate := func(ctx context.Context, spec []byte) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	}
	get := func(ctx context.Context, rawURL string) ([]byte, error) {
		return []byte(`{"openapi":"3.1.0"}`), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- watchOpenAPI(ctx, Options{
			PollInterval: 10 * time.Millisecond,
			HTTPGet:      get,
			Generate:     generate,
			Stderr:       &bytes.Buffer{},
		}, "http://127.0.0.1:8080/openapi.json")
	}()

	time.Sleep(80 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchOpenAPI() did not return after cancel")
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("Generate called %d times, want 1 for unchanged spec", calls)
	}
}

func TestWatchOpenAPIDoesNotRetryFailedGenerateUntilSpecChanges(t *testing.T) {
	t.Parallel()

	const (
		specOne = `{"openapi":"3.1.0","info":{"title":"one"}}`
		specTwo = `{"openapi":"3.1.0","info":{"title":"two"}}`
	)
	var (
		fetches   int
		generated []string
		stderr    bytes.Buffer
	)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	get := func(context.Context, string) ([]byte, error) {
		fetches++
		switch fetches {
		case 1, 2:
			return []byte(specOne), nil
		case 3:
			return []byte(specTwo), nil
		default:
			cancel()
			return []byte(specTwo), nil
		}
	}
	generate := func(_ context.Context, spec []byte) error {
		generated = append(generated, string(spec))
		return errors.New("generation failed")
	}

	err := watchOpenAPI(ctx, Options{
		PollInterval: time.Millisecond,
		HTTPGet:      get,
		Generate:     generate,
		Stderr:       &stderr,
	}, "http://127.0.0.1:8080/openapi.json")
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("watchOpenAPI() error = %v", err)
	}

	if len(generated) != 2 {
		t.Fatalf("Generate called %d times for two distinct specs, want 2: %v", len(generated), generated)
	}
	if generated[0] != specOne || generated[1] != specTwo {
		t.Fatalf("generated = %v, want [%s %s]", generated, specOne, specTwo)
	}
	if got := strings.Count(stderr.String(), "gombit dev: regenerate TypeScript client: generation failed"); got != 2 {
		t.Fatalf("generation error logged %d times, want once per distinct spec", got)
	}
}
