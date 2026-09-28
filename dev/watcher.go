package dev

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gombit-dev/gombit/client"
)

func watchOpenAPI(ctx context.Context, opts Options, specURL string) error {
	get := opts.HTTPGet
	if get == nil {
		get = defaultHTTPGet
	}
	generate := opts.Generate
	if generate == nil {
		generate = func(ctx context.Context, spec []byte) error {
			return generateClient(ctx, opts, spec)
		}
	}

	var last []byte
	ticker := time.NewTicker(opts.PollInterval)
	defer ticker.Stop()

	try := func() {
		spec, err := get(ctx, specURL)
		if err != nil {
			return
		}
		if len(spec) == 0 || bytes.Equal(spec, last) {
			return
		}
		if err := generate(ctx, spec); err != nil {
			_, _ = fmt.Fprintf(opts.Stderr, "gombit dev: regenerate TypeScript client: %v\n", err)
			return
		}
		last = append([]byte(nil), spec...)
	}

	try()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			try()
		}
	}
}

func generateClient(ctx context.Context, opts Options, spec []byte) error {
	dir := filepath.Join(opts.WorkDir, ".gombit")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	specPath := filepath.Join(dir, "openapi.json")
	if err := os.WriteFile(specPath, spec, 0o600); err != nil {
		return fmt.Errorf("write spec: %w", err)
	}
	return client.Generate(ctx, client.Options{
		WorkDir:  opts.WorkDir,
		SpecPath: specPath,
		OutDir:   opts.ClientOut,
		Stdout:   opts.Stdout,
		Stderr:   opts.Stderr,
	})
}

// specHTTPClient fetches the dev server's /openapi.json. The 2s timeout
// bounds a hung server; tests swap the transport.
var specHTTPClient = &http.Client{Timeout: 2 * time.Second}

func defaultHTTPGet(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := specHTTPClient.Do(req) //nolint:gosec // URL is the local Go server's /openapi.json
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", rawURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 8<<20 {
		return nil, fmt.Errorf("spec exceeds 8MiB")
	}
	return body, nil
}
