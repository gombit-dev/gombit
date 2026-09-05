package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/manifest"
)

func migrationsDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, sql := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(sql), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRunVerifyClassifiesDataLoss(t *testing.T) {
	dir := migrationsDir(t, map[string]string{
		"0001_create_users.sql": `CREATE TABLE "users" ("id" bigserial NOT NULL);`,
		"0002_drop_legacy.sql":  `ALTER TABLE "users" DROP COLUMN "legacy";`,
	})

	var out, errOut bytes.Buffer
	if err := runVerify(&out, &errOut, dir, false, false); err != nil {
		t.Fatalf("runVerify() error = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "0002_drop_legacy: REQUIRES CONFIRMATION") {
		t.Errorf("output missing data-loss flag:\n%s", got)
	}
	if !strings.Contains(got, "0001_create_users: safe") {
		t.Errorf("output missing safe classification:\n%s", got)
	}
}

func TestRunVerifyWriteThenVerifyPasses(t *testing.T) {
	dir := migrationsDir(t, map[string]string{
		"0001_drop_legacy.sql": `ALTER TABLE "users" DROP COLUMN "legacy";`,
	})

	var out, errOut bytes.Buffer
	if err := runVerify(&out, &errOut, dir, true, false); err != nil {
		t.Fatalf("runVerify(--write) error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "0001_drop_legacy.manifest.json")); err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
	out.Reset()
	errOut.Reset()
	if err := runVerify(&out, &errOut, dir, false, false); err != nil {
		t.Fatalf("runVerify() after --write error = %v; stderr=%s", err, errOut.String())
	}
}

func TestRunVerifyFailsOnTamperedSQL(t *testing.T) {
	dir := migrationsDir(t, map[string]string{
		"0001_add_note.sql": `ALTER TABLE "users" ADD COLUMN "note" text;`,
	})
	var out, errOut bytes.Buffer
	if err := runVerify(&out, &errOut, dir, true, false); err != nil {
		t.Fatalf("runVerify(--write) error = %v", err)
	}
	// Change the SQL after the manifest was written.
	if err := os.WriteFile(filepath.Join(dir, "0001_add_note.sql"),
		[]byte(`ALTER TABLE "users" DROP COLUMN "note";`), 0o600); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errOut.Reset()
	err := runVerify(&out, &errOut, dir, false, false)
	if err == nil {
		t.Fatal("runVerify() error = nil, want failure for tampered SQL")
	}
	if !strings.Contains(errOut.String(), "hash mismatch") {
		t.Errorf("stderr = %q, want hash mismatch", errOut.String())
	}
}

func TestRunVerifyFailsOnForgedManifest(t *testing.T) {
	dir := migrationsDir(t, map[string]string{
		"0001_drop_legacy.sql": `ALTER TABLE "users" DROP COLUMN "legacy";`,
	})
	// A manifest with the correct hash but a lie about the classification.
	sql := `ALTER TABLE "users" DROP COLUMN "legacy";`
	forged := manifest.SafetyManifest{
		ManifestVersion:      manifest.ManifestVersion,
		Migration:            manifest.Migration{Version: "0001", Name: "drop_legacy"},
		SQLSHA256:            manifest.HashSQL(sql),
		RequiresConfirmation: false,
		Operations:           []manifest.Operation{{Kind: manifest.OpOther, Resource: "users", Safety: manifest.SafetyNonDestructive}},
	}
	data, _ := json.MarshalIndent(forged, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "0001_drop_legacy.manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	err := runVerify(&out, &errOut, dir, false, false)
	if err == nil {
		t.Fatal("runVerify() error = nil, want failure for forged manifest")
	}
	if !strings.Contains(errOut.String(), "classification mismatch") {
		t.Errorf("stderr = %q, want classification mismatch", errOut.String())
	}
}

func TestRunVerifyJSON(t *testing.T) {
	dir := migrationsDir(t, map[string]string{
		"0001_drop_legacy.sql": `ALTER TABLE "users" DROP COLUMN "legacy";`,
	})
	var out, errOut bytes.Buffer
	if err := runVerify(&out, &errOut, dir, false, true); err != nil {
		t.Fatalf("runVerify(--json) error = %v", err)
	}
	var manifests []manifest.SafetyManifest
	if err := json.Unmarshal(out.Bytes(), &manifests); err != nil {
		t.Fatalf("unmarshal json: %v\n%s", err, out.String())
	}
	if len(manifests) != 1 || !manifests[0].RequiresConfirmation {
		t.Fatalf("manifests = %+v, want 1 requiring confirmation", manifests)
	}
}
