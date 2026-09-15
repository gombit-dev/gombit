package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/benchmarks/internal/metadata"
	"github.com/gombit-dev/gombit/benchmarks/internal/microbench"
	"github.com/gombit-dev/gombit/benchmarks/internal/report"
)

// benchAll returns full `go test -bench` output (all five scenarios) with the
// given ns/op for valid-post.
func benchAll(validPostNs string) string {
	var b strings.Builder
	for _, s := range microbench.Scenarios {
		ns := "1000"
		if s == "valid-post" {
			ns = validPostNs
		}
		b.WriteString("BenchmarkFrameworkTax/" + s + "-16   100   " + ns + " ns/op   512 B/op   8 allocs/op\n")
	}
	return b.String()
}

func TestRunAccumulatesStacks(t *testing.T) {
	out := filepath.Join(t.TempDir(), "microbench.json")
	feed := func(stack, ns string) int {
		var so, se bytes.Buffer
		return run([]string{"-stack", stack, "-out", out}, strings.NewReader(benchAll(ns)), &so, &se)
	}
	if code := feed("nethttp", "900"); code != 0 {
		t.Fatalf("nethttp exit=%d", code)
	}
	if code := feed("gombit", "3100"); code != 0 {
		t.Fatalf("gombit exit=%d", code)
	}

	f, err := os.Open(out) //nolint:gosec // test reads a temp file it just wrote
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rows, err := microbench.ReadJSON(f)
	if err != nil {
		t.Fatal(err)
	}
	// Two stacks × five scenarios.
	if len(rows) != 2*len(microbench.Scenarios) {
		t.Fatalf("rows = %d, want %d", len(rows), 2*len(microbench.Scenarios))
	}
	got := map[string]float64{}
	for _, r := range rows {
		if r.Scenario == "valid-post" {
			got[r.Stack] = r.MedianNsPerOp()
		}
	}
	if got["nethttp"] != 900 || got["gombit"] != 3100 {
		t.Errorf("stacks not accumulated: %+v", got)
	}
}

func TestRunRejectsEmptyStackOrIncompleteOutput(t *testing.T) {
	out := filepath.Join(t.TempDir(), "microbench.json")
	var so, se bytes.Buffer
	if code := run([]string{"-out", out}, strings.NewReader(""), &so, &se); code == 0 {
		t.Error("missing -stack should be non-zero")
	}
	// Only one scenario present -> incomplete run -> must fail (not write a partial file).
	partial := "BenchmarkFrameworkTax/json-16   1   1000 ns/op   1 B/op   1 allocs/op\n"
	if code := run([]string{"-stack", "gin", "-out", out}, strings.NewReader(partial), &so, &se); code == 0 {
		t.Error("an incomplete bench run must fail, not write a partial stack")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("a rejected run must not have written the output file")
	}
}

// An unreadable metadata.json must fail the run before microbench.json is
// touched. Merging the rows first and failing at the stamp would leave fresh
// rows on disk beside the previous run's provenance for their stack.
func TestRunFailsBeforeWritingRowsWhenMetadataIsCorrupt(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "microbench.json")
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var so, se bytes.Buffer
	if code := run([]string{"-stack", "gin", "-out", out}, strings.NewReader(benchAll("1000")), &so, &se); code == 0 {
		t.Fatal("a corrupt metadata.json must fail the run")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("microbench.json was written before the metadata failure (stat err: %v)", err)
	}
}

// The review-round-2 rule for the last producer: a partial microbench write must
// never relabel stacks it did not measure. Two documented partial writes are
// driven through the real writer from the committed snapshot: one stack
// (`microbench -stack gin`, as this command's doc comment shows) and the
// ablation ladder (`make benchmark-micro-ablation`, which owns only the
// gombit-ablation namespace).
func TestPartialWriteThroughTheWriterNeverRelabelsOtherStacks(t *testing.T) {
	ablation := func() string {
		var b strings.Builder
		for _, row := range []string{"baseline", "full-app"} {
			for _, s := range microbench.Scenarios {
				b.WriteString("BenchmarkAblation/gombit-ablation/" + row + "/" + s + "-16   100   1500 ns/op   256 B/op   4 allocs/op\n")
			}
		}
		return b.String()
	}
	published := []string{"nethttp", "gin", "huma", "gombit"}

	for name, c := range map[string]struct {
		stack, input string
		refreshed    string // the published stack this write re-measures, if any
	}{
		"one stack":       {stack: "gin", input: benchAll("1000"), refreshed: "gin"},
		"ablation ladder": {stack: "gombit-ablation", input: ablation()},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range []string{"microbench.json", "metadata.json"} {
				data, err := os.ReadFile(filepath.Join("..", "..", "results", "latest", f)) //nolint:gosec // fixed committed snapshot path
				if err != nil {
					t.Fatalf("read committed %s: %v", f, err)
				}
				if err := os.WriteFile(filepath.Join(dir, f), data, 0o600); err != nil { //nolint:gosec // dir is t.TempDir(), f a fixed literal
					t.Fatal(err)
				}
			}
			out := filepath.Join(dir, "microbench.json")
			before, err := metadata.ReadFile(filepath.Join(dir, "metadata.json"))
			if err != nil {
				t.Fatal(err)
			}

			var so, se bytes.Buffer
			if code := run([]string{"-stack", c.stack, "-out", out}, strings.NewReader(c.input), &so, &se); code != 0 {
				t.Fatalf("run exit=%d stderr=%s", code, se.String())
			}
			after, err := metadata.ReadFile(filepath.Join(dir, "metadata.json"))
			if err != nil {
				t.Fatal(err)
			}
			if after.UnitProvenance(metadata.GroupMicrobench, c.stack).Empty() {
				t.Fatalf("the written namespace %q must record its own provenance", c.stack)
			}
			for _, s := range published {
				if s == c.refreshed {
					continue
				}
				was := before.UnitProvenance(metadata.GroupMicrobench, s)
				now := after.UnitProvenance(metadata.GroupMicrobench, s)
				if !was.ComparableTo(now) || was.Timestamp != now.Timestamp {
					t.Errorf("untouched stack %s was relabelled: %s -> %s", s, was.GitCommit, now.GitCommit)
				}
			}

			taxSection := func(rowsPath string, meta metadata.Metadata) string {
				f, err := os.Open(rowsPath) //nolint:gosec // test-owned or committed snapshot path
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = f.Close() }()
				rows, err := microbench.ReadJSON(f)
				if err != nil {
					t.Fatal(err)
				}
				rendered := report.Render(nil, nil, rows, meta)
				return rendered[strings.Index(rendered, "### Framework tax"):strings.Index(rendered, "### PostgreSQL CRUD read")]
			}
			section := taxSection(out, after)
			if c.refreshed == "" {
				// The ablation publishes nothing, so the published ladder must render
				// exactly as it did before the write: same rows, same caption.
				if want := taxSection(filepath.Join("..", "..", "results", "latest", "microbench.json"), before); section != want {
					t.Errorf("an ablation write changed the published ladder:\n--- before\n%s\n--- after\n%s", want, section)
				}
				return
			}
			if before.UnitProvenance(metadata.GroupMicrobench, "nethttp").ComparableTo(after.UnitProvenance(metadata.GroupMicrobench, c.refreshed)) {
				t.Skip("the committed ladder was measured at this very commit and host; no mixed state to render")
			}
			if !strings.Contains(section, "Rows were not measured together") {
				t.Errorf("a ladder mixing two source states must refuse a table-wide caption:\n%s", section)
			}
		})
	}
}
