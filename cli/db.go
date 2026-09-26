package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/migrations"
	"github.com/gombit-dev/gombit/migrations/schemaplan"
	"github.com/gombit-dev/gombit/resourcegen"
	"github.com/spf13/cobra"
)

func newDBCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "db",
		Short: "Database migration commands",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dbUsage(stderr)
			if len(args) == 0 {
				return fmt.Errorf("gombit db: subcommand is required")
			}
			return fmt.Errorf("gombit db: unknown subcommand %q", args[0])
		},
	})
	cmd.AddCommand(newMakeMigrationsCommand(stdout, stderr))
	cmd.AddCommand(newPlanCommand(stdout, stderr))
	cmd.AddCommand(newMigrateCommand(stdout, stderr))
	cmd.AddCommand(newRollbackCommand(stdout, stderr))
	cmd.AddCommand(newStatusCommand(stdout, stderr))
	cmd.AddCommand(newVerifyCommand(stdout, stderr))
	cmd.AddCommand(newSeedCommand(stdout, stderr))
	cmd.AddCommand(newResetCommand(stdout, stderr))
	cmd.AddCommand(newHashCommand(stdout, stderr))
	return cmd
}

func newHashCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "hash",
		Short: "Recompute the migration directory checksum (atlas.sum) after a manual edit",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("gombit db hash: unexpected argument %q", args[0])
			}
			migrationDir, err := cmd.Flags().GetString("dir")
			if err != nil {
				return err
			}
			atlasBin, err := cmd.Flags().GetString("atlas-bin")
			if err != nil {
				return err
			}
			return migrations.Hash(cmd.Context(), migrations.ApplyOptions{
				WorkDir:      ".",
				MigrationDir: migrationDir,
				AtlasBinary:  atlasBin,
				Stdout:       stdout,
				Stderr:       stderr,
			})
		},
	})
	bindApplyFlags(cmd)
	return cmd
}

func newMakeMigrationsCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "makemigrations [name]",
		Short: "Generate an Atlas SQL migration from GORM models",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("gombit db makemigrations: migration name is required")
			}
			if len(args) != 1 {
				return fmt.Errorf("gombit db makemigrations: unexpected argument %q", args[1])
			}
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			driver, err := cmd.Flags().GetString("driver")
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("driver") {
				driver = string(cfg.Database.Driver)
			}
			migrationDir, err := cmd.Flags().GetString("dir")
			if err != nil {
				return err
			}
			atlasBin, err := cmd.Flags().GetString("atlas-bin")
			if err != nil {
				return err
			}
			modelValues, err := cmd.Flags().GetStringArray("model")
			if err != nil {
				return err
			}
			models, err := parseModels(modelValues)
			if err != nil {
				return err
			}
			forgetValues, err := cmd.Flags().GetStringArray("forget-model")
			if err != nil {
				return err
			}
			forgetModels, err := parseModels(forgetValues)
			if err != nil {
				return err
			}
			// Fail closed on contradictory desired state: a --forget-model that is
			// still an AutoMigrate argument would have its DROP silently undone by the
			// next make resource / app start (AutoMigrate re-adds it). makemigrations
			// must never rewrite or counteract the app's declared desired state, so
			// refuse and tell the user to retire it there first (#300).
			if err := ensureForgetModelsRetired(".", forgetModels); err != nil {
				return err
			}
			renameValues, err := cmd.Flags().GetStringArray("rename")
			if err != nil {
				return err
			}
			renames, err := parseRenames(renameValues)
			if err != nil {
				return err
			}
			tableRenameValues, err := cmd.Flags().GetStringArray("rename-table")
			if err != nil {
				return err
			}
			tableRenames, err := parseTableRenames(tableRenameValues)
			if err != nil {
				return err
			}
			allow, err := cmd.Flags().GetStringArray("allow")
			if err != nil {
				return err
			}
			return migrations.MakeMigrations(cmd.Context(), migrations.Options{
				WorkDir:      ".",
				Name:         args[0],
				Driver:       config.DatabaseDriver(driver),
				MigrationDir: migrationDir,
				AtlasBinary:  atlasBin,
				Models:       models,
				ForgetModels: forgetModels,
				Renames:      renames,
				TableRenames: tableRenames,
				// Refuse a destructive or unsafe change nothing acknowledged (#309).
				Gate:   schemaplan.Gate(allow, stderr),
				Stdout: stdout,
				Stderr: stderr,
			})
		},
	})
	cmd.Flags().String("driver", "", "database driver: sqlite, postgres, or mysql")
	cmd.Flags().String("dir", "database/migrations", "migration directory")
	cmd.Flags().String("atlas-bin", "atlas", "Atlas CLI binary path")
	cmd.Flags().StringArray("model", nil, "GORM model import path and type, e.g. github.com/acme/app/internal/product.Product; repeat for multiple models. Merged with models already registered from earlier makemigrations runs — you don't need to repeat them.")
	cmd.Flags().StringArray("forget-model", nil, "GORM model import path and type to stop tracking, proposing a DROP for its table; repeat for multiple models")
	cmd.Flags().StringArray("rename", nil, "rename a column data-preservingly as table.old_column:new_column (or table.old_column=table.new_column; native RENAME COLUMN, not a drop+add rebuild); repeat for multiple columns. Alone it cannot be combined with --model/--forget-model.")
	cmd.Flags().StringArray("rename-table", nil, "rename a table data-preservingly as old_table:new_table (native ALTER TABLE ... RENAME TO); repeat for multiple tables. Applies before --rename, which then names the new table. Pair it with --forget-model <old model> --model <new model> to swap a renamed model in the registry.")
	cmd.Flags().StringArray("allow", nil, "acknowledge a destructive or unsafe change from 'gombit db plan' by ID (drop_column:products.name) or code (drop_column); repeat for multiple. Without it, a migration containing one is not written.")
	return cmd
}

func parseTableRenames(values []string) ([]migrations.TableRename, error) {
	renames := make([]migrations.TableRename, 0, len(values))
	for _, value := range values {
		rename, err := migrations.ParseTableRename(value)
		if err != nil {
			return nil, err
		}
		renames = append(renames, rename)
	}
	return renames, nil
}

func parseRenames(values []string) ([]migrations.Rename, error) {
	renames := make([]migrations.Rename, 0, len(values))
	for _, value := range values {
		rename, err := migrations.ParseRename(value)
		if err != nil {
			return nil, err
		}
		renames = append(renames, rename)
	}
	return renames, nil
}

// ensureForgetModelsRetired refuses to forget a model that is still registered in
// the app's AutoMigrate call. AutoMigrate (internal/platform/database.go) and the
// migration registry are two declarations of desired state; forgetting a model
// the app still auto-migrates leaves them contradictory, and the next make
// resource / app start re-adds it — the DROP never sticks. Apps without that file
// (a non-scaffolded layout) skip the check (#300).
func ensureForgetModelsRetired(workDir string, forget []migrations.Model) error {
	if len(forget) == 0 {
		return nil
	}
	registered, ok, err := resourcegen.AutoMigrateModels(workDir)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	have := make(map[migrations.Model]bool, len(registered))
	for _, model := range registered {
		have[model] = true
	}
	for _, model := range forget {
		if have[model] {
			return fmt.Errorf(
				"gombit db makemigrations: cannot forget %s.%s: it is still registered in AutoMigrate (%s).\n\n"+
					"Remove it from AutoMigrate (and any routes/resources referencing it), then rerun:\n\n"+
					"    gombit db makemigrations <name> --forget-model %s.%s",
				model.ImportPath, model.TypeName, resourcegen.PlatformDBRel(), model.ImportPath, model.TypeName)
		}
	}
	return nil
}

