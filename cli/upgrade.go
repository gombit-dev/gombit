package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/gombit-dev/gombit/upgrade"
)

func newUpgradeCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "upgrade",
		Short: "Framework upgrade commands",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			upgradeUsage(stderr)
			if len(args) == 0 {
				return errors.New("gombit upgrade: subcommand is required")
			}
			return fmt.Errorf("gombit upgrade: unknown subcommand %q", args[0])
		},
	})
	cmd.AddCommand(newUpgradeBaselineCommand(stdout))
	return cmd
}

func newUpgradeBaselineCommand(stdout io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "baseline",
		Short: "Show the app's upgrade baseline (framework and scaffold versions)",
		Long: `Show the application's upgrade baseline: the framework version it builds
against (read from go.mod) and the scaffold conventions it was generated
with (recorded in gombit.yaml by gombit new).

An app generated before upgrade metadata existed records none; its scaffold
version is detected as 0. --write records that in gombit.yaml (appending the
metadata block, and changing nothing else), so later upgrades start from an
explicit baseline. Nothing is written without --write.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := cmd.Flags().GetString("dir")
			if err != nil {
				return err
			}
			asJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return err
			}
			write, err := cmd.Flags().GetBool("write")
			if err != nil {
				return err
			}
			return runUpgradeBaseline(stdout, dir, asJSON, write)
		},
	})
	cmd.Flags().String("dir", ".", "application directory (go.mod and gombit.yaml)")
	cmd.Flags().Bool("json", false, "print the baseline as JSON")
	cmd.Flags().Bool("write", false, "record a detected baseline in gombit.yaml (apps without upgrade metadata)")
	return cmd
}

func runUpgradeBaseline(stdout io.Writer, dir string, asJSON, write bool) error {
	var (
		b     upgrade.Baseline
		wrote bool
		err   error
	)
	if write {
		b, wrote, err = upgrade.RecordBaseline(dir)
	} else {
		b, err = upgrade.Detect(dir)
	}
	if err != nil {
		return fmt.Errorf("gombit upgrade baseline: %w", err)
	}
	if asJSON {
		out := struct {
			upgrade.Baseline
			// Written is whether --write recorded the baseline (only with --write).
			Written *bool `json:"written,omitempty"`
		}{Baseline: b}
		if write {
			out.Written = &wrote
		}
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return fmt.Errorf("gombit upgrade baseline: %w", err)
		}
		_, err = fmt.Fprintf(stdout, "%s\n", data)
		return err
	}
	if err := printFramework(stdout, b.Framework); err != nil {
		return err
	}
	switch {
	case wrote:
		_, err = fmt.Fprintf(stdout, "Scaffold:  %d (recorded in %s now, metadata format %d)\n", b.Scaffold, upgrade.ProjectFile, b.Metadata)
	case b.Recorded:
		_, err = fmt.Fprintf(stdout, "Scaffold:  %d (recorded in %s, metadata format %d)\n", b.Scaffold, upgrade.ProjectFile, b.Metadata)
	default:
		_, err = fmt.Fprintf(stdout, "Scaffold:  0 (no upgrade metadata in %s: generated before it existed)\n"+
			"           Run gombit upgrade baseline --write to record it.\n", upgrade.ProjectFile)
	}
	return err
}

// printFramework writes the Framework line of the baseline.
func printFramework(stdout io.Writer, fw upgrade.Framework) error {
	var replaced string
	if r := fw.Replace; r != nil {
		replaced = r.Path
		if r.Version != "" {
			replaced += " " + r.Version
		}
	}
	var err error
	switch {
	case fw.Workspace != "":
		_, err = fmt.Fprintf(stdout, "Framework: %s, decided by the workspace %s (go.mod requires %s; with GOWORK=off the app builds from go.mod alone)\n", fw.Module, fw.Workspace, fw.Required)
	case fw.Local:
		_, err = fmt.Fprintf(stdout, "Framework: %s, replaced by the local directory %s (no published version)\n", fw.Module, replaced)
	case fw.Version == "":
		_, err = fmt.Fprintf(stdout, "Framework: %s, replaced by another module, %s (not a framework release)\n", fw.Module, replaced)
	case fw.Replace != nil:
		_, err = fmt.Fprintf(stdout, "Framework: %s %s (go.mod; replaced by %s)\n", fw.Module, fw.Version, replaced)
	default:
		_, err = fmt.Fprintf(stdout, "Framework: %s %s (go.mod)\n", fw.Module, fw.Version)
	}
	return err
}

func upgradeUsage(stderr io.Writer) {
	_, _ = fmt.Fprintln(stderr, "Usage: gombit upgrade <subcommand>")
	_, _ = fmt.Fprintln(stderr, "  baseline    Show (or record) the app's upgrade baseline")
}
