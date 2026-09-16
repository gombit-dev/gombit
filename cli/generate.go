package cli

import (
	"fmt"
	"io"

	"github.com/gombit-dev/gombit/generate"
	"github.com/spf13/cobra"
)

// newGenerateCommand builds `gombit generate`: regenerate the model-first
// resource files (*.gen.go) from the application's real GORM models, or verify
// they are current with --check (a CI drift gate).
func newGenerateCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	var (
		check  bool
		dryRun bool
	)
	cmd := silence(&cobra.Command{
		Use:   "generate",
		Short: "Regenerate model-first resource files (*.gen.go) from the app's models",
		Long: `Regenerate the generator-owned resource files (the *.gen.go DTOs, mappers,
and CRUD handler) from the application's real GORM models plus their declared
field policy. Run it after changing a model or its policy — regeneration is how
the DTOs, mappers, and handler stay in sync with the model (ADR-016).

It reads the actual compiled models via a throwaway program run inside the app
module (like gombit db makemigrations), so run it from the application root.

The human-owned hooks file is seeded once when absent and never overwritten.

  --check   render and compare against the committed *.gen.go without writing;
            exit non-zero if any differ (a CI drift gate). Never mutates the tree.
  --dry-run print what a write run would do without writing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			err := generate.Generate(cmd.Context(), generate.Options{
				WorkDir: ".",
				Check:   check,
				DryRun:  dryRun,
				Stdout:  stdout,
				Stderr:  stderr,
			})
			if err != nil {
				return fmt.Errorf("gombit generate: %w", err)
			}
			return nil
		},
	})
	cmd.Flags().BoolVar(&check, "check", false, "verify the committed *.gen.go are current; do not write")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be written without writing")
	return cmd
}
