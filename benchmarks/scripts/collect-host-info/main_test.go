package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gombit-dev/gombit/benchmarks/internal/metadata"
)

// Stamp mode must fold this run's provenance into the snapshot on disk without
// disturbing anything else — the whole reason a 40-second microbenchmark
// refresh is allowed to touch a file an hours-long CRUD sweep also owns
// (issue #266).
func TestStampGroupPreservesTheExistingSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.json")
	existing := metadata.Metadata{
		SchemaVersion:   metadata.SchemaVersion,
		GitCommit:       "aaaa1111",
		CPUModel:        "Old Bench Host",
		PostgresVersion: "postgres:16.4-alpine",
		Trials:          5,
		Concurrency:     []int{1, 10, 100, 500, 1000},
		Groups:          map[string]metadata.Provenance{metadata.GroupCRUD: {GitCommit: "aaaa1111"}},
	}
	writeFixture(t, path, existing)

	collected := metadata.Metadata{GitCommit: "bbbb2222", CPUModel: "New Dev Host", GoVersion: "go1.26.1"}
	got, err := stampGroup(path, metadata.GroupMicrobench, collected)
	if err != nil {
		t.Fatalf("stampGroup: %v", err)
	}

	if got.Groups[metadata.GroupMicrobench].GitCommit != "bbbb2222" {
		t.Errorf("microbench group = %+v, want the collected commit", got.Groups[metadata.GroupMicrobench])
	}
	if got.Groups[metadata.GroupCRUD].GitCommit != "aaaa1111" {
		t.Errorf("the CRUD group was disturbed: %+v", got.Groups[metadata.GroupCRUD])
	}
	if got.GitCommit != "aaaa1111" || got.CPUModel != "Old Bench Host" ||
		got.PostgresVersion != "postgres:16.4-alpine" || got.Trials != 5 {
		t.Errorf("stamping restamped fields it does not own: %+v", got)
	}
}

// A fresh OUT_DIR has no metadata.json yet; the first group to run must be able
// to stamp itself into an otherwise-empty record rather than failing.
func TestStampGroupOnMissingFileStartsAFreshRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.json")
	got, err := stampGroup(path, metadata.GroupFootprint, metadata.Metadata{GitCommit: "cccc3333"})
	if err != nil {
		t.Fatalf("stampGroup on a missing file: %v", err)
	}
	if got.SchemaVersion != metadata.SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", got.SchemaVersion, metadata.SchemaVersion)
	}
	if got.Groups[metadata.GroupFootprint].GitCommit != "cccc3333" {
		t.Errorf("footprint group = %+v", got.Groups[metadata.GroupFootprint])
	}
}

// A corrupt snapshot must fail loudly. Silently replacing it would discard
// whatever hours-long run produced the file.
func TestStampGroupRefusesACorruptSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := stampGroup(path, metadata.GroupMicrobench, metadata.Metadata{}); err == nil {
		t.Error("stampGroup on a corrupt file = nil error; want a failure, not a silent overwrite")
	}
}

