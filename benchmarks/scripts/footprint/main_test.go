package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/benchmarks/internal/footprint"
	"github.com/gombit-dev/gombit/benchmarks/internal/metadata"
	"github.com/gombit-dev/gombit/benchmarks/internal/report"
)

func TestRunMergesContainerAndEmbeddedRows(t *testing.T) {
	out := filepath.Join(t.TempDir(), "footprint.json")
	var so, se bytes.Buffer

	// First a container row.
	code := run([]string{
		"-framework", "gombit", "-framework-version", "v0.1.3",
		"-runtime", "go", "-runtime-version", "go1.25.7",
		"-variant", "container", "-cold-start-ms", "200,220,240",
		"-idle-rss-bytes", "18000000", "-out", out,
	}, &so, &se)
	if code != 0 {
		t.Fatalf("container run exit=%d stderr=%s", code, se.String())
	}
	// Then an embedded row for the same framework — must accumulate, not replace.
	code = run([]string{
		"-framework", "gombit", "-variant", "embedded",
		"-cold-start-ms", "150", "-binary-size-bytes", "25000000", "-out", out,
	}, &so, &se)
	if code != 0 {
		t.Fatalf("embedded run exit=%d stderr=%s", code, se.String())
	}

	f, err := os.Open(out) //nolint:gosec // test reads a temp file it just wrote
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rows, err := footprint.ReadJSON(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (container + embedded)", len(rows))
	}
	byVariant := map[string]footprint.Footprint{}
	for _, r := range rows {
		byVariant[r.Variant] = r
	}
	// median of 200,220,240 is 220.
	if byVariant["container"].ColdStart.MedianMs != 220 {
		t.Errorf("container cold-start median = %v, want 220", byVariant["container"].ColdStart.MedianMs)
	}
	if byVariant["embedded"].BinarySizeBytes != 25000000 {
		t.Errorf("embedded binary size = %d, want 25000000", byVariant["embedded"].BinarySizeBytes)
	}
	// The sibling CSV is written too.
	if _, err := os.Stat(strings.TrimSuffix(out, ".json") + ".csv"); err != nil {
		t.Errorf("sibling .csv not written: %v", err)
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	out := filepath.Join(t.TempDir(), "footprint.json")
	cases := map[string][]string{
		"missing framework":   {"-variant", "container", "-out", out},
		"bad cold-start":      {"-framework", "x", "-variant", "container", "-cold-start-ms", "1,two,3", "-out", out},
		"negative cold-start": {"-framework", "x", "-variant", "container", "-cold-start-ms", "-5", "-out", out},
	}
	for name, args := range cases {
		var so, se bytes.Buffer
		if code := run(args, &so, &se); code == 0 {
			t.Errorf("%s: exit=0, want non-zero", name)
		}
	}
}

// An unreadable metadata.json must fail the run before footprint.json is
// touched. Merging the row first and failing at the stamp would leave a fresh
// row on disk beside the previous run's provenance for its unit.
func TestRunFailsBeforeWritingRowsWhenMetadataIsCorrupt(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "footprint.json")
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var so, se bytes.Buffer
	code := run([]string{"-framework", "gombit", "-variant", "container", "-cold-start-ms", "200", "-out", out}, &so, &se)
	if code == 0 {
		t.Fatal("a corrupt metadata.json must fail the run")
	}
	for _, p := range []string{out, strings.TrimSuffix(out, ".json") + ".csv"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s was written before the metadata failure (stat err: %v)", filepath.Base(p), err)
		}
	}
}

// The review-round-2 blocker, end to end through the real writer: start from the
// committed six-row footprint snapshot, re-measure one app the way
// `APPS=gombit make benchmark-footprint` does, and render the README. The five
// untouched rows must keep their own provenance, and the table must refuse a
// table-wide caption naming the re-run's commit.
func TestSubsetRefreshThroughTheWriterNeverRelabelsUntouchedRows(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"footprint.json", "metadata.json"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "results", "latest", name)) //nolint:gosec // fixed committed snapshot path
		if err != nil {
			t.Fatalf("read committed %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil { //nolint:gosec // dir is t.TempDir(), name a fixed literal
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "footprint.json")
	before := readMetadata(t, filepath.Join(dir, "metadata.json"))
	prints := readFootprint(t, out)
	if len(prints) < 2 {
		t.Fatalf("committed snapshot has %d footprint rows; the scenario needs several", len(prints))
	}

	var so, se bytes.Buffer
	if code := run([]string{"-framework", "gombit", "-variant", "container", "-cold-start-ms", "200", "-out", out}, &so, &se); code != 0 {
		t.Fatalf("run exit=%d stderr=%s", code, se.String())
	}
	after := readMetadata(t, filepath.Join(dir, "metadata.json"))
	prints = readFootprint(t, out)

	refreshed := after.UnitProvenance(metadata.GroupFootprint, "gombit:container")
	if refreshed.Empty() {
		t.Fatal("the re-measured row must record its own provenance")
	}
	for _, p := range prints {
		if p.Framework == "gombit" {
			continue
		}
		was := before.UnitProvenance(metadata.GroupFootprint, p.ProvenanceUnit())
		now := after.UnitProvenance(metadata.GroupFootprint, p.ProvenanceUnit())
		if !was.ComparableTo(now) || was.Timestamp != now.Timestamp {
			t.Errorf("untouched row %s was relabelled: %s -> %s", p.ProvenanceUnit(), was.GitCommit, now.GitCommit)
		}
	}

	if refreshed.ComparableTo(before.UnitProvenance(metadata.GroupFootprint, "rails:container")) {
		t.Skip("the committed rows were measured at this very commit and host; no mixed state to render")
	}
	rendered := report.Render(nil, prints, nil, after)
	section := rendered[strings.Index(rendered, "### Operational footprint"):]
	if !strings.Contains(section, "Rows were not measured together") {
		t.Errorf("a table mixing two source states must refuse a table-wide caption:\n%s", section)
	}
	if strings.Contains(section, "_Measured at `"+refreshed.GitCommit[:12]+"`") {
		t.Errorf("the subset run's commit captions the whole table:\n%s", section)
	}
}

func readMetadata(t *testing.T, path string) metadata.Metadata {
	t.Helper()
	m, err := metadata.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func readFootprint(t *testing.T, path string) []footprint.Footprint {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rows, err := footprint.ReadJSON(f)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
