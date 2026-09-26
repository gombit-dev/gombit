package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/manifest"
	"github.com/gombit-dev/gombit/migrations"
	"github.com/gombit-dev/gombit/migrations/schemaplan"
	"github.com/spf13/cobra"
)

func newLintCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "lint",
		Short: "Check the migration directory: integrity, layout, and the safety of its newest migrations",
		Long: `Check the migration directory without touching the application database.

  integrity  atlas.sum matches every migration, and the migrations apply to an
             empty dev database (Atlas Community Edition 'migrate validate')
  layout     every *.sql file is an up migration; down files live in downs/
  safety     every migration is classified from the schema before and after
             it, like 'gombit db plan', and from its own statements (DELETE,
             UPDATE, TRUNCATE, a DROP TABLE the schema does not show, SQL
             Gombit cannot classify): a destructive or unsafe change passes
             only when the migration carries a line for it:

               -- gombit:allow drop_column:products.name

makemigrations writes those lines for every change --allow acknowledged, so a
generated migration lints clean; a hand-written one needs them added (then
'gombit db repair', since the edit changes atlas.sum). Renames the migration
states (ALTER TABLE ... RENAME TO / RENAME COLUMN) count as safe.

It checks every migration by default, and exits non-zero on any problem, so CI
can gate on it. --latest N classifies only the N newest migrations, a local
shortcut when the dev database is slow to start; don't use it in CI.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("gombit db lint: unexpected argument %q", args[0])
			}
			opts, err := dirFlags(cmd)
			if err != nil {
				return err
			}
			latest, err := cmd.Flags().GetInt("latest")
			if err != nil {
				return err
			}
			asJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return err
			}
			report, err := schemaplan.Lint(cmd.Context(), schemaplan.LintOptions{
				WorkDir:      opts.WorkDir,
				Driver:       opts.Driver,
				MigrationDir: opts.MigrationDir,
				AtlasBinary:  opts.AtlasBinary,
				Latest:       latest,
				Stderr:       stderr,
			})
			if err != nil {
				return err
			}
			if asJSON {
				data, err := json.MarshalIndent(report, "", "  ")
				if err != nil {
					return fmt.Errorf("gombit db lint: marshal: %w", err)
				}
				if _, err := stdout.Write(append(data, '\n')); err != nil {
					return err
				}
			} else {
				schemaplan.WriteLint(stdout, report)
			}
			if report.Failed() {
				return errors.New("gombit db lint: the migration directory has problems")
			}
			return nil
		},
	})
	bindDirFlags(cmd)
	cmd.Flags().Int("latest", 0, "classify only the N newest migrations (0, the default, classifies every one; use 0 in CI)")
	cmd.Flags().Bool("json", false, "print the report as JSON")
	return cmd
}

func newRepairCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "repair",
		Short: "Restore migration-directory consistency after an intentional hand edit",
		Long: `Restore the migration directory after an intentional edit to a migration
(a backfill, a --gombit:allow line), using only gombit:

  1. rehash atlas.sum ('gombit db hash')
  2. check that every migration still applies to an empty dev database
  3. check HOST-3 safety manifests (<version>_<name>.manifest.json) against
     their SQL; a manifest binds reviewed SQL, so a stale one is only
     rewritten with --write-manifests, after you review the change

Run 'gombit db lint' afterwards to classify the edited migration.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("gombit db repair: unexpected argument %q", args[0])
			}
			opts, err := dirFlags(cmd)
			if err != nil {
				return err
			}
			writeManifests, err := cmd.Flags().GetBool("write-manifests")
			if err != nil {
				return err
			}
			return runRepair(cmd, opts, writeManifests, stdout, stderr)
		},
	})
	bindDirFlags(cmd)
	cmd.Flags().Bool("write-manifests", false, "rewrite stale safety manifests from the current SQL (review the SQL first)")
	return cmd
}

