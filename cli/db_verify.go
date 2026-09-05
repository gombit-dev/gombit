package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gombit-dev/gombit/manifest"
	"github.com/gombit-dev/gombit/migrations"
	"github.com/spf13/cobra"
)

func newVerifyCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "verify",
		Short: "Classify and verify migration safety manifests (HOST-3)",
		Long: `Classify each migration's SQL and verify its safety manifest (ADR-015 / HOST-3).

Reports which migrations lose data (drop column/table, destructive alter,
delete/truncate) and therefore require confirmation before a host applies them.

  --write  (re)generate a <version>_<name>.manifest.json beside each migration
  --json   print the classification of every migration as JSON (for a host)

Without --write, an existing manifest is verified against its SQL: a hash
mismatch (SQL changed after review) or a mis-declared safety fails. --strict
additionally exits non-zero when any migration requires confirmation, so a host
or CI can gate on the exit code. Gombit classifies and verifies; the approval
gate itself is the host's policy.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("gombit db verify: unexpected argument %q", args[0])
			}
			opts := verifyOptions{}
			var err error
			if opts.dir, err = cmd.Flags().GetString("dir"); err != nil {
				return err
			}
			if opts.write, err = cmd.Flags().GetBool("write"); err != nil {
				return err
			}
			if opts.asJSON, err = cmd.Flags().GetBool("json"); err != nil {
				return err
			}
			if opts.strict, err = cmd.Flags().GetBool("strict"); err != nil {
				return err
			}
			return runVerify(stdout, stderr, opts)
		},
	})
	cmd.Flags().String("dir", "database/migrations", "migration directory")
	cmd.Flags().Bool("write", false, "generate/overwrite a manifest beside each migration")
	cmd.Flags().Bool("json", false, "print each migration's classification as JSON")
	cmd.Flags().Bool("strict", false, "exit non-zero if any migration requires confirmation (for host/CI gating)")
	return cmd
}

type verifyOptions struct {
	dir    string
	write  bool
	asJSON bool
	strict bool
}

func runVerify(stdout io.Writer, stderr io.Writer, opts verifyOptions) error {
	dir := opts.dir
	write := opts.write
	asJSON := opts.asJSON
	files, err := migrations.ListMigrationFiles(dir)
	if err != nil {
		return fmt.Errorf("gombit db verify: %w", err)
	}

	results := make([]manifest.SafetyManifest, 0, len(files))
	var failures []string

	for _, f := range files {
		sql, err := os.ReadFile(f.UpPath) // #nosec G304 -- migration path from the enumerated migrations dir
		if err != nil {
			return fmt.Errorf("gombit db verify: read %s: %w", f.UpPath, err)
		}
		gen := manifest.Generate(manifest.Migration{Version: f.Version, Name: f.Name}, string(sql))
		results = append(results, gen)

		mpath := manifestPath(f.UpPath)
		if write {
			if err := writeManifest(mpath, gen); err != nil {
				return fmt.Errorf("gombit db verify: %w", err)
			}
			_, _ = fmt.Fprintf(stdout, "wrote %s\n", mpath)
			continue
		}
		if declared, ok, err := readManifest(mpath); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", f.Name, err))
		} else if ok {
			if err := manifest.Verify(declared, string(sql)); err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", f.Name, err))
			}
		}
	}

	if write {
		return nil
	}

	if asJSON {
		data, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return fmt.Errorf("gombit db verify: marshal: %w", err)
		}
		if _, err := stdout.Write(append(data, '\n')); err != nil {
			return err
		}
	} else {
		writeVerifyTable(stdout, results)
	}

	if len(failures) > 0 {
		for _, f := range failures {
			_, _ = fmt.Fprintf(stderr, "verification failed: %s\n", f)
		}
		return fmt.Errorf("gombit db verify: %d migration(s) failed verification", len(failures))
	}

	if opts.strict {
		var needConfirm []string
		for _, m := range results {
			if m.RequiresConfirmation {
				needConfirm = append(needConfirm, m.Migration.Version+"_"+m.Migration.Name)
			}
		}
		if len(needConfirm) > 0 {
			for _, n := range needConfirm {
				_, _ = fmt.Fprintf(stderr, "requires confirmation (data loss): %s\n", n)
			}
			return fmt.Errorf("gombit db verify: %d migration(s) require confirmation", len(needConfirm))
		}
	}
	return nil
}

func writeVerifyTable(stdout io.Writer, results []manifest.SafetyManifest) {
	if len(results) == 0 {
		_, _ = fmt.Fprintln(stdout, "no migrations found")
		return
	}
	dataLoss := 0
	for _, m := range results {
		status := "safe"
		if m.RequiresConfirmation {
			status = "REQUIRES CONFIRMATION (data loss)"
			dataLoss++
		}
		_, _ = fmt.Fprintf(stdout, "%s_%s: %s\n", m.Migration.Version, m.Migration.Name, status)
	}
	_, _ = fmt.Fprintf(stdout, "\n%d migration(s), %d require confirmation\n", len(results), dataLoss)
}

// manifestPath is the manifest file beside a migration's up SQL:
// 0001_name.sql -> 0001_name.manifest.json.
func manifestPath(upPath string) string {
	return strings.TrimSuffix(upPath, ".sql") + ".manifest.json"
}

func writeManifest(path string, m manifest.SafetyManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	// #nosec G703 G304 -- path is derived from the enumerated migrations dir.
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// readManifest reads and decodes a manifest. ok is false when the file does not
// exist (no manifest yet, not an error).
func readManifest(path string) (m manifest.SafetyManifest, ok bool, err error) {
	data, err := os.ReadFile(path) // #nosec G304 -- manifest path derived from the enumerated migrations dir
	if errors.Is(err, os.ErrNotExist) {
		return manifest.SafetyManifest{}, false, nil
	}
	if err != nil {
		return manifest.SafetyManifest{}, false, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return manifest.SafetyManifest{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return m, true, nil
}
