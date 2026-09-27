package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gombit-dev/gombit/contract"
)

//go:generate go run ../cmd/gombit client check --write --spec ../examples/client/openapi.json --out ../examples/client/frontend/src/api/generated

const (
	// SampleSpecPath is the committed OpenAPI fixture for the sample widget API.
	SampleSpecPath = "examples/client/openapi.json"
	// SampleOutDir is the committed TypeScript client generated from SampleSpecPath.
	SampleOutDir = "examples/client/frontend/src/api/generated"
)

var sampleClientFiles = []string{"schema.ts", "client.ts", "error.ts"}

// DriftOptions configures a contract drift check or fixture rewrite.
type DriftOptions struct {
	// WorkDir is the repository root. Empty means the current working directory.
	WorkDir string
	// SpecPath is the committed OpenAPI fixture, relative to WorkDir unless absolute.
	// Default: SampleSpecPath.
	SpecPath string
	// OutDir is the committed generated-client directory, relative to WorkDir unless absolute.
	// Default: SampleOutDir.
	OutDir string
	// API is the Huma API to emit. Nil and SpecBytes nil means SampleApp().
	API huma.API
	// SpecBytes is a pre-fetched OpenAPI document (e.g. from a live /openapi.json
	// URL) to treat as the current contract. Takes precedence over API. Callers
	// outside this module — generated apps have no Go-level huma.API to pass —
	// use this to check their own contract instead of the framework's SampleApp.
	SpecBytes []byte
	// Write regenerates committed fixtures instead of comparing them.
	Write bool
	// NPXBinary is the npx executable used by Generate. Default: npx.
	NPXBinary string
	Stdout    io.Writer
	Stderr    io.Writer
}

// ErrDrift is the error CheckDrift wraps when a committed artifact differs
// from what the contract produces.
var ErrDrift = errors.New("client: contract drift")

// CheckDrift regenerates the OpenAPI document and TypeScript client and
// reports whether committed artifacts would change.
//
// The spec is compared semantically via encoding/json, so whitespace-only
// differences are not drift. Generated TypeScript is compared byte-for-byte.
//
// The document used for comparison comes from, in order: opts.SpecBytes (a
// pre-fetched live spec), opts.API (an in-process huma.API — this module's
// own tests and go:generate directive use this to compare examples/client/
// against SampleApp), or SampleApp() itself. Generated apps have neither a
// Go-level huma.API nor a reason to compare against the framework's sample
// widget API, so the CLI's `gombit client check --url` path fetches the
// live spec over HTTP and passes it as SpecBytes.
//
// When Write is true, fixtures are rewritten in place.
func CheckDrift(ctx context.Context, opts DriftOptions) error {
	if ctx == nil {
		return errors.New("client: nil context")
	}
	opts = normalizeDriftOptions(opts)

	spec := opts.SpecBytes
	if spec == nil {
		api := opts.API
		if api == nil {
			app, err := SampleApp()
			if err != nil {
				return fmt.Errorf("client: sample app: %w", err)
			}
			api = app.API()
		}
		var err error
		spec, err = contract.OpenAPIJSON(api)
		if err != nil {
			return err
		}
	}

	if opts.Write {
		return writeSampleFixtures(ctx, opts, spec)
	}
	return compareSampleFixtures(ctx, opts, spec)
}

func normalizeDriftOptions(opts DriftOptions) DriftOptions {
	if opts.WorkDir == "" {
		opts.WorkDir = "."
	}
	if opts.SpecPath == "" {
		opts.SpecPath = SampleSpecPath
	}
	if opts.OutDir == "" {
		opts.OutDir = SampleOutDir
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	if opts.NPXBinary == "" {
		opts.NPXBinary = "npx"
	}
	return opts
}

func resolvePath(workDir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(workDir, path)
}

func writeSampleFixtures(ctx context.Context, opts DriftOptions, spec []byte) error {
	specPath := resolvePath(opts.WorkDir, opts.SpecPath)
	if err := contract.WriteOpenAPIFile(specPath, spec); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(opts.Stdout, "wrote %s\n", displayPath(opts.WorkDir, specPath)); err != nil {
		return err
	}
	return Generate(ctx, Options{
		WorkDir:   opts.WorkDir,
		SpecPath:  specPath,
		OutDir:    resolvePath(opts.WorkDir, opts.OutDir),
		NPXBinary: opts.NPXBinary,
		Stdout:    opts.Stdout,
		Stderr:    opts.Stderr,
	})
}

func compareSampleFixtures(ctx context.Context, opts DriftOptions, generatedSpec []byte) error {
	tmpDir, err := os.MkdirTemp("", "gombit-drift-*")
	if err != nil {
		return fmt.Errorf("client: temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	generatedSpecPath := filepath.Join(tmpDir, "openapi.json")
	if err := contract.WriteOpenAPIFile(generatedSpecPath, generatedSpec); err != nil {
		return err
	}

	specPath := resolvePath(opts.WorkDir, opts.SpecPath)
	// #nosec G304 -- spec path is a user-supplied committed fixture
	committedSpec, err := os.ReadFile(specPath)
	if err != nil {
		return fmt.Errorf("client: read committed spec: %w", err)
	}

	var drifted []string
	equal, err := jsonEqual(committedSpec, generatedSpec)
	if err != nil {
		return err
	}
	if !equal {
		drifted = append(drifted, displayPath(opts.WorkDir, specPath))
	}

	generatedOut := filepath.Join(tmpDir, "generated")
	if err := Generate(ctx, Options{
		WorkDir:   tmpDir,
		SpecPath:  generatedSpecPath,
		OutDir:    generatedOut,
		NPXBinary: opts.NPXBinary,
		Stdout:    io.Discard,
		Stderr:    opts.Stderr,
	}); err != nil {
		return err
	}

	committedOut := resolvePath(opts.WorkDir, opts.OutDir)
	for _, name := range sampleClientFiles {
		committedPath := filepath.Join(committedOut, name)
		generatedPath := filepath.Join(generatedOut, name)
		// #nosec G304 -- committedPath is the checked-in generated client file
		committed, readErr := os.ReadFile(committedPath)
		if readErr != nil {
			return fmt.Errorf("client: read committed %s: %w", displayPath(opts.WorkDir, committedPath), readErr)
		}
		// #nosec G304 -- generatedPath is created in this function
		generated, readErr := os.ReadFile(generatedPath)
		if readErr != nil {
			return fmt.Errorf("client: read generated %s: %w", name, readErr)
		}
		if !bytes.Equal(committed, generated) {
			drifted = append(drifted, displayPath(opts.WorkDir, committedPath))
		}
	}

	if len(drifted) > 0 {
		return fmt.Errorf("%w in %s; regenerate with gombit client check --write", ErrDrift, strings.Join(drifted, ", "))
	}
	_, err = fmt.Fprintln(opts.Stdout, "no contract drift")
	return err
}

func jsonEqual(left, right []byte) (bool, error) {
	var leftValue any
	var rightValue any
	if err := json.Unmarshal(left, &leftValue); err != nil {
		return false, fmt.Errorf("client: parse committed spec: %w", err)
	}
	if err := json.Unmarshal(right, &rightValue); err != nil {
		return false, fmt.Errorf("client: parse generated spec: %w", err)
	}
	leftBytes, err := json.Marshal(leftValue)
	if err != nil {
		return false, fmt.Errorf("client: marshal committed spec: %w", err)
	}
	rightBytes, err := json.Marshal(rightValue)
	if err != nil {
		return false, fmt.Errorf("client: marshal generated spec: %w", err)
	}
	return string(leftBytes) == string(rightBytes), nil
}
