package cli

import (
	"fmt"
	"io"

	"github.com/gombit-dev/gombit/build"
	"github.com/spf13/cobra"
)

func newBuildCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	var (
		embed  bool
		out    string
		dryRun bool
	)
	cmd := silence(&cobra.Command{
		Use:   "build",
		Short: "Build the application (embed is opt-in)",
		Long: `Build a production server binary from an application directory.

Split deploy is the default (C5). A bare gombit build without --embed is
refused and does not change production to embed. Pass --embed to:

  1. run the Vite production build in frontend/ (pnpm when frontend/pnpm-lock.yaml
     exists, otherwise npm; pnpm only if npm is absent)
  2. collectstatic frontend/dist into internal/web/static
  3. go build ./cmd/server

The resulting binary serves Huma /api/*, probes, static assets, and SPA
index.html fallback for unmatched GET frontend routes. go run ./cmd/server
after gombit new still works without a Vite dist: the placeholder embed
has no index.html, so the runtime does not install SPA fallback.

--dry-run prints the plan without writing or compiling.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !embed {
				return fmt.Errorf("gombit %w", build.ErrEmbedRequired)
			}
			err := RunBuild(cmd.Context(), build.Options{
				WorkDir: ".",
				Out:     out,
				Embed:   true,
				DryRun:  dryRun,
				Stdout:  stdout,
				Stderr:  stderr,
			})
			if err != nil {
				return fmt.Errorf("gombit %w", err)
			}
			return nil
		},
	})
	cmd.Flags().BoolVar(&embed, "embed", false, "embed the Vite frontend into the server binary (required to run this command; split deploy stays the default)")
	cmd.Flags().StringVar(&out, "out", build.DefaultOut, "output binary path")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan without writing or compiling")
	return cmd
}

// RunBuild performs gombit build --embed. Tests may replace it.
var RunBuild = build.Run
