package scaffold

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	// DefaultDatabase is the C1–C6 / D12 SQLite default.
	DefaultDatabase = "sqlite"
	// DefaultCache is the in-process memory cache default.
	DefaultCache = "memory"
	// DefaultAuth is Bearer JWT (C3). Cookie mode is first-class (M5-3).
	DefaultAuth = "jwt"
	// DefaultUI is minimal/headless (C4). --ui mui scaffolds the MUI CRUD preset.
	DefaultUI = "minimal"
	// DefaultAPIPrefix is D8.
	DefaultAPIPrefix = "/api/v1"
	// DefaultModulePrefix is the module path used when --module is omitted.
	DefaultModulePrefix = "github.com/example"

	generatedGoVersion = "1.26.0"
)

var (
	validDatabases = []string{"sqlite", "postgres", "mysql"}
	validCaches    = []string{"memory", "redis", "noop"}
	validAuths     = []string{"jwt", "cookie"}
	validUIs       = []string{"minimal", "mui"}
)

// Options configures application scaffolding.
type Options struct {
	Name     string
	Module   string
	Database string
	Cache    string
	Auth     string
	UI       string
	// FrameworkVersion is the gombit version the generated go.mod requires.
	// Empty means derive it from the running CLI; see ResolveFrameworkVersion.
	FrameworkVersion string
	// Tidy runs `go mod tidy` in the generated tree so it builds without
	// further steps. It reaches the network, so it is opt-in: the CLI enables
	// it, library callers and tests do not.
	Tidy    bool
	WorkDir string
	Dest    string
	DryRun  bool
	Force   bool
	// skipAtlas bypasses the initial bootstrap migration in tests that stub
	// go.sum population without a real go.mod/go.sum, mirroring resourcegen's
	// own skipAtlas seam.
	skipAtlas        bool
	Stdin            io.Reader
	Stdout           io.Writer
	Stderr           io.Writer
	IsTTY            func() bool
}

func (opts *Options) normalize() error {
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.WorkDir == "" {
		opts.WorkDir = "."
	}
	workDir, err := filepath.Abs(opts.WorkDir)
	if err != nil {
		return fmt.Errorf("scaffold: resolve work dir: %w", err)
	}
	opts.WorkDir = workDir
	opts.Name = strings.TrimSpace(opts.Name)
	opts.Module = strings.TrimSpace(opts.Module)
	opts.Database = strings.ToLower(strings.TrimSpace(opts.Database))
	opts.Cache = strings.ToLower(strings.TrimSpace(opts.Cache))
	opts.Auth = strings.ToLower(strings.TrimSpace(opts.Auth))
	opts.UI = strings.ToLower(strings.TrimSpace(opts.UI))
	opts.FrameworkVersion = strings.TrimSpace(opts.FrameworkVersion)
	if opts.Database == "" {
		opts.Database = DefaultDatabase
	}
	if opts.Cache == "" {
		opts.Cache = DefaultCache
	}
	if opts.Auth == "" {
		opts.Auth = DefaultAuth
	}
	if opts.UI == "" {
		opts.UI = DefaultUI
	}
	return nil
}

func (opts *Options) validate() error {
	if err := validateName(opts.Name); err != nil {
		return err
	}
	if err := validateChoice("database", opts.Database, validDatabases); err != nil {
		return err
	}
	if err := validateChoice("cache", opts.Cache, validCaches); err != nil {
		return err
	}
	if err := validateChoice("auth", opts.Auth, validAuths); err != nil {
		return err
	}
	if err := validateChoice("ui", opts.UI, validUIs); err != nil {
		return err
	}
	if opts.Module == "" {
		opts.Module = DefaultModulePrefix + "/" + opts.Name
	}
	if err := validateModule(opts.Module); err != nil {
		return err
	}
	if opts.Dest == "" {
		opts.Dest = filepath.Join(opts.WorkDir, opts.Name)
	}
	return nil
}

func validateName(name string) error {
	if name == "" {
		return errors.New("scaffold: project name is required")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("scaffold: invalid project name %q", name)
	}
	if strings.ContainsAny(name, `/\`) || filepath.IsAbs(name) {
		return fmt.Errorf("scaffold: project name must be a single directory segment, got %q", name)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("scaffold: invalid project name %q", name)
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
			if i == 0 {
				return fmt.Errorf("scaffold: project name %q must start with a letter", name)
			}
		case r == '-' || r == '_':
			if i == 0 {
				return fmt.Errorf("scaffold: project name %q must start with a letter", name)
			}
		default:
			if unicode.IsSpace(r) {
				return fmt.Errorf("scaffold: project name %q must not contain whitespace", name)
			}
			return fmt.Errorf("scaffold: project name %q contains invalid character %q", name, string(r))
		}
	}
	return nil
}

func validateModule(module string) error {
	if module == "" {
		return errors.New("scaffold: module path is required")
	}
	if strings.Contains(module, "\\") || strings.Contains(module, " ") {
		return fmt.Errorf("scaffold: invalid module path %q", module)
	}
	for _, part := range strings.Split(module, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("scaffold: invalid module path %q", module)
		}
	}
	return nil
}

func validateChoice(field, value string, allowed []string) error {
	for _, item := range allowed {
		if value == item {
			return nil
		}
	}
	return fmt.Errorf("scaffold: %s must be one of %s, got %q", field, strings.Join(allowed, ", "), value)
}

func stdinIsTTY(r io.Reader) bool {
	file, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