func parseModels(values []string) ([]migrations.Model, error) {
	models := make([]migrations.Model, 0, len(values))
	for _, value := range values {
		model, err := migrations.ParseModel(value)
		if err != nil {
			return nil, err
		}
		models = append(models, model)
	}
	return models, nil
}

func newMigrateCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	return newApplyCommand("migrate", "Apply pending Atlas migrations", stdout, stderr, migrations.Migrate)
}

func newRollbackCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	return newApplyCommand("rollback", "Roll back the latest migration batch", stdout, stderr, migrations.Rollback)
}

func newStatusCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	return newApplyCommand("status", "Show applied and pending migrations", stdout, stderr, migrations.Status)
}

func newApplyCommand(
	name string,
	short string,
	stdout io.Writer,
	stderr io.Writer,
	fn func(context.Context, migrations.ApplyOptions) error,
) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   name,
		Short: short,
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("gombit db %s: unexpected argument %q", name, args[0])
			}
			opts, err := applyOptionsFromCmd(cmd)
			if err != nil {
				return err
			}
			opts.Stdout = stdout
			opts.Stderr = stderr
			return fn(cmd.Context(), opts)
		},
	})
	bindApplyFlags(cmd)
	return cmd
}

func newSeedCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "seed",
		Short: "Apply SQL seed files",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("gombit db seed: unexpected argument %q", args[0])
			}
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			seedDir, err := cmd.Flags().GetString("seeds")
			if err != nil {
				return err
			}
			return migrations.Seed(cmd.Context(), migrations.SeedOptions{
				WorkDir:  ".",
				SeedDir:  seedDir,
				Database: cfg.Database,
				Stdout:   stdout,
				Stderr:   stderr,
			})
		},
	})
	cmd.Flags().String("seeds", "database/seeds", "seed directory")
	return cmd
}

func newResetCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "reset",
		Short: "Drop schema, re-apply migrations, and seed",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("gombit db reset: unexpected argument %q", args[0])
			}
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			migrationDir, err := cmd.Flags().GetString("dir")
			if err != nil {
				return err
			}
			seedDir, err := cmd.Flags().GetString("seeds")
			if err != nil {
				return err
			}
			atlasBin, err := cmd.Flags().GetString("atlas-bin")
			if err != nil {
				return err
			}
			force, err := cmd.Flags().GetBool("force")
			if err != nil {
				return err
			}
			return migrations.Reset(cmd.Context(), migrations.ResetOptions{
				ApplyOptions: migrations.ApplyOptions{
					WorkDir:      ".",
					MigrationDir: migrationDir,
					AtlasBinary:  atlasBin,
					Database:     cfg.Database,
					Stdout:       stdout,
					Stderr:       stderr,
				},
				SeedDir:     seedDir,
				Force:       force,
				Environment: cfg.Environment,
			})
		},
	})
	bindApplyFlags(cmd)
	cmd.Flags().String("seeds", "database/seeds", "seed directory")
	cmd.Flags().Bool("force", false, "allow reset when GOMBIT_ENV=production")
	return cmd
}

func bindApplyFlags(cmd *cobra.Command) {
	cmd.Flags().String("dir", "database/migrations", "migration directory")
	cmd.Flags().String("atlas-bin", "atlas", "Atlas CLI binary path")
}

func applyOptionsFromCmd(cmd *cobra.Command) (migrations.ApplyOptions, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return migrations.ApplyOptions{}, err
	}
	migrationDir, err := cmd.Flags().GetString("dir")
	if err != nil {
		return migrations.ApplyOptions{}, err
	}
	atlasBin, err := cmd.Flags().GetString("atlas-bin")
	if err != nil {
		return migrations.ApplyOptions{}, err
	}
	return migrations.ApplyOptions{
		WorkDir:      ".",
		MigrationDir: migrationDir,
		AtlasBinary:  atlasBin,
		Database:     cfg.Database,
	}, nil
}
