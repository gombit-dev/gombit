package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLintAndRepairHelp(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want []string
	}{
		{"lint", []string{"integrity", "-- gombit:allow", "--latest", "every migration", "--json", "gombit db repair", "DELETE"}},
		{"repair", []string{"rehash atlas.sum", "--write-manifests", "gombit db lint"}},
	} {
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		if err := ExecuteRoot(context.Background(), NewRoot(stdout, stderr), []string{"db", tc.cmd, "--help"}); err != nil {
			t.Fatalf("gombit db %s --help: %v", tc.cmd, err)
		}
		out := stdout.String() + stderr.String()
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Errorf("db %s help missing %q:\n%s", tc.cmd, want, out)
			}
		}
	}
}

// TestRepairAtlasCLISQLiteWhenAvailable edits a migration by hand and repairs
// the directory with gombit alone, including a stale safety manifest.
func TestRepairAtlasCLISQLiteWhenAvailable(t *testing.T) {
	atlasBin := os.Getenv("ATLAS_BINARY")
	if atlasBin == "" {
		var err error
		if atlasBin, err = exec.LookPath("atlas"); err != nil {
			t.Skip("Atlas CLI not found; set ATLAS_BINARY to run the real repair test")
		}
	}
	app := t.TempDir()
	t.Chdir(app)
	dir := filepath.Join("database", "migrations")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	up := filepath.Join(dir, "20260101000000_create_widgets.sql")
	if err := os.WriteFile(up, []byte("CREATE TABLE widgets (id integer PRIMARY KEY, name text NOT NULL);\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		flags := []string{"--atlas-bin", atlasBin}
		if args[1] != "hash" {
			flags = append(flags, "--driver", "sqlite")
		}
		err := ExecuteRoot(context.Background(), NewRoot(stdout, stderr), append(args, flags...))
		return stdout.String() + stderr.String(), err
	}
	if _, err := run("db", "hash"); err != nil {
		t.Fatalf("db hash: %v", err)
	}
	if err := runVerify(new(bytes.Buffer), new(bytes.Buffer), verifyOptions{dir: dir, write: true}); err != nil {
		t.Fatalf("db verify --write: %v", err)
	}

	// Hand-edit the migration: lint reports the checksum with the gombit fix.
	if err := os.WriteFile(up, []byte("CREATE TABLE widgets (id integer PRIMARY KEY, name text NOT NULL, note text NULL);\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run("db", "lint")
	if err == nil || !strings.Contains(out, "gombit db repair") || strings.Contains(out, "atlas migrate hash") {
		t.Fatalf("db lint after an edit: err = %v, out:\n%s", err, out)
	}

	// Repair rehashes and replays, then refuses the stale manifest.
	out, err = run("db", "repair")
	if err == nil || !strings.Contains(out, "stale safety manifest: 20260101000000_create_widgets") || !strings.Contains(err.Error(), "--write-manifests") {
		t.Fatalf("db repair with a stale manifest: err = %v, out:\n%s", err, out)
	}
	out, err = run("db", "repair", "--write-manifests")
	if err != nil || !strings.Contains(out, "The migration directory is consistent") {
		t.Fatalf("db repair --write-manifests: err = %v, out:\n%s", err, out)
	}
	if out, err := run("db", "lint"); err != nil {
		t.Fatalf("db lint after repair: err = %v, out:\n%s", err, out)
	}

	if err := runVerify(new(bytes.Buffer), new(bytes.Buffer), verifyOptions{dir: dir}); err != nil {
		t.Fatalf("db verify after repair: %v", err)
	}

	// A broken edit: repair's replay check fails, and atlas.sum is put back,
	// so the broken SQL is not left checksummed.
	sum := filepath.Join(dir, "atlas.sum")
	before, _ := os.ReadFile(sum) // #nosec G304 -- atlas.sum in the test temp app
	if err := os.WriteFile(up, []byte("CREATE TABL widgets (id integer PRIMARY KEY);\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = run("db", "repair")
	if err == nil || !strings.Contains(out+err.Error(), "atlas.sum was left as it was") {
		t.Fatalf("db repair with broken SQL: err = %v, out:\n%s", err, out)
	}
	if after, _ := os.ReadFile(sum); string(after) != string(before) { // #nosec G304 -- atlas.sum in the test temp app
		t.Fatalf("atlas.sum changed after a failed repair:\n%s\nwant\n%s", after, before)
	}
}
