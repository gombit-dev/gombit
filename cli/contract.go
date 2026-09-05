package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gombit-dev/gombit/appcontract"
	"github.com/spf13/cobra"
)

func newContractCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "contract",
		Short: "Application contract commands",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			contractUsage(stderr)
			if len(args) == 0 {
				return errors.New("gombit contract: subcommand is required")
			}
			return fmt.Errorf("gombit contract: unknown subcommand %q", args[0])
		},
	})
	cmd.AddCommand(newContractAppCommand(stdout, stderr))
	return cmd
}

func newContractAppCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "app",
		Short: "Emit the machine-readable application contract (HOST-1)",
		Long: `Emit the machine-readable application contract (ADR-015 / HOST-1).

The contract describes how a deployment host builds, health-checks, and
migrates this app. Every field is projected from declared configuration
(config/.env and the framework version in go.mod) — nothing is inferred from
the source tree. --dir selects the project directory that both the config
(.env) and go.mod are read from. Writes JSON to stdout, or to --out.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("gombit contract app: unexpected argument %q", args[0])
			}
			dir, err := cmd.Flags().GetString("dir")
			if err != nil {
				return err
			}
			out, err := cmd.Flags().GetString("out")
			if err != nil {
				return err
			}
			return runContractApp(stdout, dir, out)
		},
	})
	cmd.Flags().String("dir", ".", "project directory both go.mod and config (.env) are read from")
	cmd.Flags().String("out", "", "output path for the contract JSON (default: stdout)")
	return cmd
}

func runContractApp(stdout io.Writer, dir string, out string) error {
	// Resolve the output path before changing directory so a relative --out
	// stays relative to the caller's cwd, not the project dir.
	if out != "" {
		abs, err := filepath.Abs(out)
		if err != nil {
			return fmt.Errorf("gombit contract app: resolve --out: %w", err)
		}
		out = abs
	}

	// Read config (.env) and go.mod from the same project directory so the
	// contract cannot mix one app's go.mod with another's config.
	restore, err := chdir(dir)
	if err != nil {
		return fmt.Errorf("gombit contract app: %w", err)
	}
	defer restore()

	cfg, err := LoadConfig()
	if err != nil {
		return err
	}

	version, err := appcontract.FrameworkVersion(".")
	if err != nil {
		if errors.Is(err, appcontract.ErrFrameworkVersionUnresolved) {
			return fmt.Errorf("gombit contract app: %w; a host cannot pin a replaced/local "+
				"framework version — build against a published %s release", err, appcontract.FrameworkModulePath)
		}
		return fmt.Errorf("gombit contract app: %w", err)
	}

	contract, err := appcontract.Project(appcontract.Inputs{
		FrameworkVersion: version,
		HTTPAddr:         cfg.HTTP.Addr,
		DatabaseDriver:   string(cfg.Database.Driver),
		DatabaseRequired: cfg.Database.Required,
	})
	if err != nil {
		return fmt.Errorf("gombit contract app: %w", err)
	}

	data, err := json.MarshalIndent(contract, "", "  ")
	if err != nil {
		return fmt.Errorf("gombit contract app: marshal contract: %w", err)
	}
	data = append(data, '\n')

	if out == "" {
		_, err = stdout.Write(data)
		return err
	}
	if err := os.WriteFile(out, data, 0o600); err != nil {
		return fmt.Errorf("gombit contract app: write %s: %w", out, err)
	}
	_, err = fmt.Fprintf(stdout, "wrote application contract to %s\n", out)
	return err
}

// chdir changes into dir and returns a function that restores the previous
// working directory. A "" or "." dir is a no-op so the caller stays in cwd.
func chdir(dir string) (func(), error) {
	if dir == "" || dir == "." {
		return func() {}, nil
	}
	prev, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if err := os.Chdir(dir); err != nil {
		return nil, err
	}
	return func() { _ = os.Chdir(prev) }, nil
}

func contractUsage(stderr io.Writer) {
	_, _ = fmt.Fprintln(stderr, "Usage: gombit contract <subcommand>")
	_, _ = fmt.Fprintln(stderr, "  app    Emit the machine-readable application contract")
}
