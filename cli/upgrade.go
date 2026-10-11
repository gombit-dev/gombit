package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/spf13/cobra"

	"github.com/gombit-dev/gombit/upgrade"
)

func newUpgradeCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade the framework dependency (--to), plan it (--dry-run), and upgrade tooling",
		Long: `Plan upgrading the application to a framework release, from the
compatibility manifest built into this gombit, without changing anything:

  gombit upgrade --dry-run [--to vX.Y.Z] [--dir .] [--json]

The plan reports the current and target versions, the dependency change,
the automatic changes the app needs, every manual action with what to do,
and the breaking changes. --to selects the target (default: the newest
release this gombit knows). A version the manifest does not list, or an app
whose baseline names no framework release (a go.work, a local checkout, a
fork), fails with the reason. It never prompts, so it runs in CI as is.

Apply the plan's dependency changes:

  gombit upgrade --to vX.Y.Z [--dir .] [--json]

It edits the replaces of the framework the plan names (go mod edit), then
runs go get github.com/gombit-dev/gombit@vX.Y.Z, prints every go command and
the go.mod and go.sum lines they changed, and leaves them for review. A go
command that fails is shown with its full output, and go.mod and go.sum are
restored. The plan's automatic and manual changes are printed as still to
do: they are not applied. --to is required here: nothing is applied to a
default target.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				upgradeUsage(stderr)
				return fmt.Errorf("gombit upgrade: unknown subcommand %q", args[0])
			}
			flags := cmd.Flags()
			dryRun, _ := flags.GetBool("dry-run")
			to, _ := flags.GetString("to")
			dir, _ := flags.GetString("dir")
			asJSON, _ := flags.GetBool("json")
			switch {
			case dryRun:
				return runUpgradePlan(stdout, dir, to, asJSON)
			case to != "":
				return runUpgradeApply(cmd.Context(), stdout, dir, to, asJSON)
			default:
				upgradeUsage(stderr)
				return errors.New("gombit upgrade: --to (apply) or --dry-run (plan) or a subcommand is required")
			}
		},
	})
	cmd.Flags().Bool("dry-run", false, "plan the upgrade and print it; change nothing")
	cmd.Flags().String("to", "", "target framework release: applied without --dry-run; with it, planned (default: the newest release this gombit knows)")
	cmd.Flags().String("dir", ".", "application directory (go.mod and gombit.yaml)")
	cmd.Flags().Bool("json", false, "print the plan as JSON")
	cmd.AddCommand(newUpgradeBaselineCommand(stdout))
	cmd.AddCommand(newUpgradeCheckCommand(stdout))
	cmd.AddCommand(newUpgradeNotesCommand(stdout))
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
	if err != nil && !wrote {
		return fmt.Errorf("gombit upgrade baseline: %w", err)
	}
	// An error after gombit.yaml changed (not synced to disk): report what is
	// now in the file, then fail with it.
	if err := printBaseline(stdout, b, asJSON, write, wrote); err != nil {
		return err
	}
	if err != nil {
		return fmt.Errorf("gombit upgrade baseline: %w", err)
	}
	return nil
}

// printBaseline writes the baseline, as text or JSON.
func printBaseline(stdout io.Writer, b upgrade.Baseline, asJSON, write, wrote bool) error {
	var err error
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

func newUpgradeNotesCommand(stdout io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "notes",
		Short: "Print upgrade notes from the compatibility manifest",
		Long: `Print the upgrade notes of framework releases, from the compatibility
manifest built into this gombit: each release's upgrade-relevant changes,
classified as manual, automatic, or informational, breaking ones marked.
The release notes and docs/upgrade-notes.md are rendered from the same
manifest.

With no flags, every release the manifest covers, newest first. --release
prints one release (or "unreleased"). --from (and --to, by default the
newest release this gombit knows) prints the releases an upgrade moves
across; a version the manifest does not list fails, rather than guessing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			flags := cmd.Flags()
			release, _ := flags.GetString("release")
			from, _ := flags.GetString("from")
			to, _ := flags.GetString("to")
			asJSON, _ := flags.GetBool("json")
			gate, _ := flags.GetBool("release-gate")
			if gate {
				if release == "" {
					return errors.New("gombit upgrade notes: --release-gate needs --release")
				}
				m, err := upgrade.LoadManifest()
				if err != nil {
					return fmt.Errorf("gombit upgrade notes: %w", err)
				}
				if err := m.CheckRelease(release); err != nil {
					return fmt.Errorf("gombit upgrade notes: %w", err)
				}
			}
			return runUpgradeNotes(stdout, release, from, to, asJSON)
		},
	})
	cmd.Flags().String("release", "", `one release's notes (a version, or "unreleased")`)
	cmd.Flags().String("from", "", "the notes of an upgrade from this version")
	cmd.Flags().String("to", "", "with --from: the target version (default: the newest release this gombit knows)")
	cmd.Flags().Bool("json", false, "print the releases and their classified changes as JSON")
	// The release workflow's gate for a stable tag (docs/releasing.md): the
	// manifest must be closed at --release (Manifest.CheckRelease).
	cmd.Flags().Bool("release-gate", false, "fail unless the manifest is ready to ship as the --release stable tag")
	_ = cmd.Flags().MarkHidden("release-gate")
	return cmd
}

