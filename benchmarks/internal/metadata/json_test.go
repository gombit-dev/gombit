package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The required framework_versions / runtime_versions / concurrency fields must
// never serialize as JSON null, even at the CLI's default (no version flags,
// no concurrency): Collect initializes empty maps/slice, so the wire shape is
// {} / [], the honest "collected, nothing to record" value.
func TestWriteJSONNeverNullForRequiredFields(t *testing.T) {
	m := Collect(context.Background(), Options{
		Run: func(context.Context, string, ...string) (string, error) { return "", nil },
	})

	var buf bytes.Buffer
	if err := WriteJSON(&buf, m); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	s := buf.String()
	for _, wantNull := range []string{"framework_versions", "runtime_versions", "concurrency"} {
		if strings.Contains(s, `"`+wantNull+`": null`) {
			t.Errorf("%s serialized as null:\n%s", wantNull, s)
		}
	}
	for _, want := range []string{
		`"framework_versions": {}`,
		`"runtime_versions": {}`,
		`"concurrency": []`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("json missing %q:\n%s", want, s)
		}
	}

	// It must round-trip back to equal values.
	var decoded Metadata
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.FrameworkVersions == nil || decoded.Concurrency == nil {
		t.Errorf("round trip lost empty collections: %+v", decoded)
	}
}

// StampUnitFile is the one read-modify-write every row-writer uses, so the
// "a producer stamps only what it measured" invariant is enforced here once
// rather than trusted at three call sites.
func TestStampUnitFileAddsOneUnitAndPreservesTheRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.json")
	seed := Metadata{SchemaVersion: SchemaVersion, PostgresVersion: "postgres:16.4-alpine", Trials: 5}
	seed = StampUnit(seed, GroupFootprint, "rails", Provenance{GitCommit: "aaaa1111"})
	seed = StampUnit(seed, GroupCRUD, "rails", Provenance{GitCommit: "aaaa1111"})
	writeTo(t, path, seed)

	if err := StampUnitFile(path, GroupFootprint, "gombit", Provenance{GitCommit: "bbbb2222"}); err != nil {
		t.Fatalf("StampUnitFile: %v", err)
	}

	got := readFrom(t, path)
	if got.Groups[GroupFootprint]["gombit"].GitCommit != "bbbb2222" {
		t.Errorf("the stamped unit = %+v", got.Groups[GroupFootprint]["gombit"])
	}
	if got.Groups[GroupFootprint]["rails"].GitCommit != "aaaa1111" {
		t.Errorf("a sibling unit was disturbed: %+v", got.Groups[GroupFootprint]["rails"])
	}
	if got.Groups[GroupCRUD]["rails"].GitCommit != "aaaa1111" {
		t.Errorf("a sibling group was disturbed: %+v", got.Groups[GroupCRUD])
	}
	if got.PostgresVersion != "postgres:16.4-alpine" || got.Trials != 5 {
		t.Errorf("shared run parameters were overwritten: %+v", got)
	}
}

// A fresh OUT_DIR: the first producer to run starts the record rather than
// failing on the missing file.
func TestStampUnitFileOnMissingFileStartsTheRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "metadata.json")
	if err := StampUnitFile(path, GroupMicrobench, "gin", Provenance{GitCommit: "cccc3333"}); err != nil {
		t.Fatalf("StampUnitFile on a missing file: %v", err)
	}
	got := readFrom(t, path)
	if got.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", got.SchemaVersion, SchemaVersion)
	}
	if got.Groups[GroupMicrobench]["gin"].GitCommit != "cccc3333" {
		t.Errorf("Groups = %+v", got.Groups)
	}
}

// Fail closed: a corrupt snapshot, an unknown group, or a missing unit must all
// error rather than write something a reader would later trust.
func TestStampUnitFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := StampUnitFile(corrupt, GroupCRUD, "gombit", Provenance{}); err == nil {
		t.Error("a corrupt snapshot must not be silently replaced")
	}

	ok := filepath.Join(dir, "ok.json")
	if err := StampUnitFile(ok, "microbnech", "gin", Provenance{}); err == nil {
		t.Error("an unknown group must be rejected, not written as a phantom entry")
	}
	if err := StampUnitFile(ok, GroupCRUD, "", Provenance{}); err == nil {
		t.Error("an empty unit must be rejected — a group-wide stamp is the bug this replaces")
	}
}

func TestSiblingPath(t *testing.T) {
	if got := SiblingPath(filepath.Join("a", "b", "footprint.json")); got != filepath.Join("a", "b", "metadata.json") {
		t.Errorf("SiblingPath = %q", got)
	}
}

func writeTo(t *testing.T, path string, m Metadata) {
	t.Helper()
	f, err := os.Create(path) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(f, m); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func readFrom(t *testing.T, path string) Metadata {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	m, err := ReadJSON(f)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
