package scaffold

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"go/format"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/upgrade"
)

// Generate scaffolds a new Gombit application into opts.Dest (or <workdir>/<name>).
func Generate(ctx context.Context, opts Options) error {
	if ctx == nil {
		return errors.New("scaffold: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := opts.normalize(); err != nil {
		return err
	}
	if err := opts.resolveInteractive(); err != nil {
		return err
	}
	if err := opts.validate(); err != nil {
		return err
	}

	if err := checkDestination(opts); err != nil {
		return err
	}

	frameworkVersion, fallbackReason := ResolveFrameworkVersion(opts.FrameworkVersion)
	if fallbackReason != "" {
		if err := warnUnresolvableFramework(opts.Stderr, opts.Module, fallbackReason); err != nil {
			return err
		}
	}

	vars := templateVars{
		Name:             opts.Name,
		Module:           opts.Module,
		Database:         opts.Database,
		Cache:            opts.Cache,
		Auth:             opts.Auth,
		UI:               opts.UI,
		APIPrefix:        DefaultAPIPrefix,
		DatabaseDSN:      defaultDSN(opts.Database, opts.Name),
		CacheNamespace:   config.DefaultCacheNamespace(opts.Name, config.EnvironmentDevelopment),
		GoVersion:        generatedGoVersion,
		FrameworkVersion: frameworkVersion,
		UpgradeMetadata:  upgrade.MetadataBlock(upgrade.ScaffoldVersion),
	}

	files, err := renderFiles(vars)
	if err != nil {
		return err
	}
	secret, err := newJWTSecret()
	if err != nil {
		return err
	}
	files, err = withGeneratedDotEnv(files, secret)
	if err != nil {
		return err
	}

	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		full := filepath.Join(opts.Dest, filepath.FromSlash(file.relPath))
		action := "create"
		if _, err := os.Stat(full); err == nil {
			action = "modify"
		}
		display, err := displayPath(opts.WorkDir, full)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(opts.Stdout, "%s %s\n", action, display); err != nil {
			return err
		}
		if opts.DryRun {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			return fmt.Errorf("scaffold: mkdir %s: %w", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, file.content, 0o600); err != nil {
			return fmt.Errorf("scaffold: write %s: %w", display, err)
		}
	}

	// A pinned version alone is not enough to build: without go.sum, `go build`
	// fails with "missing go.sum entry" rather than fetching. Only worth
	// attempting when the pin is resolvable.
	if opts.Tidy && !opts.DryRun && fallbackReason == "" {
		tidied, err := tidyModule(ctx, opts)
		if err != nil {
			return err
		}
		// The bootstrap migration shells out to `go run` on a loader that
		// imports the module's own dependencies; without a tidied go.sum
		// that fails, so only attempt it once the module actually resolves.
		if tidied && !opts.skipAtlas {
			if err := seedBootstrapMigration(ctx, opts); err != nil {
				return err
			}
		}
	}
	return nil
}

// goModTidy runs `go mod tidy` in dir. Indirected for tests, which must not
// reach the network.
var goModTidy = func(ctx context.Context, dir string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "go", "mod", "tidy")
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

// tidyModule populates go.sum so the generated app builds as-is. A failure
// here (no network, no toolchain) is reported but not fatal: the tree is
// already written and correct, and the user can rerun the command themselves.
// The returned bool reports whether go.sum is now actually usable.
func tidyModule(ctx context.Context, opts Options) (bool, error) {
	if _, err := fmt.Fprintln(opts.Stdout, "go mod tidy"); err != nil {
		return false, err
	}
	output, err := goModTidy(ctx, opts.Dest)
	if err == nil {
		return true, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, ctxErr
	}
	_, writeErr := fmt.Fprintf(opts.Stderr,
		"warning: go mod tidy failed: %v\n%s  %s will not build until you run it yourself:\n    cd %s && go mod tidy\n",
		err, indentOutput(output), opts.Module, opts.Name)
	return false, writeErr
}

func indentOutput(output []byte) string {
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return ""
	}
	var b strings.Builder
	for _, line := range strings.Split(trimmed, "\n") {
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

type templateVars struct {
	Name             string
	Module           string
	Database         string
	Cache            string
	Auth             string
	UI               string
	APIPrefix        string
	DatabaseDSN      string
	CacheNamespace   string
	GoVersion        string
	FrameworkVersion string
	// UpgradeMetadata is gombit.yaml's upgrade baseline block: the scaffold
	// conventions this app is generated with (upgrade.ScaffoldVersion).
	UpgradeMetadata string
}

// warnUnresolvableFramework explains why the generated go.mod pins a version
// the module proxy cannot resolve, and what to do about it. Without this the
// user's first `go build` fails with an opaque "missing go.sum entry".
func warnUnresolvableFramework(w io.Writer, module, reason string) error {
	_, err := fmt.Fprintf(w, `warning: %s, so go.mod requires %s %s, which the module proxy cannot resolve.
  %s will not build until you point it at a framework checkout:
    go mod edit -replace %s=/path/to/gombit
    go mod tidy
  Installing a released gombit (go install %s/cmd/gombit@latest) avoids this.
`, reason, frameworkModulePath, FallbackFrameworkVersion, module, frameworkModulePath, frameworkModulePath)
	return err
}

type renderedFile struct {
	relPath string
	content []byte
}

func renderFiles(vars templateVars) ([]renderedFile, error) {
	var files []renderedFile
	err := fs.WalkDir(templateFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel("templates", path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		rel = strings.TrimSuffix(rel, ".tmpl")
		rel = rewriteTemplateName(rel)
		if skipMUIOnlyTemplate(rel, vars.UI) {
			return nil
		}
		// #nosec G304 -- path is a rooted embed.FS entry from WalkDir
		raw, err := templateFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("scaffold: read template %s: %w", path, err)
		}
		content, err := executeTemplate(path, string(raw), vars)
		if err != nil {
			return err
		}
		if strings.HasSuffix(rel, ".go") {
			formatted, err := format.Source(content)
			if err != nil {
				return fmt.Errorf("scaffold: format %s: %w", rel, err)
			}
			content = formatted
		}
		files = append(files, renderedFile{relPath: rel, content: content})
		return nil
	})
	if err != nil {
		return nil, err
	}
	files = append(files,
		renderedFile{relPath: "database/migrations/.gitkeep", content: []byte("")},
		renderedFile{relPath: "database/seeds/.gitkeep", content: []byte("")},
		// .keep (not a template) so go:embed all:static compiles after gombit
		// new. No index.html: framework.WithEmbeddedFrontend is a no-op until
		// gombit build --embed collectstatics frontend/dist here.
		renderedFile{relPath: "internal/web/static/.keep", content: []byte("")},
	)
	sort.Slice(files, func(i, j int) bool {
		return files[i].relPath < files[j].relPath
	})
	return files, nil
}

// skipMUIOnlyTemplate keeps MUI-only files (theme.ts) out of the default
// minimal tree so `gombit new` never writes @mui imports or a theme module.
func skipMUIOnlyTemplate(rel, ui string) bool {
	if ui == "mui" {
		return false
	}
	switch rel {
	case "frontend/src/theme.ts":
		return true
	default:
		return false
	}
}

func rewriteTemplateName(rel string) string {
	switch rel {
	case "env.example":
		return ".env.example"
	case "gitignore":
		return ".gitignore"
	case "air.toml":
		return ".air.toml"
	default:
		return rel
	}
}

func executeTemplate(name, src string, vars templateVars) ([]byte, error) {
	tmpl, err := template.New(name).Parse(src)
	if err != nil {
		return nil, fmt.Errorf("scaffold: parse template %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return nil, fmt.Errorf("scaffold: execute template %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

func newJWTSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("scaffold: generate JWT secret: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// envExampleJWTComment is the comment block .env.example carries above its
// GOMBIT_JWT_SECRET line. It is accurate there: DevelopmentJWTPlaceholder
// really is shorter than MinProductionJWTSecretLength. withGeneratedDotEnv
// swaps it for dotEnvJWTComment in the generated .env, where the value is a
// full-length random secret instead, so the same claim would be false.
const envExampleJWTComment = "" +
	"# HMAC secret shared by Bearer access JWTs and (in cookie mode) the CSRF\n" +
	"# double-submit signature. This development placeholder is shorter than 32\n" +
	"# characters and is rejected in production (Appendix C). `gombit new` also\n" +
	"# writes a gitignored `.env` with a per-project random secret. Never put\n" +
	"# this in VITE_*.\n"

// dotEnvJWTComment replaces envExampleJWTComment in the generated .env.
const dotEnvJWTComment = "" +
	"# HMAC secret shared by Bearer access JWTs and (in cookie mode) the CSRF\n" +
	"# double-submit signature. gombit new generated this as a random,\n" +
	"# per-project secret — do not commit it or reuse it across environments.\n" +
	"# Never put this in VITE_*.\n"

func withGeneratedDotEnv(files []renderedFile, secret string) ([]renderedFile, error) {
	var example []byte
	for _, file := range files {
		if file.relPath == ".env.example" {
			example = file.content
			break
		}
	}
	if example == nil {
		return nil, errors.New("scaffold: missing .env.example")
	}
	placeholder := "GOMBIT_JWT_SECRET=" + config.DevelopmentJWTPlaceholder
	replaced := bytes.Replace(example, []byte(placeholder), []byte("GOMBIT_JWT_SECRET="+secret), 1)
	if bytes.Equal(replaced, example) {
		return nil, errors.New("scaffold: JWT placeholder missing from .env.example")
	}
	commentReplaced := bytes.Replace(replaced, []byte(envExampleJWTComment), []byte(dotEnvJWTComment), 1)
	if bytes.Equal(commentReplaced, replaced) {
		return nil, errors.New("scaffold: JWT comment block missing from .env.example")
	}
	files = append(files, renderedFile{relPath: ".env", content: commentReplaced})
	sort.Slice(files, func(i, j int) bool {
		return files[i].relPath < files[j].relPath
	})
	return files, nil
}

func checkDestination(opts Options) error {
	info, err := os.Stat(opts.Dest)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("scaffold: stat dest: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("scaffold: destination %q exists and is not a directory", opts.Dest)
	}
	entries, err := os.ReadDir(opts.Dest)
	if err != nil {
		return fmt.Errorf("scaffold: read dest: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}
	if opts.Force {
		return nil
	}
	return fmt.Errorf("scaffold: destination %q is not empty; pass --force to overwrite", opts.Dest)
}

func defaultDSN(driver, name string) string {
	switch driver {
	case "postgres":
		return "postgres://USER:PASSWORD@127.0.0.1:5432/" + name + "?sslmode=disable"
	case "mysql":
		return "USER:PASSWORD@tcp(127.0.0.1:3306)/" + name + "?parseTime=true"
	default:
		return "file:gombit.db?cache=shared&_fk=1"
	}
}

func displayPath(workDir, full string) (string, error) {
	rel, err := filepath.Rel(workDir, full)
	if err != nil {
		return "", fmt.Errorf("scaffold: relative path: %w", err)
	}
	return filepath.ToSlash(rel), nil
}
