package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/gombit-dev/gombit/dev"
	"github.com/spf13/cobra"
)

func newDevCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	var (
		httpAddr     string
		frontendHost string
		frontendPort int
		clientOut    string
		poll         time.Duration
	)
	cmd := silence(&cobra.Command{
		Use:   "dev",
		Short: "Run the API and Vite frontend together",
		Long: `Run the Go API and Vite frontend with HMR from an application directory.

Backend reload uses air when it is on PATH, then watchexec, then falls back
to go run ./cmd/server (with a hint). The frontend uses pnpm when available
(or when frontend/pnpm-lock.yaml exists), otherwise npm.

Vite proxies /api, /openapi.json, /docs, and /admin to the Go origin.
Prefixes that do not start with /api (for example /svc/v2) get an extra
proxy entry from the live GOMBIT_API_PREFIX. When the live OpenAPI document
changes, gombit regenerates frontend/src/api/generated (the same output as
gombit client generate).

A service table is printed at startup, including the API docs URL (/docs).
Cookie-mode apps also print the Admin SPA URL (/admin/).

SIGINT/SIGTERM stops the child processes. air and watchexec are optional
(go run fallback). Node.js is required for Vite HMR; a missing
frontend/package.json is an error (backend-only is not supported).

The generated frontend is a Vite + React + TypeScript skeleton
(router, generated client, React Hook Form). Bearer login is the
default; --auth cookie is the CSRF/session preset. --ui mui is the
opt-in MUI CRUD preset (default UI stays minimal). Split deploy is the
default; gombit build --embed is the optional single-binary SPA path.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			apiPrefix := "/api/v1"
			adminURL := ""
			cfg, cfgErr := LoadConfig()
			httpFromFlag := cmd.Flags().Changed("http")
			if !httpFromFlag {
				if cfgErr != nil {
					return fmt.Errorf("gombit dev: %w", cfgErr)
				}
				httpAddr = dev.HTTPAddrFromConfig(cfg)
			}
			if cfgErr == nil {
				apiPrefix = dev.APIPrefixFromConfig(cfg)
				adminURL = dev.AdminURLFromConfig(cfg, httpAddr)
			}
			// ValidateFlags labels the address --http, which is wrong for one
			// read from configuration: name GOMBIT_HTTP_ADDR instead (#513).
			if !httpFromFlag {
				if err := dev.HTTPAddrProblem("GOMBIT_HTTP_ADDR", httpAddr); err != nil {
					return fmt.Errorf("gombit %w", err)
				}
			}
			if err := dev.ValidateFlags(httpAddr, frontendHost, frontendPort, poll, clientOut); err != nil {
				return fmt.Errorf("gombit %w", err)
			}
			err := RunDev(cmd.Context(), dev.Options{
				WorkDir:      ".",
				HTTPAddr:     httpAddr,
				APIPrefix:    apiPrefix,
				AdminURL:     adminURL,
				FrontendHost: frontendHost,
				FrontendPort: frontendPort,
				ClientOut:    clientOut,
				PollInterval: poll,
				Stdout:       stdout,
				Stderr:       stderr,
			})
			if err != nil {
				return fmt.Errorf("gombit %w", err)
			}
			return nil
		},
	})
	cmd.Flags().StringVar(&httpAddr, "http", dev.DefaultHTTPAddr, "Go API listen address (default GOMBIT_HTTP_ADDR or :8080)")
	cmd.Flags().StringVar(&frontendHost, "frontend-host", dev.DefaultFrontendHost, "Vite bind host")
	cmd.Flags().IntVar(&frontendPort, "frontend-port", dev.DefaultFrontendPort, "Vite port")
	cmd.Flags().StringVar(&clientOut, "client-out", dev.DefaultClientOut, "TypeScript client output directory")
	cmd.Flags().DurationVar(&poll, "poll", dev.DefaultPollInterval, "OpenAPI poll interval")
	return cmd
}

// RunDev starts the API and Vite frontend. Tests may replace it.
var RunDev = dev.Run
