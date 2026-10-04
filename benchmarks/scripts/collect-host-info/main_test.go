package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gombit-dev/gombit/benchmarks/internal/metadata"
)

// The whole-snapshot rewrite must carry every unit's provenance forward.
// Dropping it would silently re-point every README caption at this collection's
// host via the report's legacy fallback — and this command measured nothing
// (issue #266).
func TestCarryGroupsPreservesUnitProvenanceAcrossAWholeSnapshotRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.json")
	existing := metadata.Metadata{SchemaVersion: metadata.SchemaVersion}
	existing = metadata.StampUnit(existing, metadata.GroupMicrobench, "gin",
		metadata.Provenance{GitCommit: "aaaa1111", CPUModel: "Micro Host"})
	existing = metadata.StampUnit(existing, metadata.GroupCRUD, "rails",
		metadata.Provenance{GitCommit: "bbbb2222", CPUModel: "CRUD Host"})
	writeFixture(t, path, existing)

	collected := metadata.Metadata{
		SchemaVersion: metadata.SchemaVersion,
		GitCommit:     "cccc3333",
		CPUModel:      "Metadata-only Host",
		Groups:        map[string]map[string]metadata.Provenance{},
	}
	got, err := carryGroups(path, collected)
	if err != nil {
		t.Fatalf("carryGroups: %v", err)
	}

	if got.Groups[metadata.GroupMicrobench]["gin"].GitCommit != "aaaa1111" ||
		got.Groups[metadata.GroupCRUD]["rails"].GitCommit != "bbbb2222" {
		t.Errorf("unit provenance was destroyed by a whole-snapshot rewrite: %+v", got.Groups)
	}
	// This collection's own fields still win — it is a rewrite, not a merge.
	if got.GitCommit != "cccc3333" || got.CPUModel != "Metadata-only Host" {
		t.Errorf("the collection's own top-level block should be written: %+v", got)
	}
}

// `make benchmark-metadata` writes run parameters at the top level and measured
// nothing, so it cannot rewrite the protocol a recorded unit ran under (#377):
// a unit's own protocol is carried unchanged, and a unit stamped before
// protocols were per unit has the top-level parameters it ran under filed on it
// before they are replaced.
func TestCarryGroupsCannotRewriteARecordedUnitsProtocol(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.json")
	canonical := metadata.RunParams{Concurrency: []int{1, 10, 100}, Trials: 5, DurationSeconds: 30, WarmupSeconds: 10, BenchmarkTool: "grafana/k6:0.55.0"}
	own := metadata.RunParams{Concurrency: []int{1, 10}, Trials: 3, DurationSeconds: 10, WarmupSeconds: 2, BenchmarkTool: "grafana/k6:0.55.0"}
	existing := metadata.Metadata{
		SchemaVersion: metadata.SchemaVersion,
		Concurrency:   canonical.Concurrency, Trials: 5, DurationSeconds: 30, WarmupSeconds: 10, BenchmarkTool: canonical.BenchmarkTool,
	}
	existing = metadata.StampUnit(existing, metadata.GroupCRUD, "gombit:auth-jwt",
		metadata.Provenance{GitCommit: "bbbb2222", Protocol: &own})
	existing = metadata.StampUnit(existing, metadata.GroupCRUD, "rails:crud-list",
		metadata.Provenance{GitCommit: "bbbb2222"})
	writeFixture(t, path, existing)

	collected := metadata.Metadata{
		SchemaVersion: metadata.SchemaVersion,
		GitCommit:     "cccc3333",
		Concurrency:   []int{1}, Trials: 1, DurationSeconds: 1, WarmupSeconds: 0, BenchmarkTool: "k6",
		Groups: map[string]map[string]metadata.Provenance{},
	}
	got, err := carryGroups(path, collected)
	if err != nil {
		t.Fatalf("carryGroups: %v", err)
	}
	if p, ok := got.UnitRunParams(metadata.GroupCRUD, "gombit:auth-jwt"); !ok || !p.Equal(own) {
		t.Errorf("a unit's own protocol was rewritten: %+v, %v", p, ok)
	}
	if p, ok := got.UnitRunParams(metadata.GroupCRUD, "rails:crud-list"); !ok || !p.Equal(canonical) {
		t.Errorf("a legacy unit must keep the top-level protocol it ran under: %+v, %v", p, ok)
	}
	if !got.RunParams().Equal(collected.RunParams()) {
		t.Errorf("the collection's own top-level parameters should still be written: %+v", got.RunParams())
	}
}

