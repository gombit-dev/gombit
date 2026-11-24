package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/gombit-dev/gombit/appcontract"
	"github.com/gombit-dev/gombit/framework"
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
migrates this app, and which processes it runs from the build (the web server,
and the jobs worker when the jobs driver needs one) with how long each needs to
stop. Every field is projected from declared configuration (config/.env and
the framework version in go.mod) — nothing is inferred from the source tree.
--dir selects the project directory that both the config (.env) and go.mod are
read from; the process environment wins over .env, as it does at runtime, so a
host that injects configuration runs this with the environment it will start
the app with. Writes JSON to stdout, or to --out.`,
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
	// Read config (.env) and go.mod from the same project directory so the
	// contract cannot mix one app's go.mod with another's config — without
	// mutating the process (no chdir), which would race parallel callers.
	cfg, err := LoadConfigFromDir(dir)
	if err != nil {
		return err
	}

	version, err := appcontract.FrameworkVersion(dir)
	if err != nil {
		if errors.Is(err, appcontract.ErrFrameworkVersionUnresolved) {
			return fmt.Errorf("gombit contract app: %w; a host cannot pin a replaced/local "+
				"framework version — build against a published %s release", err, appcontract.FrameworkModulePath)
		}
		return fmt.Errorf("gombit contract app: %w", err)
	}

	in := appcontract.Inputs{
		FrameworkVersion: version,
		HTTPAddr:         cfg.HTTP.Addr,
		DatabaseDriver:   string(cfg.Database.Driver),
		DatabaseRequired: cfg.Database.Required,
		WebStopTimeout:   framework.WebKillAfter(cfg),
		RedisRequired:    cfg.RequiresRedis(),
	}
	if cfg.Jobs.Driver.NeedsWorker() {
		// A host starts the worker with no flags: it runs on the declared
		// settings, the ones this stop timeout is computed from.
		in.Worker = true
		in.WorkerStopTimeout = framework.WorkerKillAfterFor(cfg, nil)
	}
	contract, err := appcontract.Project(in)
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

func contractUsage(stderr io.Writer) {
	_, _ = fmt.Fprintln(stderr, "Usage: gombit contract <subcommand>")
	_, _ = fmt.Fprintln(stderr, "  app    Emit the machine-readable application contract")
}
