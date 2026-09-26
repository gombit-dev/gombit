package migrations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/gombit-dev/gombit/config"
)

// DirOptions configures the migration-directory checks `gombit db lint` and
// `gombit db repair` run on the dev database.
type DirOptions struct {
	WorkDir      string
	Driver       config.DatabaseDriver
	MigrationDir string
	AtlasBinary  string
	Stdout       io.Writer
	Stderr       io.Writer

	runner commandRunner
}

func (o DirOptions) withDefaults() (DirOptions, string, string, error) {
	if o.WorkDir == "" {
		o.WorkDir = "."
	}
	if o.MigrationDir == "" {
		o.MigrationDir = defaultMigrationDir
	}
	if o.AtlasBinary == "" {
		o.AtlasBinary = defaultAtlasBinary
	}
	if o.Stdout == nil {
		o.Stdout = io.Discard
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	if o.runner == nil {
		o.runner = execRunner{}
	}
	if err := validateDriver(o.Driver); err != nil {
		return o, "", "", err
	}
	absWorkDir, err := filepath.Abs(o.WorkDir)
	if err != nil {
		return o, "", "", fmt.Errorf("migrations: resolve work dir: %w", err)
	}
	dir := o.MigrationDir
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(absWorkDir, dir)
	}
	return o, absWorkDir, dir, nil
}

// ChecksumError reports migration files that no longer match atlas.sum.
type ChecksumError struct {
	// Files are the migration files Atlas reports as edited, added, or
	// removed since atlas.sum was written.
	Files []string
}

func (e *ChecksumError) Error() string {
	what := "the migration directory"
	if len(e.Files) > 0 {
		what = strings.Join(e.Files, ", ")
	}
	return fmt.Sprintf("migrations: checksum mismatch: %s changed after atlas.sum was written; if the change is intended, run 'gombit db repair' (or 'gombit db hash')", what)
}

// ReplayError reports a migration directory that does not apply cleanly on
// an empty dev database.
type ReplayError struct {
	// Detail is Atlas's explanation, with raw-Atlas recovery hints rewritten
	// to their gombit commands.
	Detail string
}

func (e *ReplayError) Error() string {
	return fmt.Sprintf("migrations: the migration directory does not apply to an empty database: %s", e.Detail)
}

// checksumFileRe matches Atlas's per-file checksum diagnostic
// ("L2: 20260101000000_init.sql was edited"), on a line of its own.
var checksumFileRe = regexp.MustCompile(`^L\d+: (\S+\.sql) was (?:edited|added|removed)$`)

// checksumDiagnostic reports Atlas's checksum failure by its own diagnostic
// lines, not by the word "checksum", which a replayed statement can contain.
func checksumDiagnostic(text string) (files []string, ok bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "Error: checksum mismatch", strings.HasPrefix(line, "You have a checksum error in your migration directory"):
			ok = true
		default:
			if m := checksumFileRe.FindStringSubmatch(line); m != nil {
				ok = true
				files = append(files, m[1])
			}
		}
	}
	return files, ok
}

// ValidateDir checks the migration directory's integrity with
// `atlas migrate validate` (Atlas Community Edition): atlas.sum matches every
// file, and the migrations replay on an empty dev database. It returns a
// *ChecksumError or a *ReplayError.
func ValidateDir(ctx context.Context, opts DirOptions) error {
	if ctx == nil {
		return errors.New("migrations: nil context")
	}
	opts, absWorkDir, dir, err := opts.withDefaults()
	if err != nil {
		return err
	}
	var out bytes.Buffer
	args := []string{"migrate", "validate", "--dir", "file://" + filepath.ToSlash(dir), "--dev-url", devURL(opts.Driver)}
	if err := opts.runner.Run(ctx, absWorkDir, opts.AtlasBinary, args, &out, &out); err != nil {
		text := atlasMessage(out.String())
		if files, ok := checksumDiagnostic(text); ok {
			return &ChecksumError{Files: files}
		}
		if text == "" {
			text = err.Error()
		}
		return &ReplayError{Detail: text}
	}
	return nil
}

// InspectDirAt returns the HCL of the schema the migration directory builds
// up to and including version, from `atlas schema inspect` on the dev
// database. An empty version is the state before the first migration.
func InspectDirAt(ctx context.Context, opts DirOptions, version string) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("migrations: nil context")
	}
	if version == "" {
		return nil, nil
	}
	opts, absWorkDir, dir, err := opts.withDefaults()
	if err != nil {
		return nil, err
	}
	var out, errOut bytes.Buffer
	url := "file://" + filepath.ToSlash(dir) + "?version=" + version
	args := []string{"schema", "inspect", "--url", url, "--dev-url", devURL(opts.Driver)}
	if err := opts.runner.Run(ctx, absWorkDir, opts.AtlasBinary, args, &out, &errOut); err != nil {
		if msg := atlasMessage(errOut.String()); msg != "" {
			return nil, fmt.Errorf("migrations: inspect the migration directory at %s: %w: %s", version, err, msg)
		}
		return nil, fmt.Errorf("migrations: inspect the migration directory at %s: %w", version, err)
	}
	return out.Bytes(), nil
}

// allowDirective marks a migration's acknowledged destructive or unsafe
// change: `-- gombit:allow <step id or code>`. makemigrations writes one for
// every step --allow (or --forget-model) acknowledged, and `gombit db lint`
// accepts a step only when its migration carries one.
const allowDirective = "-- gombit:allow "

// AllowDirectives returns the step IDs or codes a migration's
// `-- gombit:allow` lines acknowledge.
func AllowDirectives(sql string) []string {
	var out []string
	for _, line := range strings.Split(sql, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), allowDirective); ok {
			if id := strings.TrimSpace(rest); id != "" {
				out = append(out, id)
			}
		}
	}
	return out
}

