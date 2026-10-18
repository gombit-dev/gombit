// Package contractcheck checks an application's generated contract
// artifacts (the resource *.gen.go, the OpenAPI document, the TypeScript
// client) after an upgrade, with the existing drift gates' own checks:
// gombit upgrade contract (UPGRADE-8).
package contractcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gombit-dev/gombit/client"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/generate"
	"github.com/gombit-dev/gombit/resourcegen"
)

// Artifact names a kind of generated contract artifact.
type Artifact string

const (
	// Resources are the generator-owned resource files (*.gen.go)
	// gombit generate derives from the app's models.
	Resources Artifact = "resources"
	// OpenAPI is the committed OpenAPI document.
	OpenAPI Artifact = "openapi"
	// Client is the generated TypeScript client.
	Client Artifact = "client"
)

// Status is what a check found.
type Status string

const (
	// Current: the committed artifacts are what generation
	// produces.
	Current Status = "current"
	// Stale: some differ (Stale lists them).
	Stale Status = "stale"
	// Written: they were stale, and regenerated (Write).
	Written Status = "written"
	// Absent: the application has none of them.
	Absent Status = "absent"
	// Unchecked: they could not be checked (Reason says why).
	Unchecked Status = "unchecked"
)

// Result is one kind of artifact, checked.
type Result struct {
	Artifact Artifact `json:"artifact"`
	Status   Status   `json:"status"`
	// Stale are the stale files, relative to the application directory
	// (with Write: the ones regenerated).
	Stale []string `json:"stale"`
	// Reason says why it is unchecked, or what a refused write left.
	Reason string `json:"reason,omitempty"`
	// Regenerate is the command that regenerates them.
	Regenerate string `json:"regenerate"`
}

// Report is the state of an application's generated contract
// artifacts.
type Report struct {
	Checks []Result `json:"checks"`
}

// Stale reports whether any check found stale artifacts it left so.
func (r Report) Stale() bool {
	for _, c := range r.Checks {
		if c.Status == Stale {
			return true
		}
	}
	return false
}

// Options configures CheckContract.
type Options struct {
	// SpecPath is the committed OpenAPI document and OutDir the generated
	// client directory, relative to the application directory: by default
	// gombit new's (client.DefaultSpecPath, client.DefaultOutDir).
	SpecPath string
	OutDir   string
	// LiveSpec is the running application's OpenAPI document (gombit
	// upgrade contract --url fetches it). Without it the committed
	// document cannot be checked, since it is derived from the running app,
	// and the client is checked against the committed one.
	LiveSpec []byte
	// Write regenerates stale artifacts with the normal generation:
	// gombit generate for the resources, gombit client generate (from the
	// live document, which is written too, or the committed one) for the
	// client. Only generator-owned files are written: never a resource's
	// hooks file, nor a file of the client directory gombit did not
	// generate.
	Write bool
	// NPXBinary runs openapi-typescript, as gombit client generate does.
	// Default: npx.
	NPXBinary string
	Stderr    io.Writer
}

