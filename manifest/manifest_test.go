package manifest

import (
	"errors"
	"testing"
)

func TestGenerateAndVerifyRoundTrip(t *testing.T) {
	sql := `CREATE TABLE "users" ("id" bigserial NOT NULL, PRIMARY KEY ("id"));
ALTER TABLE "users" DROP COLUMN "legacy";`
	mig := Migration{Version: "20260901120000", Name: "drop_legacy"}

	m := Generate(mig, sql)
	if m.ManifestVersion != ManifestVersion {
		t.Fatalf("manifest_version = %d, want %d", m.ManifestVersion, ManifestVersion)
	}
	if !m.RequiresConfirmation {
		t.Fatal("requires_confirmation = false, want true (SQL drops a column)")
	}
	if err := Verify(m, sql); err != nil {
		t.Fatalf("Verify(generated) error = %v, want nil", err)
	}
}

func TestVerifyNonDestructiveMigration(t *testing.T) {
	sql := `CREATE TABLE "users" ("id" bigserial NOT NULL, PRIMARY KEY ("id"));`
	m := Generate(Migration{Version: "1", Name: "create_users"}, sql)
	if m.RequiresConfirmation {
		t.Fatal("requires_confirmation = true, want false")
	}
	if err := Verify(m, sql); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
}

// Adversarial case (DESIGN.md §100): the SQL was changed after the manifest was
// written. The hash no longer matches.
func TestVerifyRejectsTamperedSQL(t *testing.T) {
	original := `ALTER TABLE "customers" ADD COLUMN "note" text;`
	m := Generate(Migration{Version: "1", Name: "add_note"}, original)

	tampered := `ALTER TABLE "customers" DROP COLUMN "legacy_code";`
	err := Verify(m, tampered)
	if !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("Verify(tampered) error = %v, want ErrHashMismatch", err)
	}
}

// Adversarial case (DESIGN.md §100): a manifest that claims the migration is
// non-destructive while the SQL drops a column must fail verification (§31).
func TestVerifyRejectsMisdeclaredSafety(t *testing.T) {
	sql := `ALTER TABLE "customers" DROP COLUMN "legacy_code";`

	// A hand-forged manifest: correct hash, but lies about the classification.
	forged := SafetyManifest{
		ManifestVersion:      ManifestVersion,
		Migration:            Migration{Version: "1", Name: "drop_legacy"},
		SQLSHA256:            HashSQL(sql),
		RequiresConfirmation: false,
		Operations: []Operation{
			{Kind: OpOther, Resource: "customers", Safety: SafetyNonDestructive},
		},
	}

	err := Verify(forged, sql)
	if !errors.Is(err, ErrClassificationMismatch) {
		t.Fatalf("Verify(forged) error = %v, want ErrClassificationMismatch", err)
	}
}

func TestVerifyRejectsUnknownManifestVersion(t *testing.T) {
	sql := `CREATE TABLE "users" ("id" bigserial NOT NULL);`
	m := Generate(Migration{Version: "1", Name: "create_users"}, sql)
	m.ManifestVersion = 999

	if err := Verify(m, sql); err == nil {
		t.Fatal("Verify() error = nil, want error for unknown manifest_version")
	}
}
