// Package contractcheck checks an application's generated contract
// artifacts (the resource *.gen.go, the OpenAPI document, the TypeScript
// client) after an upgrade, with the existing drift gates' own checks:
// gombit upgrade contract (UPGRADE-8).
package contractcheck

import (
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
	// Stale: some differ (Files lists them).
	Stale Status = "stale"
	// Written: they were stale, and regenerated (Write).
	Written Status = "written"
	// Absent: the application has none of them.
	Absent Status = "absent"
	// Unchecked: they could not be checked (Reason says why). A gate
	// that must not pass on what it could not see fails on it too
	// (Report.Unchecked; gombit upgrade contract --strict).
	Unchecked Status = "unchecked"
)

// Result is one kind of artifact, checked.
type Result struct {
	Artifact Artifact `json:"artifact"`
	Status   Status   `json:"status"`
	// Files are the files the status is about, relative to the
	// application directory: the stale ones, or (Written) the ones
	// regenerated.
	Files []string `json:"files"`
	// Reason says why it is unchecked; for a stale one, what else there is
	// to know: why the document is stale, or why a write was refused.
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

// Unchecked reports whether any check could not be made.
func (r Report) Unchecked() bool {
	for _, c := range r.Checks {
		if c.Status == Unchecked {
			return true
		}
	}
	return false
}

// Options configures Check.
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

// Check reports whether the generated contract artifacts of the application
// in workDir are what generation produces now, which an upgrade can change:
// the framework renders the resource files and the client, and the running
// app derives the OpenAPI document. Each check is the same one the existing
// drift gates make (gombit generate --check, gombit client check), so it
// agrees with them:
//
//   - the resource files are rendered as gombit generate --check renders
//     them, by a loader run inside the application's module, so with the
//     framework version its go.mod requires; they are unchecked when the
//     loader fails (the models do not compile);
//   - the OpenAPI document is compared with LiveSpec (semantically), and
//     is unchecked without it;
//   - the client is regenerated, as gombit client generate does, from
//     LiveSpec or else the committed document, and compared byte for byte;
//     it is unchecked when there is no document, or no npx to run
//     openapi-typescript.
//
// The artifacts depend on each other: the running app is built from the
// resource files, and serves the document the client is generated from. So
// while the resources are not current (stale, just regenerated, or
// unchecked), the checks against LiveSpec are unchecked: the app must be
// rebuilt first.
//
// With Write it regenerates what is stale, through the same generation,
// and reports what it wrote; it never writes the document or the client
// from an app built before the resources were. A failed write is an error,
// naming the files written before it.
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
	if opts.LiveSpec != nil && !json.Valid(opts.LiveSpec) {
		return Report{}, errors.New("contractcheck: the live OpenAPI document is not JSON")
	}
	resources, err := checkResources(ctx, workDir, opts)
	if err != nil {
		return Report{}, err
	}
	spec, cl, err := checkClient(ctx, workDir, opts, resources)
	if err != nil {
		if resources.Status == Written {
			err = fmt.Errorf("%w (the resources were regenerated first: %s)", err, strings.Join(resources.Files, ", "))
		}
		return Report{}, err
	}
	return Report{Checks: []Result{resources, spec, cl}}, nil
}

