package dev

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gombit-dev/gombit/client"
	"github.com/gombit-dev/gombit/config"
)

const (
	// DefaultHTTPAddr is the Go API listen address when config and flags omit it.
	DefaultHTTPAddr = ":8080"
	// DefaultFrontendHost is the Vite bind host.
	DefaultFrontendHost = "127.0.0.1"
	// DefaultFrontendPort is Vite's conventional port.
	DefaultFrontendPort = 5173
	// DefaultPollInterval is how often the OpenAPI watcher fetches /openapi.json.
	DefaultPollInterval = time.Second
	// DefaultClientOut is the generated TypeScript client directory (design §23.3).
	DefaultClientOut = client.DefaultOutDir
)

// CommandFunc starts a subprocess. Tests replace this with short-lived fakes.
type CommandFunc func(name string, args ...string) *exec.Cmd

// LookPathFunc locates an executable. Tests replace this to simulate air/pnpm.
type LookPathFunc func(file string) (string, error)

// HTTPGetFunc fetches a URL. Tests replace this to stub /openapi.json.
type HTTPGetFunc func(ctx context.Context, rawURL string) ([]byte, error)

// GenerateFunc regenerates the TypeScript client from an OpenAPI document.
type GenerateFunc func(ctx context.Context, spec []byte) error

// Options configures `gombit dev`.
type Options struct {
	WorkDir      string
	HTTPAddr     string
	APIPrefix    string
	FrontendHost string
	FrontendPort int
	ClientOut    string
	PollInterval time.Duration
	Stdout       io.Writer
	Stderr       io.Writer
	LookPath     LookPathFunc
	Command      CommandFunc
	HTTPGet      HTTPGetFunc
	Generate     GenerateFunc
	ShutdownWait time.Duration
	// AdminURL is printed in the service table when non-empty (cookie-mode
	// apps that mount the framework admin SPA). JWT apps leave this empty.
	AdminURL string
	// onCmdReady is an in-package test seam; see runProcesses. Always nil
	// outside dev's own tests.
	onCmdReady func(*exec.Cmd)
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
		return fmt.Errorf("dev: resolve work dir: %w", err)
	}
	opts.WorkDir = abs
	if opts.HTTPAddr == "" {
		opts.HTTPAddr = DefaultHTTPAddr
	}
	if opts.APIPrefix == "" {
		opts.APIPrefix = "/api/v1"
	}
	if opts.FrontendHost == "" {
		opts.FrontendHost = DefaultFrontendHost
	}
	if opts.FrontendPort == 0 {
		opts.FrontendPort = DefaultFrontendPort
	}
	if opts.ClientOut == "" {
		opts.ClientOut = DefaultClientOut
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.ShutdownWait == 0 {
		opts.ShutdownWait = 3 * time.Second
	}
	return nil
}

// ValidateFlags reports user-facing errors for `gombit dev` flag values
// before defaults are applied.
func ValidateFlags(httpAddr string, frontendHost string, frontendPort int, poll time.Duration, clientOut string) error {
	if strings.TrimSpace(httpAddr) == "" {
		return errors.New("dev: --http must not be empty")
	}
	if _, err := parseListenAddr(httpAddr); err != nil {
		return fmt.Errorf("dev: --http: %w", err)
	}
	if strings.TrimSpace(frontendHost) == "" {
		return errors.New("dev: --frontend-host must not be empty")
	}
	if frontendPort < 1 || frontendPort > 65535 {
		return fmt.Errorf("dev: --frontend-port must be between 1 and 65535, got %d", frontendPort)
	}
	if poll <= 0 {
		return errors.New("dev: --poll must be greater than zero")
	}
	if strings.TrimSpace(clientOut) == "" {
		return errors.New("dev: --client-out must not be empty")
	}
	return nil
}

func (opts Options) validate() error {
	return ValidateFlags(opts.HTTPAddr, opts.FrontendHost, opts.FrontendPort, opts.PollInterval, opts.ClientOut)
}

// HTTPAddrFromConfig returns the listen address from cfg, or DefaultHTTPAddr.
func HTTPAddrFromConfig(cfg config.Config) string {
	addr := strings.TrimSpace(cfg.HTTP.Addr)
	if addr == "" {
		return DefaultHTTPAddr
	}
	return addr
}

// APIPrefixFromConfig returns config.API.Prefix, or D8 `/api/v1`.
func APIPrefixFromConfig(cfg config.Config) string {
	prefix := strings.TrimSuffix(strings.TrimSpace(cfg.API.Prefix), "/")
	if prefix == "" {
		return "/api/v1"
	}
	return prefix
}

// AdminURLFromConfig returns the admin SPA URL for cookie-mode apps with
// auth enabled. JWT apps and auth-disabled configs return "".
func AdminURLFromConfig(cfg config.Config, httpAddr string) string {
	if !cfg.Auth.Enabled() || cfg.Auth.EffectiveMode() != config.AuthModeCookie {
		return ""
	}
	return originFromAddr(httpAddr) + "/admin/"
}

func parseListenAddr(addr string) (hostPort string, err error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", errors.New("address must not be empty")
	}
	if !strings.Contains(addr, ":") {
		addr = ":" + addr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", fmt.Errorf("invalid port %q", port)
	}
	if host == "" && port == "" {
		return "", errors.New("address must not be empty")
	}
	return addr, nil
}

func originFromAddr(addr string) string {
	normalized, err := parseListenAddr(addr)
	if err != nil {
		normalized = DefaultHTTPAddr
	}
	host, port, err := net.SplitHostPort(normalized)
	if err != nil {
		return "http://127.0.0.1:8080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func frontendOrigin(host string, port int) string {
	display := host
	if display == "" || display == "0.0.0.0" || display == "::" || display == "[::]" {
		display = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(display, strconv.Itoa(port))
}