func runRepair(cmd *cobra.Command, opts migrations.DirOptions, writeManifests bool, stdout, stderr io.Writer) error {
	// Atlas replays only a directory whose atlas.sum matches, so the rehash
	// comes first; if the replay then fails, the previous atlas.sum goes back,
	// so the broken SQL is not left checksummed.
	sumPath := filepath.Join(opts.MigrationDir, "atlas.sum")
	prevSum, sumErr := os.ReadFile(sumPath) // #nosec G304 -- atlas.sum in the configured migration directory
	if sumErr != nil && !errors.Is(sumErr, os.ErrNotExist) {
		return fmt.Errorf("gombit db repair: read atlas.sum: %w", sumErr)
	}
	restoreSum := func() {
		if sumErr == nil {
			_ = os.WriteFile(sumPath, prevSum, 0o600) // #nosec G703 -- restoring the file this command rewrote
		} else {
			_ = os.Remove(sumPath)
		}
	}
	if err := migrations.Hash(cmd.Context(), migrations.ApplyOptions{
		WorkDir:      opts.WorkDir,
		MigrationDir: opts.MigrationDir,
		AtlasBinary:  opts.AtlasBinary,
		Stdout:       stdout,
		Stderr:       stderr,
	}); err != nil {
		return err
	}
	if err := migrations.ValidateDir(cmd.Context(), opts); err != nil {
		restoreSum()
		return fmt.Errorf("%w (atlas.sum was left as it was; fix the migration, then run 'gombit db repair' again)", err)
	}
	_, _ = fmt.Fprintln(stdout, "Every migration applies to an empty database.")

	files, err := migrations.ListMigrationFiles(opts.MigrationDir)
	if err != nil {
		return err
	}
	var stale []string
	for _, f := range files {
		sql, err := os.ReadFile(f.UpPath) // #nosec G304 -- an up migration listed from the configured directory
		if err != nil {
			return err
		}
		mpath := manifestPath(f.UpPath)
		declared, ok, err := readManifest(mpath)
		if err != nil {
			return fmt.Errorf("gombit db repair: %w", err)
		}
		if !ok || manifest.Verify(declared, string(sql)) == nil {
			continue
		}
		if writeManifests {
			if err := writeManifest(mpath, manifest.Generate(manifest.Migration{Version: f.Version, Name: f.Name}, string(sql))); err != nil {
				return fmt.Errorf("gombit db repair: %w", err)
			}
			_, _ = fmt.Fprintf(stdout, "Rewrote the safety manifest for %s_%s.\n", f.Version, f.Name)
			continue
		}
		stale = append(stale, f.Version+"_"+f.Name)
	}
	if len(stale) > 0 {
		for _, s := range stale {
			_, _ = fmt.Fprintf(stderr, "stale safety manifest: %s no longer matches its SQL\n", s)
		}
		return fmt.Errorf("gombit db repair: %d safety manifest(s) no longer match their SQL; review the change, then run 'gombit db repair --write-manifests'", len(stale))
	}
	_, _ = fmt.Fprintln(stdout, "The migration directory is consistent. Run 'gombit db lint' to classify the edited migration.")
	return nil
}

func bindDirFlags(cmd *cobra.Command) {
	cmd.Flags().String("driver", "", "database driver: sqlite, postgres, or mysql (default: the configured one)")
	cmd.Flags().String("dir", "database/migrations", "migration directory")
	cmd.Flags().String("atlas-bin", "atlas", "Atlas CLI binary path")
}

func dirFlags(cmd *cobra.Command) (migrations.DirOptions, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return migrations.DirOptions{}, err
	}
	driver, err := cmd.Flags().GetString("driver")
	if err != nil {
		return migrations.DirOptions{}, err
	}
	if !cmd.Flags().Changed("driver") {
		driver = string(cfg.Database.Driver)
	}
	dir, err := cmd.Flags().GetString("dir")
	if err != nil {
		return migrations.DirOptions{}, err
	}
	atlasBin, err := cmd.Flags().GetString("atlas-bin")
	if err != nil {
		return migrations.DirOptions{}, err
	}
	return migrations.DirOptions{WorkDir: ".", Driver: config.DatabaseDriver(driver), MigrationDir: dir, AtlasBinary: atlasBin}, nil
}