func writeFixture(t *testing.T, path string, m metadata.Metadata) {
	t.Helper()
	f, err := os.Create(path) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.WriteJSON(f, m); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestParseIntListRejectsInvalidToken(t *testing.T) {
	// A malformed token is an error, not a silently dropped element — the
	// recorded sweep must equal the requested one or fail.
	for _, in := range []string{"1,10,abc,100", "1,,10", "100o", "1, 2, x"} {
		if got, err := parseIntList(in); err == nil {
			t.Errorf("parseIntList(%q) = %v, nil; want an error", in, got)
		}
	}
}

func TestParseIntListValid(t *testing.T) {
	got, err := parseIntList("1, 10 , 100,500,1000")
	if err != nil {
		t.Fatalf("parseIntList: %v", err)
	}
	want := []int{1, 10, 100, 500, 1000}
	if len(got) != len(want) {
		t.Fatalf("parseIntList len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("parseIntList[%d] = %d, want %d", i, got[i], want[i])
		}
	}

	if got, err := parseIntList(""); err != nil || got != nil {
		t.Errorf("parseIntList(\"\") = %v, %v; want nil, nil", got, err)
	}
}

func TestParseKeyValsEmptyIsNonNilMap(t *testing.T) {
	m, err := parseKeyVals("")
	if err != nil {
		t.Fatalf("parseKeyVals(\"\"): %v", err)
	}
	if m == nil {
		t.Fatal("parseKeyVals(\"\") = nil; want an empty (non-nil) map")
	}
	if len(m) != 0 {
		t.Errorf("parseKeyVals(\"\") = %v, want empty", m)
	}
}

func TestParseKeyValsValid(t *testing.T) {
	got, err := parseKeyVals("gombit=v0.1.0, gin-gorm=v1.11.0 ,go=1.25.7,empty=")
	if err != nil {
		t.Fatalf("parseKeyVals: %v", err)
	}
	if got["gombit"] != "v0.1.0" || got["gin-gorm"] != "v1.11.0" || got["go"] != "1.25.7" {
		t.Errorf("parseKeyVals = %v", got)
	}
	// An explicit empty value (django=) is a recorded fact, kept; a missing
	// '=' (django) is not — see TestParseKeyValsFailsClosed.
	if v, ok := got["empty"]; !ok || v != "" {
		t.Errorf("parseKeyVals should keep an explicit empty value: %v", got)
	}
}

// The sibling of parseIntList must fail closed too: a token that can't be a
// key=value pair is an error, never a silently dropped version. Otherwise the
// document claims a framework/runtime set that isn't what ran.
func TestParseKeyValsFailsClosed(t *testing.T) {
	for _, in := range []string{
		"gombit=v0.1.0,django",        // 'django' has no '='
		"go=1.25.7,python",            // 'python' has no '='
		"gombit=v0.1.0,",              // trailing comma
		"=v0.1.0",                     // empty key
		"gombit=v0.1.0,gombit=v0.2.0", // duplicate key
	} {
		if got, err := parseKeyVals(in); err == nil {
			t.Errorf("parseKeyVals(%q) = %v, nil; want an error", in, got)
		}
	}
}

// The no -group path rewrites the whole snapshot, but must still carry every
// group's provenance forward. Dropping it would silently re-point all three
// README captions at this collection's host via the report's legacy fallback —
// and this command measured nothing (issue #266).
func TestCarryGroupsPreservesProvenanceAcrossAWholeSnapshotRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.json")
	clean := false
	writeFixture(t, path, metadata.Metadata{
		SchemaVersion: metadata.SchemaVersion,
		Groups: map[string]metadata.Provenance{
			metadata.GroupMicrobench: {GitCommit: "aaaa1111", CPUModel: "Micro Host", GitDirty: &clean},
			metadata.GroupCRUD:       {GitCommit: "bbbb2222", CPUModel: "CRUD Host", GitDirty: &clean},
		},
	})

	collected := metadata.Metadata{
		SchemaVersion: metadata.SchemaVersion,
		GitCommit:     "cccc3333",
		CPUModel:      "Metadata-only Host",
		Groups:        map[string]metadata.Provenance{},
	}
	got, err := carryGroups(path, collected)
	if err != nil {
		t.Fatalf("carryGroups: %v", err)
	}

	if got.Groups[metadata.GroupMicrobench].GitCommit != "aaaa1111" ||
		got.Groups[metadata.GroupCRUD].GitCommit != "bbbb2222" {
		t.Errorf("group provenance was destroyed by a whole-snapshot rewrite: %+v", got.Groups)
	}
	// This collection's own fields still win — it is a rewrite, not a merge.
	if got.GitCommit != "cccc3333" || got.CPUModel != "Metadata-only Host" {
		t.Errorf("the collection's own top-level block should be written: %+v", got)
	}
}

// A fresh OUT_DIR has nothing to carry; the rewrite must succeed rather than
// fail on the missing file.
func TestCarryGroupsOnMissingFileIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.json")
	got, err := carryGroups(path, metadata.Metadata{GitCommit: "cccc3333"})
	if err != nil {
		t.Fatalf("carryGroups on a missing file: %v", err)
	}
	if len(got.Groups) != 0 {
		t.Errorf("Groups = %v, want empty", got.Groups)
	}
}
