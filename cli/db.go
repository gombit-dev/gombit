package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/migrations"
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
	cmd.AddCommand(newMigrateCommand(stdout, stderr))
	cmd.AddCommand(newRollbackCommand(stdout, stderr))
	cmd.AddCommand(newStatusCommand(stdout, stderr))
	cmd.AddCommand(newVerifyCommand(stdout, stderr))
	cmd.AddCommand(newSeedCommand(stdout, stderr))
	cmd.AddCommand(newResetCommand(stdout, stderr))
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
			return migrations.MakeMigrations(cmd.Context(), migrations.Options{
				WorkDir:      ".",
				Name:         args[0],
				Driver:       config.DatabaseDriver(driver),
				MigrationDir: migrationDir,
				AtlasBinary:  atlasBin,
				Models:       models,
				ForgetModels: forgetModels,
				Stdout:       stdout,
				Stderr:       stderr,
			})
		},
	})
	cmd.Flags().String("driver", "", "database driver: sqlite, postgres, or mysql")
	cmd.Flags().String("dir", "database/migrations", "migration directory")
	cmd.Flags().String("atlas-bin", "atlas", "Atlas CLI binary path")
	cmd.Flags().StringArray("model", nil, "GORM model import path and type, e.g. github.com/acme/app/internal/product.Product; repeat for multiple models. Merged with models already registered from earlier makemigrations runs — you don't need to repeat them.")
	cmd.Flags().StringArray("forget-model", nil, "GORM model import path and type to stop tracking, proposing a DROP for its table; repeat for multiple models")
	return cmd
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
