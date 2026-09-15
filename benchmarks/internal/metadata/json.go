package metadata

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// WriteJSON encodes the metadata as pretty-printed JSON (results/latest/
// metadata.json).
func WriteJSON(w io.Writer, m Metadata) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// ReadJSON decodes a metadata.json. It is the inverse of WriteJSON, used by the
// producers that merge into an existing snapshot rather than replacing it.
func ReadJSON(r io.Reader) (Metadata, error) {
	var m Metadata
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return Metadata{}, err
	}
	return m, nil
}

// ReadFile returns the metadata.json at path. It is the one way every producer
// reads an existing snapshot before adding to it.
//
// A missing file is not an error: the first producer to run in a fresh OUT_DIR
// starts the record, so it returns an empty record at the current
// SchemaVersion. A file that exists but cannot be read or parsed IS an error —
// silently replacing a corrupt snapshot would discard whatever hours-long runs
// produced it, along with every unit's provenance.
//
// Producers call it before writing any rows, not only when stamping afterwards:
// otherwise a corrupt metadata.json is discovered after the rows are already
// merged, leaving them on disk beside the previous run's provenance for their
// unit.
func ReadFile(path string) (Metadata, error) {
	// path is composed from an operator-supplied output dir, not untrusted
	// input — G304 does not apply.
	f, err := os.Open(path) //nolint:gosec
	if os.IsNotExist(err) {
		return Metadata{SchemaVersion: SchemaVersion}, nil
	}
	if err != nil {
		return Metadata{}, fmt.Errorf("metadata: read %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	m, err := ReadJSON(f)
	if err != nil {
		return Metadata{}, fmt.Errorf("metadata: read %s: %w", path, err)
	}
	return m, nil
}

// StampUnitFile records one unit's provenance into the metadata.json at path,
// preserving everything already there.
//
// It is the single read-modify-write used by every producer that writes rows:
// scripts/microbench, scripts/footprint and scripts/run-crud each call it for
// the unit they just measured. Keeping one implementation is what makes the
// "a producer stamps only what it measured" invariant enforceable rather than a
// convention three call sites are trusted to follow.
//
// It reads through ReadFile, so it fails closed on a snapshot it cannot read.
func StampUnitFile(path, group, unit string, prov Provenance) error {
	if !ValidGroup(group) {
		return fmt.Errorf("metadata: unknown group %q", group)
	}
	if unit == "" {
		return fmt.Errorf("metadata: group %q needs a unit to stamp", group)
	}

	existing, err := ReadFile(path)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // operator-supplied out dir
		return err
	}
	out, err := os.Create(path) //nolint:gosec
	if err != nil {
		return err
	}
	if err := WriteJSON(out, StampUnit(existing, group, unit, prov)); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// SiblingPath returns the metadata.json that belongs beside a data file, so a
// producer given `-out .../footprint.json` writes provenance to the snapshot it
// is contributing to without a second path flag to keep in sync.
func SiblingPath(dataFile string) string {
	return filepath.Join(filepath.Dir(dataFile), "metadata.json")
}
