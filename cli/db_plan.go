package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/migrations"
	"github.com/gombit-dev/gombit/migrations/schemaplan"
	"github.com/spf13/cobra"
)

func newPlanCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "plan",
		Short: "Classify the schema change the models imply, without writing a migration",
		Long: `Compare the schema the migration directory builds with the schema the models
declare, and classify every change the next makemigrations would write.

  destructive  loses data: a dropped table or column, a narrowed type
  unsafe       can fail on a table that has rows: a new NOT NULL column with
               no default, nullable -> NOT NULL, a new unique index, foreign
               key, or check
  review       applies, but changes behavior: a delete rule, a widened type,
               a SQLite table rebuild
  safe         adds structure or relaxes a rule

A dropped column next to an added column of the same type is flagged as a
possible rename, with the --rename command that keeps the data.

The command exits non-zero while any destructive or unsafe change is not
acknowledged with --allow, so CI can gate on it. --allow takes a step ID
(drop_column:products.name) or a code (drop_column); makemigrations accepts
the same flag and refuses to write a migration without it.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("gombit db plan: unexpected argument %q", args[0])
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
			allow, err := cmd.Flags().GetStringArray("allow")
			if err != nil {
				return err
			}
			asJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return err
			}
			in, err := migrations.Inspect(cmd.Context(), migrations.InspectOptions{
				WorkDir:      ".",
				Driver:       config.DatabaseDriver(driver),
				MigrationDir: migrationDir,
				AtlasBinary:  atlasBin,
				Models:       models,
				ForgetModels: forgetModels,
				Stderr:       stderr,
			})
			if err != nil {
				return err
			}
			plan, err := schemaplan.Build(in)
			if err != nil {
				return err
			}
			for _, a := range plan.Acknowledge(allow) {
				_, _ = fmt.Fprintf(stderr, "warning: --allow %s matched no change in the plan\n", a)
			}
			return writePlanResult(stdout, plan, asJSON)
		},
	})
	cmd.Flags().String("driver", "", "database driver: sqlite, postgres, or mysql")
	cmd.Flags().String("dir", "database/migrations", "migration directory")
	cmd.Flags().String("atlas-bin", "atlas", "Atlas CLI binary path")
	cmd.Flags().StringArray("model", nil, "GORM model import path and type to add to the registered models, as for makemigrations; repeat for multiple models")
	cmd.Flags().StringArray("forget-model", nil, "GORM model import path and type to stop tracking, as for makemigrations (acknowledges the drop of its table under GORM's default name); repeat for multiple models")
	cmd.Flags().StringArray("allow", nil, "acknowledge a destructive or unsafe step by ID (drop_column:products.name) or code (drop_column); repeat for multiple")
	cmd.Flags().Bool("json", false, "print the plan as JSON")
	return cmd
}

// planJSON is the --json shape: the steps plus whether the plan still needs
// acknowledgement (the exit status).
type planJSON struct {
	Driver                config.DatabaseDriver `json:"driver"`
	Steps                 []schemaplan.PlanStep `json:"steps"`
	NeedsAcknowledgement  bool                  `json:"needs_acknowledgement"`
	UnacknowledgedStepIDs []string              `json:"unacknowledged"`
}

func writePlanResult(stdout io.Writer, plan schemaplan.SchemaPlan, asJSON bool) error {
	pending := plan.Unacknowledged()
	if asJSON {
		ids := make([]string, 0, len(pending))
		for _, s := range pending {
			ids = append(ids, s.ID)
		}
		data, err := json.MarshalIndent(planJSON{
			Driver:                plan.Driver,
			Steps:                 plan.Steps,
			NeedsAcknowledgement:  len(pending) > 0,
			UnacknowledgedStepIDs: ids,
		}, "", "  ")
		if err != nil {
			return fmt.Errorf("gombit db plan: marshal: %w", err)
		}
		if _, err := stdout.Write(append(data, '\n')); err != nil {
			return err
		}
	} else {
		schemaplan.WritePlan(stdout, plan)
	}
	if len(pending) > 0 {
		return fmt.Errorf("gombit db plan: %d destructive or unsafe change(s) need acknowledgement", len(pending))
	}
	return nil
}
