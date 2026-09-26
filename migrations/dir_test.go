package migrations

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/config"
)

func TestHintWriterRewritesRawAtlasAdvice(t *testing.T) {
	var out bytes.Buffer
	w := newHintWriter(&out)
	_, _ = io.WriteString(w, "You have a checksum error in your migration directory.\nPlease check your migration files and run 'atlas migrate ")
	_, _ = io.WriteString(w, "hash' to re-hash the contents\nError: checksum mismatch")
	w.Flush()
	got := out.String()
	if strings.Contains(got, "atlas migrate") || !strings.Contains(got, "run 'gombit db hash' to re-hash") || !strings.HasSuffix(got, "Error: checksum mismatch") {
		t.Fatalf("hintWriter output = %q", got)
	}
}

func TestAtlasMessageDropsTheNotice(t *testing.T) {
	raw := "Notice: This Atlas edition lacks support for features such as checkpoints,\ntesting, down migrations, and more.\n\nTo install the non-community version of Atlas, use the following command:\n\n\tcurl -sSf https://atlasgo.sh | sh\n\nOr, visit the website to see all installation options:\n\n\thttps://atlasgo.io/docs#installation\n\nError: sql/migrate: execute: near \"TABL\": syntax error\n"
	if got := atlasMessage(raw); got != `Error: sql/migrate: execute: near "TABL": syntax error` {
		t.Fatalf("atlasMessage() = %q", got)
	}
}

func TestAllowDirectives(t *testing.T) {
	sql := "-- gombit:allow drop_column:products.name\n  -- gombit:allow add_not_null\n-- a comment\nALTER TABLE x DROP COLUMN name;\n"
	got := AllowDirectives(sql)
	if len(got) != 2 || got[0] != "drop_column:products.name" || got[1] != "add_not_null" {
		t.Fatalf("AllowDirectives() = %v", got)
	}
	out := withAllowDirectives(sql, []string{"drop_table:legacy", "drop_column:products.name"})
	if strings.Count(out, "drop_column:products.name") != 1 || !strings.HasPrefix(out, "-- gombit:allow drop_table:legacy\n") {
		t.Fatalf("withAllowDirectives() = %q, want the new id prepended once", out)
	}
}

// scriptedRunner answers one Atlas command with fixed output and exit.
type scriptedRunner struct {
	out  string
	err  error
	args []string
}

func (r *scriptedRunner) Run(_ context.Context, _ string, _ string, args []string, stdout io.Writer, stderr io.Writer) error {
	r.args = append([]string(nil), args...)
	_, _ = io.WriteString(stderr, r.out)
	return r.err
}

func TestValidateDirMapsAtlasErrors(t *testing.T) {
	dir := t.TempDir()
	checksum := &scriptedRunner{out: "You have a checksum error in your migration directory.\n\tL2: 20260101000000_init.sql was edited\n\tL3: 20260102000000_next.sql was added\nPlease check your migration files and run 'atlas migrate hash' to re-hash the contents\nError: checksum mismatch\n", err: errors.New("exit status 1")}
	err := ValidateDir(context.Background(), DirOptions{WorkDir: dir, Driver: config.DatabaseDriverSQLite, MigrationDir: dir, runner: checksum})
	var cerr *ChecksumError
	if !errors.As(err, &cerr) || len(cerr.Files) != 2 || !strings.Contains(err.Error(), "gombit db repair") {
		t.Fatalf("ValidateDir() = %v, want a ChecksumError naming both files", err)
	}
	if checksum.args[0] != "migrate" || checksum.args[1] != "validate" {
		t.Fatalf("atlas args = %v, want migrate validate", checksum.args)
	}
	replay := &scriptedRunner{out: "Error: sql/migrate: executing statement \"CREATE TABL x\" from version \"20260101000000\": near \"TABL\": syntax error\n", err: errors.New("exit status 1")}
	err = ValidateDir(context.Background(), DirOptions{WorkDir: dir, Driver: config.DatabaseDriverSQLite, MigrationDir: dir, runner: replay})
	var rerr *ReplayError
	if !errors.As(err, &rerr) || !strings.Contains(rerr.Detail, "20260101000000") {
		t.Fatalf("ValidateDir() = %v, want a ReplayError naming the version", err)
	}
	// A replayed statement that mentions "checksum" is still a replay error.
	named := &scriptedRunner{out: "Error: sql/migrate: executing statement \"ALTER TABLE files ADD COLUMN checksum text NOT NULL\" from version \"20260103000000\": Cannot add a NOT NULL column with default value NULL\n", err: errors.New("exit status 1")}
	err = ValidateDir(context.Background(), DirOptions{WorkDir: dir, Driver: config.DatabaseDriverSQLite, MigrationDir: dir, runner: named})
	if !errors.As(err, &rerr) {
		t.Fatalf("ValidateDir() = %v, want a ReplayError, not a checksum error", err)
	}
}