// withAllowDirectives prepends one `-- gombit:allow` line per id the SQL does
// not carry yet.
func withAllowDirectives(sql string, ids []string) string {
	have := map[string]bool{}
	for _, id := range AllowDirectives(sql) {
		have[id] = true
	}
	var b strings.Builder
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for _, id := range sorted {
		if !have[id] {
			have[id] = true
			b.WriteString(allowDirective + id + "\n")
		}
	}
	return b.String() + sql
}

// writeAllowDirectives records acknowledged steps in the migration files a
// diff just wrote, then rehashes the directory. A failed hash restores both
// the files and atlas.sum (a failing hash may already have rewritten it), so
// the directory is left exactly as `atlas migrate diff` made it.
func writeAllowDirectives(ctx context.Context, opts Options, absWorkDir, migrationDir string, files []string, ids []string) error {
	if len(ids) == 0 || len(files) == 0 {
		return nil
	}
	sumPath := filepath.Join(migrationDir, "atlas.sum")
	prevSum, sumErr := os.ReadFile(sumPath) // #nosec G304 -- atlas.sum in the configured migration directory
	if sumErr != nil && !errors.Is(sumErr, os.ErrNotExist) {
		return fmt.Errorf("migrations: read atlas.sum: %w", sumErr)
	}
	originals := map[string][]byte{}
	for _, f := range files {
		data, err := os.ReadFile(f) // #nosec G304 -- a migration file Atlas just wrote in the configured directory
		if err != nil {
			return fmt.Errorf("migrations: read %s: %w", f, err)
		}
		originals[f] = data
		// #nosec G703 -- f is a migration file Atlas just wrote in the configured directory
		if err := os.WriteFile(f, []byte(withAllowDirectives(string(data), ids)), 0o600); err != nil {
			return fmt.Errorf("migrations: record acknowledgements in %s: %w", f, err)
		}
	}
	hashArgs := []string{"migrate", "hash", "--dir", "file://" + filepath.ToSlash(migrationDir)}
	hints := newHintWriter(opts.Stderr)
	err := opts.runner.Run(ctx, absWorkDir, opts.AtlasBinary, hashArgs, io.Discard, hints)
	hints.Flush()
	if err != nil {
		for f, data := range originals {
			// #nosec G703 -- restoring the file this function rewrote
			_ = os.WriteFile(f, data, 0o600)
		}
		restoreFile(sumPath, prevSum, sumErr == nil)
		return fmt.Errorf("migrations: atlas migrate hash after recording acknowledgements: %w", err)
	}
	return nil
}

// newMigrationFiles returns the up migrations in dir that are not in before.
func newMigrationFiles(dir string, before map[string]bool) ([]string, error) {
	files, err := ListMigrationFiles(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		if !before[f.UpPath] {
			out = append(out, f.UpPath)
		}
	}
	return out, nil
}

func migrationFileSet(dir string) (map[string]bool, error) {
	files, err := ListMigrationFiles(dir)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(files))
	for _, f := range files {
		set[f.UpPath] = true
	}
	return set, nil
}

// atlasRecoveryHints maps raw-Atlas recovery advice to the gombit command
// that does the same thing, so Atlas errors seen through gombit never send a
// user to the Atlas CLI.
var atlasRecoveryHints = strings.NewReplacer(
	"'atlas migrate hash'", "'gombit db hash'",
	"`atlas migrate hash`", "`gombit db hash`",
	"atlas migrate hash", "gombit db hash",
	"'atlas migrate validate'", "'gombit db lint'",
	"atlas migrate validate", "gombit db lint",
	"'atlas migrate lint'", "'gombit db lint'",
	"atlas migrate lint", "gombit db lint",
)

// translateAtlasHints rewrites raw-Atlas recovery advice in s.
func translateAtlasHints(s string) string {
	return atlasRecoveryHints.Replace(s)
}

// atlasMessage is Atlas's output without its Community Edition notice, with
// recovery advice translated.
func atlasMessage(s string) string {
	var keep []string
	notice := false
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "Notice: This Atlas edition"):
			notice = true
			continue
		case notice && t == "":
			continue
		case notice && (strings.HasPrefix(t, "testing,") || strings.HasPrefix(t, "triggers,") || strings.HasPrefix(t, "To install") || strings.HasPrefix(t, "curl ") || strings.HasPrefix(t, "Or, visit") || strings.HasPrefix(t, "https://atlasgo.io")):
			continue
		}
		notice = false
		if t != "" {
			keep = append(keep, t)
		}
	}
	return translateAtlasHints(strings.Join(keep, "\n"))
}

// hintWriter passes Atlas output through line by line with raw-Atlas
// recovery advice rewritten to the gombit command. Flush writes a trailing
// partial line.
type hintWriter struct {
	w   io.Writer
	buf []byte
}

func newHintWriter(w io.Writer) *hintWriter {
	return &hintWriter{w: w}
}

func (h *hintWriter) Write(p []byte) (int, error) {
	h.buf = append(h.buf, p...)
	for {
		i := bytes.IndexByte(h.buf, '\n')
		if i < 0 {
			break
		}
		if _, err := io.WriteString(h.w, translateAtlasHints(string(h.buf[:i+1]))); err != nil {
			return len(p), err
		}
		h.buf = h.buf[i+1:]
	}
	return len(p), nil
}

// Flush writes whatever partial line is left.
func (h *hintWriter) Flush() {
	if len(h.buf) > 0 {
		_, _ = io.WriteString(h.w, translateAtlasHints(string(h.buf)))
		h.buf = nil
	}
}