// CheckContract reports whether the generated contract artifacts of the
// application in workDir are what generation produces now, which an upgrade
// can change: the framework renders the resource files and the client, and
// the running app derives the OpenAPI document. Each check is the same one
// the existing drift gates make (gombit generate --check, gombit client
// check), so it agrees with them:
//
//   - the resource files are rendered as gombit generate --check renders
//     them, by a loader run inside the application's module, so with the
//     framework version its go.mod requires;
//   - the OpenAPI document is compared with LiveSpec (semantically), and
//     is unchecked without it;
//   - the client is regenerated, as gombit client generate does, from
//     LiveSpec or else the committed document, and compared byte for byte;
//     it is unchecked when there is no document, or no npx to run
//     openapi-typescript.
//
// With Write it regenerates what is stale, through the same generation,
// and reports what it wrote.
func Check(ctx context.Context, workDir string, opts Options) (Report, error) {
	if opts.SpecPath == "" {
		opts.SpecPath = client.DefaultSpecPath
	}
	if opts.OutDir == "" {
		opts.OutDir = client.DefaultOutDir
	}
	if opts.NPXBinary == "" {
		opts.NPXBinary = "npx"
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	resources, err := checkResources(ctx, workDir, opts)
	if err != nil {
		return Report{}, err
	}
	spec, cl, err := checkClient(ctx, workDir, opts)
	if err != nil {
		return Report{}, err
	}
	return Report{Checks: []Result{resources, spec, cl}}, nil
}

func checkResources(ctx context.Context, workDir string, opts Options) (Result, error) {
	c := Result{Artifact: Resources, Stale: []string{}, Regenerate: "gombit generate"}
	if resourcegen.ValidateAppLayout(workDir) != nil {
		c.Status = Absent // not a gombit new application: no resources
		return c, nil
	}
	gen := generate.Options{WorkDir: workDir, Stdout: io.Discard, Stderr: opts.Stderr}
	check := generate.Stale
	if opts.Write {
		check = generate.Update // renders once, writes only the stale files
	}
	stale, resources, err := check(ctx, gen)
	if err != nil {
		// The loader compiles the app's models: after an upgrade with
		// breaking changes they may not compile yet. Say so, and go on.
		c.Status, c.Reason = Unchecked, "the resource loader failed (fix the build first): "+err.Error()
		return c, nil
	}
	if resources == 0 {
		c.Status = Absent
		return c, nil
	}
	for _, f := range stale {
		c.Stale = append(c.Stale, f.Path)
	}
	switch {
	case len(stale) == 0:
		c.Status = Current
	case opts.Write:
		c.Status = Written
	default:
		c.Status = Stale
	}
	return c, nil
}

// checkClient checks the committed OpenAPI document (against the live one)
// and the generated client (against what the live, or else the committed,
// document generates).
func checkClient(ctx context.Context, workDir string, opts Options) (spec, cl Result, err error) {
	spec = Result{Artifact: OpenAPI, Stale: []string{}, Regenerate: "gombit openapi generate --url <the running app>/openapi.json --out " + opts.SpecPath}
	cl = Result{Artifact: Client, Stale: []string{}, Regenerate: "gombit client generate --spec " + opts.SpecPath + " --out " + opts.OutDir}
	specPath := filepath.Join(workDir, filepath.FromSlash(opts.SpecPath))
	outDir := filepath.Join(workDir, filepath.FromSlash(opts.OutDir))
	committedSpec, specErr := os.ReadFile(specPath) // #nosec G304 -- the application's committed OpenAPI document
	if specErr != nil && !errors.Is(specErr, os.ErrNotExist) {
		return spec, cl, fmt.Errorf("contractcheck: %w", specErr)
	}
	hasSpec := specErr == nil
	hasClient := false
	for _, name := range client.GeneratedFiles() {
		if _, err := os.Stat(filepath.Join(outDir, name)); err == nil {
			hasClient = true
		}
	}

	switch {
	case !hasSpec: // not committed (gombit dev writes it): not the app's artifact
		spec.Status = Absent
	case opts.LiveSpec == nil:
		spec.Status, spec.Reason = Unchecked, "it is derived from the running app: pass its document (gombit upgrade contract --url http://127.0.0.1:8080/openapi.json)"
	}
	source := opts.LiveSpec
	if source == nil {
		source = committedSpec
	}
	switch {
	case !hasClient:
		cl.Status = Absent
	case source == nil:
		cl.Status, cl.Reason = Unchecked, "there is no OpenAPI document to generate it from ("+opts.SpecPath+", or --url)"
	}
	if _, err := exec.LookPath(opts.NPXBinary); err != nil && cl.Status == "" {
		cl.Status, cl.Reason = Unchecked, "openapi-typescript runs through npx, which is not installed: "+err.Error()
	}

	// The document against the live one; the client against what the
	// source document generates (client.Drift, the comparison gombit client
	// check makes, given that document as both the committed and the
	// current one, so only the client can differ).
	if spec.Status == "" {
		equal, err := jsonEqual(committedSpec, opts.LiveSpec)
		if err != nil {
			return spec, cl, err
		}
		spec.Status = Current
		if !equal {
			spec.Status, spec.Stale = Stale, []string{opts.SpecPath}
		}
	}
	if cl.Status == "" {
		stale, err := clientDrift(ctx, workDir, opts, source)
		if err != nil {
			return spec, cl, err
		}
		cl.Stale, cl.Status = stale, statusOf(stale)
	}
	if !opts.Write {
		return spec, cl, nil
	}
	return writeClient(ctx, workDir, opts, spec, cl)
}

// clientDrift are the client files that differ from what source generates.
func clientDrift(ctx context.Context, workDir string, opts Options, source []byte) ([]string, error) {
	tmp, err := os.MkdirTemp("", "gombit-contract-*")
	if err != nil {
		return nil, fmt.Errorf("contractcheck: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	specPath := filepath.Join(tmp, "openapi.json")
	if err := os.WriteFile(specPath, source, 0o600); err != nil { // #nosec G703 -- a file in this function's own temporary directory
		return nil, fmt.Errorf("contractcheck: %w", err)
	}
	drifted, err := client.Drift(ctx, client.DriftOptions{
		WorkDir: workDir, SpecPath: specPath, OutDir: opts.OutDir, SpecBytes: source,
		NPXBinary: opts.NPXBinary, Stderr: opts.Stderr,
	})
	if err != nil {
		return nil, fmt.Errorf("contractcheck: check the client: %w", err)
	}
	stale := []string{}
	for _, d := range drifted {
		if d.Spec {
			continue
		}
		name := filepath.ToSlash(d.Path)
		if d.Missing {
			name += " (missing)"
		}
		stale = append(stale, name)
	}
	return stale, nil
}

func jsonEqual(a, b []byte) (bool, error) {
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		return false, fmt.Errorf("contractcheck: the committed OpenAPI document is not JSON: %w", err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		return false, fmt.Errorf("contractcheck: the live OpenAPI document is not JSON: %w", err)
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya), nil
}

func statusOf(stale []string) Status {
	if len(stale) > 0 {
		return Stale
	}
	return Current
}

// writeClient regenerates a stale document (from the live one) and a stale
// client, through gombit client generate's own code, which refuses to
// overwrite a file it did not generate.
func writeClient(ctx context.Context, workDir string, opts Options, spec, cl Result) (Result, Result, error) {
	specPath := filepath.Join(workDir, filepath.FromSlash(opts.SpecPath))
	if spec.Status == Stale {
		// As gombit openapi generate writes it.
		if err := contract.WriteOpenAPIFile(specPath, opts.LiveSpec); err != nil {
			return spec, cl, fmt.Errorf("contractcheck: write %s: %w", opts.SpecPath, err)
		}
		spec.Status = Written
	}
	if cl.Status != Stale {
		return spec, cl, nil
	}
	// Generate from the document the client was checked against: the live
	// one when given (the committed file may not exist: gombit dev writes
	// it, and an app need not commit it), else the committed one.
	source := specPath
	if opts.LiveSpec != nil {
		tmp, err := os.MkdirTemp("", "gombit-contract-*")
		if err != nil {
			return spec, cl, fmt.Errorf("contractcheck: %w", err)
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		source = filepath.Join(tmp, "openapi.json")
		if err := contract.WriteOpenAPIFile(source, opts.LiveSpec); err != nil {
			return spec, cl, fmt.Errorf("contractcheck: %w", err)
		}
	}
	var out strings.Builder
	err := client.Generate(ctx, client.Options{
		WorkDir: workDir, SpecPath: source, OutDir: filepath.Join(workDir, filepath.FromSlash(opts.OutDir)),
		NPXBinary: opts.NPXBinary, Stdout: &out, Stderr: opts.Stderr,
	})
	if err != nil {
		// A file there that gombit did not generate: left as it is.
		cl.Reason = err.Error()
		return spec, cl, nil
	}
	cl.Status = Written
	return spec, cl, nil
}

// Render writes the report as text.
func (r Report) Render(w io.Writer) error {
	var b strings.Builder
	b.WriteString("Generated contract artifacts:\n")
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "  %-9s %s", c.Artifact+":", c.Status)
		switch c.Status {
		case Stale:
			fmt.Fprintf(&b, ": %s\n      regenerate: %s (or gombit upgrade contract --write)\n", strings.Join(c.Stale, ", "), c.Regenerate)
		case Written:
			fmt.Fprintf(&b, ": %s\n", strings.Join(c.Stale, ", "))
		case Unchecked:
			fmt.Fprintf(&b, ": %s\n", c.Reason)
		default:
			b.WriteString("\n")
		}
		if c.Status == Stale && c.Reason != "" {
			fmt.Fprintf(&b, "      not written: %s\n", c.Reason)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}