func checkResources(ctx context.Context, workDir string, opts Options) (Result, error) {
	c := Result{Artifact: Resources, Files: []string{}, Regenerate: "gombit generate"}
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
	var loaderErr *generate.LoaderError
	switch {
	case errors.As(err, &loaderErr) && ctx.Err() == nil: // not a cancellation
		// The loader compiles the app's models: after an upgrade with
		// breaking changes they may not compile yet. Say so, and go on.
		c.Status, c.Reason = Unchecked, "the resource loader failed (fix the build first): "+err.Error()
		return c, nil
	case err != nil && len(stale) > 0: // Update wrote these, then failed
		return c, fmt.Errorf("contractcheck: regenerate the resources: wrote %s, then: %w", strings.Join(paths(stale), ", "), err)
	case err != nil:
		return c, fmt.Errorf("contractcheck: check the resources: %w", err)
	}
	if resources == 0 {
		c.Status = Absent
		return c, nil
	}
	c.Files = paths(stale)
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

func paths(files []generate.StaleFile) []string {
	out := []string{}
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

// checkClient checks the committed OpenAPI document (against the live one)
// and the generated client (against what the live, or else the committed,
// document generates), given the resources' result: the live document is
// the app's as built from them.
func checkClient(ctx context.Context, workDir string, opts Options, resources Result) (spec, cl Result, err error) {
	spec = Result{Artifact: OpenAPI, Files: []string{}, Regenerate: "gombit openapi generate --url <the running app>/openapi.json --out " + opts.SpecPath}
	cl = Result{Artifact: Client, Files: []string{}, Regenerate: "gombit client generate --spec " + opts.SpecPath + " --out " + opts.OutDir}
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

	// The running app was built from the resource files: unless they are
	// current, its document is not the one it will serve once rebuilt.
	rebuild := ""
	switch resources.Status {
	case Stale:
		rebuild = "the resource files are stale, so the running app's document is not the one it will serve: regenerate them (gombit generate), rebuild and restart the app, then rerun"
	case Written:
		rebuild = "the resource files were just regenerated, so the running app's document predates them: rebuild and restart the app, then rerun"
	case Unchecked:
		rebuild = "the resource files could not be checked, so the running app's document may not be current: fix the build, rebuild and restart the app, then rerun"
	}
	switch {
	case !hasSpec: // not committed (gombit dev writes it): not the app's artifact
		spec.Status = Absent
	case opts.LiveSpec == nil:
		spec.Status, spec.Reason = Unchecked, "it is derived from the running app: pass its document (gombit upgrade contract --url http://127.0.0.1:8080/openapi.json)"
	case rebuild != "":
		spec.Status, spec.Reason = Unchecked, rebuild
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
	case opts.LiveSpec != nil && rebuild != "":
		cl.Status, cl.Reason = Unchecked, rebuild
	case !json.Valid(source):
		cl.Status, cl.Reason = Unchecked, "the OpenAPI document it is generated from ("+opts.SpecPath+") is not JSON"
	}
	if _, err := exec.LookPath(opts.NPXBinary); err != nil && cl.Status == "" {
		cl.Status, cl.Reason = Unchecked, "openapi-typescript runs through npx, which is not installed: "+err.Error()
	}

	// The document against the live one, semantically; the client against
	// what the source document generates, byte for byte (the comparisons
	// gombit client check makes).
	if spec.Status == "" {
		equal, err := client.SpecEqual(committedSpec, opts.LiveSpec)
		switch {
		case err != nil: // the committed one is not JSON: regenerating fixes it
			spec.Status, spec.Files, spec.Reason = Stale, []string{opts.SpecPath}, err.Error()
		case equal:
			spec.Status = Current
		default:
			spec.Status, spec.Files = Stale, []string{opts.SpecPath}
		}
	}
	if cl.Status == "" {
		stale, err := clientDrift(ctx, workDir, opts, source)
		if err != nil {
			return spec, cl, err
		}
		cl.Files, cl.Status = stale, statusOf(stale)
	}
	if !opts.Write {
		return spec, cl, nil
	}
	return writeClient(ctx, workDir, opts, spec, cl)
}

// clientDrift are the client files that differ from what source generates.
func clientDrift(ctx context.Context, workDir string, opts Options, source []byte) ([]string, error) {
	drifted, err := client.ClientDrift(ctx, client.DriftOptions{
		WorkDir: workDir, OutDir: opts.OutDir, SpecBytes: source,
		NPXBinary: opts.NPXBinary, Stderr: opts.Stderr,
	})
	if err != nil {
		return nil, fmt.Errorf("contractcheck: check the client: %w", err)
	}
	stale := []string{}
	for _, d := range drifted {
		name := filepath.ToSlash(d.Path)
		if d.Missing {
			name += " (missing)"
		}
		stale = append(stale, name)
	}
	return stale, nil
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
		cl.Reason = "not written: " + err.Error()
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
			fmt.Fprintf(&b, ": %s\n      regenerate: %s (or gombit upgrade contract --write)\n", strings.Join(c.Files, ", "), c.Regenerate)
		case Written:
			fmt.Fprintf(&b, ": %s\n", strings.Join(c.Files, ", "))
		case Unchecked:
			fmt.Fprintf(&b, ": %s\n", c.Reason)
		default:
			b.WriteString("\n")
		}
		if c.Status == Stale && c.Reason != "" {
			fmt.Fprintf(&b, "      %s\n", c.Reason)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}
