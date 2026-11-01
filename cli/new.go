package cli

import (
	"fmt"
	"io"

	"github.com/gombit-dev/gombit/scaffold"
	"github.com/spf13/cobra"
)

func newNewCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	var (
		database         string
		cache            string
		auth             string
		ui               string
		module           string
		frameworkVersion string
		dryRun           bool
		force            bool
		skipTidy         bool
	)
	cmd := silence(&cobra.Command{
		Use:   "new [name]",
		Short: "Scaffold a new Gombit application",
		Long: `Scaffold a new Gombit application using the feature-package layout.

Non-interactive example:

  gombit new demo --database sqlite

Defaults: --database sqlite, --cache memory, --auth jwt, --ui minimal.
The Go module path defaults to github.com/example/<name> (--module).
--auth cookie scaffolds HttpOnly session cookies and CSRF (see
docs/auth-cookie.md). --ui mui scaffolds the MUI CRUD preset
(ThemeProvider, AppBar, Table, TextField); the default UI stays
minimal/headless. The generated frontend is a Vite + React skeleton.
Split deploy is the default (C5); gombit build --embed is the optional
single-binary path. --dry-run prints the file list without writing. A
non-empty destination requires --force.

The generated go.mod requires the same gombit version as the binary that
scaffolded it, so an installed release produces a tree that resolves from
the module proxy, and go mod tidy then runs to populate go.sum — the app
builds with no further steps. Use --skip-tidy to stay offline.

A source build reports "dev", which is not resolvable: the tree is pinned
to v0.0.0, tidy is skipped, and the app needs a replace directive pointing
at your framework checkout. Override with --framework-version.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if frameworkVersion == "" {
				// Pin the generated app to the gombit that generated it, so a
				// released binary produces a tree that resolves from the proxy.
				frameworkVersion = resolveVersion()
			}
			opts := scaffold.Options{
				Database:         database,
				Cache:            cache,
				Auth:             auth,
				UI:               ui,
				Module:           module,
				FrameworkVersion: frameworkVersion,
				Tidy:             !skipTidy,
				DryRun:           dryRun,
				Force:            force,
				WorkDir:          ".",
				Stdin:            cmd.InOrStdin(),
				Stdout:           stdout,
				Stderr:           stderr,
			}
			if len(args) == 1 {
				opts.Name = args[0]
			}
			if err := scaffold.Generate(cmd.Context(), opts); err != nil {
				return fmt.Errorf("gombit new: %w", err)
			}
			return nil
		},
	})
	cmd.Flags().StringVar(&database, "database", scaffold.DefaultDatabase, "database driver: sqlite, postgres, or mysql")
	cmd.Flags().StringVar(&cache, "cache", scaffold.DefaultCache, "cache driver: memory, redis, or noop")
	cmd.Flags().StringVar(&auth, "auth", scaffold.DefaultAuth, "auth mode: jwt (Bearer default) or cookie (HttpOnly session + CSRF)")
	cmd.Flags().StringVar(&ui, "ui", scaffold.DefaultUI, "UI preset: minimal (headless default) or mui (MUI CRUD screens)")
	cmd.Flags().StringVar(&module, "module", "", "Go module path (default github.com/example/<name>)")
	cmd.Flags().StringVar(&frameworkVersion, "framework-version", "",
		"gombit version the generated go.mod requires (default: this binary's version)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print files that would be written without writing")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite files in a non-empty destination")
	cmd.Flags().BoolVar(&skipTidy, "skip-tidy", false, "do not run go mod tidy in the generated app (no network)")
	return cmd
}