func runUpgradeNotes(stdout io.Writer, release, from, to string, asJSON bool) error {
	m, err := upgrade.LoadManifest()
	if err != nil {
		return fmt.Errorf("gombit upgrade notes: %w", err)
	}
	var releases []upgrade.Release
	switch {
	case release != "" && (from != "" || to != ""):
		return errors.New("gombit upgrade notes: --release cannot be combined with --from/--to")
	case to != "" && from == "":
		return errors.New("gombit upgrade notes: --to needs --from")
	case release != "":
		r, err := m.Release(release)
		if err != nil {
			return fmt.Errorf("gombit upgrade notes: %w", err)
		}
		releases = []upgrade.Release{r}
	case from != "":
		if to == "" {
			to = m.Latest()
		}
		if releases, err = m.Path(from, to); err != nil {
			return fmt.Errorf("gombit upgrade notes: %w", err)
		}
	default:
		releases = slices.Clone(m.Releases)
		slices.Reverse(releases)
	}
	if asJSON {
		data, err := json.MarshalIndent(struct {
			Releases       []upgrade.Release      `json:"releases"`
			Classification upgrade.Classification `json:"classification"`
		}{releases, upgrade.Classify(releases)}, "", "  ")
		if err != nil {
			return fmt.Errorf("gombit upgrade notes: %w", err)
		}
		_, err = fmt.Fprintf(stdout, "%s\n", data)
		return err
	}
	if len(releases) == 0 {
		_, err = fmt.Fprintf(stdout, "%s is already %s: no releases to move across.\n", from, to)
		return err
	}
	return m.RenderNotes(stdout, releases)
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

// runUpgradePlan prints the plan for upgrading the app in dir to to.
func runUpgradePlan(stdout io.Writer, dir, to string, asJSON bool) error {
	m, err := upgrade.LoadManifest()
	if err != nil {
		return fmt.Errorf("gombit upgrade: %w", err)
	}
	plan, err := upgrade.PlanUpgrade(dir, m, to)
	if err != nil {
		return fmt.Errorf("gombit upgrade: %w", err)
	}
	if asJSON {
		data, err := json.MarshalIndent(plan, "", "  ")
		if err != nil {
			return fmt.Errorf("gombit upgrade: %w", err)
		}
		_, err = fmt.Fprintf(stdout, "%s\n", data)
		return err
	}
	return plan.Render(stdout)
}

// runUpgradeApply applies the dependency changes of upgrading the app in
// dir to to, and prints what it did and what is still to do.
func runUpgradeApply(ctx context.Context, stdout io.Writer, dir, to string, asJSON bool) error {
	m, err := upgrade.LoadManifest()
	if err != nil {
		return fmt.Errorf("gombit upgrade: %w", err)
	}
	applied, err := upgrade.ApplyDependencies(ctx, dir, m, to)
	if err != nil {
		return fmt.Errorf("gombit upgrade: %w", err)
	}
	if asJSON {
		data, err := json.MarshalIndent(applied, "", "  ")
		if err != nil {
			return fmt.Errorf("gombit upgrade: %w", err)
		}
		_, err = fmt.Fprintf(stdout, "%s\n", data)
		return err
	}
	return applied.Render(stdout)
}

func newUpgradeCheckCommand(stdout io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "check",
		Short: "Report uses of deprecated framework APIs",
		Long: `Report every use, in the application, of a framework API marked deprecated
(a "Deprecated:" paragraph in its doc comment, the Go convention): file,
line and column, the symbol, and what to use instead. Uses are found from
type information (go/packages, go/types), not by matching text, in every
package of the app, tests included. The app must type-check.

It exits 1 when it finds any, so a CI job can gate on it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, _ := cmd.Flags().GetString("dir")
			asJSON, _ := cmd.Flags().GetBool("json")
			return runUpgradeCheck(cmd.Context(), stdout, dir, asJSON)
		},
	})
	cmd.Flags().String("dir", ".", "application directory")
	cmd.Flags().Bool("json", false, "print the uses as JSON")
	return cmd
}

// runUpgradeCheck reports the app's uses of deprecated framework APIs, and
// fails when there are any.
func runUpgradeCheck(ctx context.Context, stdout io.Writer, dir string, asJSON bool) error {
	uses, err := upgrade.ScanDeprecated(ctx, dir)
	if err != nil {
		return fmt.Errorf("gombit upgrade check: %w", err)
	}
	if asJSON {
		if uses == nil {
			uses = []upgrade.DeprecatedUse{}
		}
		data, err := json.MarshalIndent(uses, "", "  ")
		if err != nil {
			return fmt.Errorf("gombit upgrade check: %w", err)
		}
		if _, err := fmt.Fprintf(stdout, "%s\n", data); err != nil {
			return err
		}
	} else if err := upgrade.RenderDeprecated(stdout, uses); err != nil {
		return err
	}
	if len(uses) > 0 {
		return fmt.Errorf("gombit upgrade check: %d use(s) of deprecated framework APIs", len(uses))
	}
	return nil
}

func upgradeUsage(stderr io.Writer) {
	_, _ = fmt.Fprintln(stderr, "Usage: gombit upgrade --to vX.Y.Z [--dir .] [--json]")
	_, _ = fmt.Fprintln(stderr, "       gombit upgrade --dry-run [--to vX.Y.Z] [--dir .] [--json]")
	_, _ = fmt.Fprintln(stderr, "       gombit upgrade <subcommand>")
	_, _ = fmt.Fprintln(stderr, "  baseline    Show (or record) the app's upgrade baseline")
	_, _ = fmt.Fprintln(stderr, "  check       Report uses of deprecated framework APIs")
	_, _ = fmt.Fprintln(stderr, "  notes       Print upgrade notes from the compatibility manifest")
}