func TestInspectDirAtPinsTheVersion(t *testing.T) {
	r := &scriptedRunner{}
	if _, err := InspectDirAt(context.Background(), DirOptions{WorkDir: t.TempDir(), Driver: config.DatabaseDriverSQLite, MigrationDir: "/m", runner: r}, "20260101000000"); err != nil {
		t.Fatal(err)
	}
	if !slicesContain(r.args, "file:///m?version=20260101000000") {
		t.Fatalf("atlas args = %v, want the version-pinned directory URL", r.args)
	}
	if hcl, err := InspectDirAt(context.Background(), DirOptions{Driver: config.DatabaseDriverSQLite, runner: r}, ""); err != nil || hcl != nil {
		t.Fatalf("InspectDirAt(\"\") = %q, %v; want the empty state without running Atlas", hcl, err)
	}
}

func slicesContain(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// directiveRunner fakes the loader, inspect, and diff like planRunner, and
// fails `migrate hash` when failHash is set.
type directiveRunner struct {
	planRunner
	failHash bool
	hashed   bool
}

func (r *directiveRunner) Run(ctx context.Context, dir string, name string, args []string, stdout io.Writer, stderr io.Writer) error {
	if len(args) >= 2 && args[0] == "migrate" && args[1] == "hash" {
		r.hashed = true
		if r.failHash {
			return errors.New("exit status 1")
		}
		return nil
	}
	return r.planRunner.Run(ctx, dir, name, args, stdout, stderr)
}

func TestMakeMigrationsRecordsAcknowledgements(t *testing.T) {
	for _, failHash := range []bool{false, true} {
		migrationDir := t.TempDir()
		seedMigrationDir(t, migrationDir)
		runner := &directiveRunner{planRunner: planRunner{t: t}, failHash: failHash}
		err := MakeMigrations(context.Background(), Options{
			WorkDir:      t.TempDir(),
			Name:         "drop_legacy",
			Driver:       config.DatabaseDriverSQLite,
			MigrationDir: migrationDir,
			AtlasBinary:  "atlas-test",
			Gate: func(context.Context, string, Inspection) ([]string, error) {
				return []string{"drop_column:products.legacy"}, nil
			},
			Stdout: io.Discard,
			Stderr: io.Discard,
			runner: runner,
		})
		written := filepath.Join(migrationDir, "20260102000000_drop_legacy.sql")
		data, readErr := os.ReadFile(written) // #nosec G304 -- test file
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !runner.hashed {
			t.Fatal("the directory was not rehashed after recording the acknowledgement")
		}
		if failHash {
			if err == nil || strings.Contains(string(data), "gombit:allow") {
				t.Fatalf("failed hash: err = %v, file = %q; want an error and the file restored", err, data)
			}
			continue
		}
		if err != nil {
			t.Fatalf("MakeMigrations() error = %v", err)
		}
		if got := AllowDirectives(string(data)); len(got) != 1 || got[0] != "drop_column:products.legacy" {
			t.Fatalf("directives = %v in %q", got, data)
		}
	}
}
