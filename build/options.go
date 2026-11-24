package build

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	// DefaultOut is the server binary path when --out is omitted.
	DefaultOut = "bin/server"

	frontendPkgRel  = "frontend/package.json"
	frontendDirRel  = "frontend"
	frontendDistRel = "frontend/dist"
	staticRel       = "internal/web/static"
	embedGoRel      = "internal/web/embed.go"
	serverPkg       = "./cmd/server"
	keepName        = ".keep"
)

// CommandFunc starts a subprocess. Tests replace this with short-lived fakes.
type CommandFunc func(name string, args ...string) *exec.Cmd

// LookPathFunc locates an executable. Tests replace this to simulate npm/go.
type LookPathFunc func(file string) (string, error)

// Options configures `gombit build --embed`.
type Options struct {
	WorkDir  string
	Out      string
	Embed    bool
	DryRun   bool
	Stdout   io.Writer
	Stderr   io.Writer
	LookPath LookPathFunc
	Command  CommandFunc
}

// DefaultOutput is the server binary path when --out is omitted for the
// target OS: DefaultOut, with .exe appended when building for Windows. go build
// only appends .exe when -o names a directory, so the extension is added here.
func DefaultOutput() string {
	if targetOS() == "windows" {
		return DefaultOut + ".exe"
	}
	return DefaultOut
}

// targetOS is the GOOS the build compiles for: the GOOS environment variable
// when set (cross-compiling), otherwise the host OS.
func targetOS() string {
	if goos := strings.TrimSpace(os.Getenv("GOOS")); goos != "" {
		return goos
	}
	return runtime.GOOS
}

func (opts *Options) normalize() error {
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	if opts.LookPath == nil {
		opts.LookPath = exec.LookPath
	}
	if opts.Command == nil {
		opts.Command = exec.Command
	}
	if opts.WorkDir == "" {
		opts.WorkDir = "."
	}
	abs, err := filepath.Abs(opts.WorkDir)
	if err != nil {
		return fmt.Errorf("build: resolve work dir: %w", err)
	}
	opts.WorkDir = abs
	if strings.TrimSpace(opts.Out) == "" {
		opts.Out = DefaultOutput()
	}
	return nil
}

func (opts Options) validate() error {
	if strings.TrimSpace(opts.Out) == "" {
		return errors.New("build: --out must not be empty")
	}
	if strings.Contains(opts.Out, "..") {
		return errors.New("build: --out must not contain path traversal")
	}
	return nil
}
