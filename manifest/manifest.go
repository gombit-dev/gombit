// Package manifest defines the migration safety manifest and its verifier —
// the HOST-3 contract (ADR-015). A manifest classifies whether a migration is
// destructive; the verifier proves that classification matches the executable
// SQL, so a deployment host (e.g. Gombit Cloud) can gate destructive migrations
// without blindly trusting a declared "safety" field (DESIGN.md §29–§31).
//
// Gombit classifies and verifies. It does not implement the approval gate — a
// host owns the policy of blocking a data-loss migration pending confirmation
// (DESIGN.md §32). The manifest does not touch the framework_migrations table
// (build plan D4); its sql_sha256 is anti-tamper binding, not drift detection.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// ManifestVersion is the schema version of an emitted manifest. A host can fail
// loudly on an unknown version.
const ManifestVersion = 1

// Migration identifies the migration a manifest describes.
type Migration struct {
	Version string `json:"version"`
	Name    string `json:"name"`
}

// SafetyManifest is the safety classification of one migration's SQL.
type SafetyManifest struct {
	ManifestVersion int       `json:"manifest_version"`
	Migration       Migration `json:"migration"`
	// SQLSHA256 binds the manifest to the exact reviewed SQL (anti-tamper).
	SQLSHA256 string `json:"sql_sha256"`
	// RequiresConfirmation is true when any operation loses data.
	RequiresConfirmation bool        `json:"requires_confirmation"`
	Operations           []Operation `json:"operations"`
}

// Generate builds the canonical manifest for a migration's SQL: the operations
// classified from the SQL, whether confirmation is required, and the SQL hash.
func Generate(mig Migration, sql string) SafetyManifest {
	ops := Classify(sql)
	return SafetyManifest{
		ManifestVersion:      ManifestVersion,
		Migration:            mig,
		SQLSHA256:            HashSQL(sql),
		RequiresConfirmation: requiresConfirmation(ops),
		Operations:           ops,
	}
}

// HashSQL returns the hex-encoded SHA-256 of the migration SQL.
func HashSQL(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(sum[:])
}

// Verify proves a manifest is consistent with the executable SQL. It fails when
//   - the manifest version is unknown;
//   - sql_sha256 does not match the SQL (the SQL was tampered with after review);
//   - the declared operations or requires_confirmation disagree with what the
//     SQL actually does (e.g. a manifest claims non_destructive while the SQL
//     drops a column).
//
// A host consumes Verify's result; it never trusts the manifest's own fields
// (DESIGN.md §31).
func Verify(m SafetyManifest, sql string) error {
	if m.ManifestVersion != ManifestVersion {
		return fmt.Errorf("manifest: unsupported manifest_version %d (want %d)", m.ManifestVersion, ManifestVersion)
	}
	want := Generate(m.Migration, sql)

	if m.SQLSHA256 != want.SQLSHA256 {
		return fmt.Errorf("%w: manifest sql_sha256 %s does not match SQL %s",
			ErrHashMismatch, short(m.SQLSHA256), short(want.SQLSHA256))
	}
	if m.RequiresConfirmation != want.RequiresConfirmation {
		return fmt.Errorf("%w: manifest requires_confirmation=%t but the SQL requires %t",
			ErrClassificationMismatch, m.RequiresConfirmation, want.RequiresConfirmation)
	}
	if !operationsEqual(m.Operations, want.Operations) {
		return fmt.Errorf("%w: declared operations do not match the SQL", ErrClassificationMismatch)
	}
	return nil
}

// ErrHashMismatch and ErrClassificationMismatch let callers distinguish a
// tampered-SQL failure from a mis-declared-safety failure.
var (
	ErrHashMismatch           = errors.New("manifest: sql hash mismatch")
	ErrClassificationMismatch = errors.New("manifest: classification mismatch")
)

func requiresConfirmation(ops []Operation) bool {
	for _, op := range ops {
		if op.Safety == SafetyDataLoss {
			return true
		}
	}
	return false
}

func operationsEqual(a, b []Operation) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12] + "…"
	}
	return hash
}