// A snapshot that records no unit has its top level as every row's protocol
// (metadata.UnitRunParams), so the rewrite must carry those parameters exactly
// as it carries that snapshot's provenance (#377).
func TestCarryGroupsKeepsAPreGroupsSnapshotsProtocol(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.json")
	reduced := metadata.RunParams{Concurrency: []int{1}, Trials: 1, DurationSeconds: 1, WarmupSeconds: 1, BenchmarkTool: "grafana/k6:0.55.0"}
	writeFixture(t, path, metadata.Metadata{SchemaVersion: metadata.SchemaVersion, GitCommit: "aaaa1111"}.WithRunParams(reduced))

	canonical := metadata.RunParams{Concurrency: []int{1, 10, 100, 500, 1000}, Trials: 5, DurationSeconds: 30, WarmupSeconds: 10, BenchmarkTool: "grafana/k6:0.55.0"}
	collected := metadata.Metadata{SchemaVersion: metadata.SchemaVersion, GitCommit: "cccc3333"}.WithRunParams(canonical)
	got, err := carryGroups(path, collected)
	if err != nil {
		t.Fatalf("carryGroups: %v", err)
	}
	if p, ok := got.UnitRunParams(metadata.GroupCRUD, "gombit:crud-list"); !ok || !p.Equal(reduced) {
		t.Errorf("a pre-Groups snapshot's rows were re-described under this collection's parameters: %+v, %v", p, ok)
	}
	if got.GitCommit != "aaaa1111" {
		t.Errorf("a pre-Groups snapshot's provenance must still be carried, got %q", got.GitCommit)
	}
}

// `make benchmark-metadata` rewrites the top-level block with a collection that
// measured nothing. Carrying Groups is not enough on its own: a unit with no
// entry must not then pick up this collection's commit and host. Starts from the
// committed snapshot and the shapes it has had, since a fixture with every unit
// pre-stamped cannot exercise that path (issue #266).
func TestWholeSnapshotRewriteNeverReattributesARow(t *testing.T) {
	committed, err := os.ReadFile(filepath.Join("..", "..", "results", "latest", "metadata.json"))
	if err != nil {
		t.Fatalf("read committed snapshot: %v", err)
	}
	var snapshot metadata.Metadata
	if err := json.Unmarshal(committed, &snapshot); err != nil {
		t.Fatalf("parse committed snapshot: %v", err)
	}
	microOnly := snapshot
	microOnly.Groups = map[string]map[string]metadata.Provenance{
		metadata.GroupMicrobench: snapshot.Groups[metadata.GroupMicrobench],
	}
	legacy := snapshot
	legacy.Groups = nil

	for name, before := range map[string]metadata.Metadata{
		"committed snapshot":        snapshot,
		"microbench units only":     microOnly,
		"legacy snapshot, no units": legacy,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "metadata.json")
			writeFixture(t, path, before)
			collected := metadata.Metadata{
				SchemaVersion: metadata.SchemaVersion,
				GitCommit:     "cccc3333cccc",
				CPUModel:      "Metadata-only Host",
				Groups:        map[string]map[string]metadata.Provenance{},
			}
			after, err := carryGroups(path, collected)
			if err != nil {
				t.Fatalf("carryGroups: %v", err)
			}
			for _, u := range allUnits() {
				was := before.UnitProvenance(u.group, u.unit)
				now := after.UnitProvenance(u.group, u.unit)
				if now.Empty() || sameProvenance(was, now) {
					continue
				}
				t.Errorf("%s/%s was re-attributed by a collection that measured nothing: %s at %s -> %s at %s",
					u.group, u.unit, was.GitCommit, was.CPUModel, now.GitCommit, now.CPUModel)
			}
		})
	}
}

type unitRef struct{ group, unit string }

// allUnits is every unit the published README tables caption.
func allUnits() []unitRef {
	var units []unitRef
	for _, s := range []string{"nethttp", "gin", "huma", "gombit"} {
		units = append(units, unitRef{metadata.GroupMicrobench, s})
	}
	for _, fw := range []string{"django", "gin-gorm", "gombit", "laravel", "nestjs", "rails"} {
		units = append(units, unitRef{metadata.GroupCRUD, fw + ":crud-list"}, unitRef{metadata.GroupFootprint, fw + ":container"})
	}
	return units
}

func sameProvenance(a, b metadata.Provenance) bool {
	return a.ComparableTo(b) && a.Timestamp == b.Timestamp
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

// A corrupt snapshot must fail loudly. Silently replacing it would discard
// whatever hours-long run produced the file.
func TestCarryGroupsRefusesACorruptSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := carryGroups(path, metadata.Metadata{}); err == nil {
		t.Error("carryGroups on a corrupt file = nil error; want a failure, not a silent overwrite")
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
