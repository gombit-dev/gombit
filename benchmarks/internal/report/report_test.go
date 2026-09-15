package report

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/benchmarks/internal/footprint"
	"github.com/gombit-dev/gombit/benchmarks/internal/metadata"
	"github.com/gombit-dev/gombit/benchmarks/internal/microbench"
	"github.com/gombit-dev/gombit/benchmarks/internal/result"
)

func crudRow(fw string, conc, trial int, rps, p50, p95, p99 float64) result.Result {
	return result.Result{
		Framework: fw, Benchmark: "crud-list", Concurrency: conc, Trial: trial,
		RequestsPerSecond: rps,
		LatencyMs:         result.Latency{P50: p50, P95: p95, P99: p99},
	}
}

// taxLadder is a complete four-rung headline-scenario ladder — the minimum the
// framework-tax table will publish (a missing rung renders "incomplete"
// instead).
func taxLadder() []microbench.Row {
	return []microbench.Row{
		{Stack: "nethttp", Scenario: "valid-post", NsPerOp: []float64{800}},
		{Stack: "gin", Scenario: "valid-post", NsPerOp: []float64{900}},
		{Stack: "huma", Scenario: "valid-post", NsPerOp: []float64{1000}},
		{Stack: "gombit", Scenario: "valid-post", NsPerOp: []float64{3000}},
	}
}

func TestRenderCRUDCarriesTailsAndCoVFlag(t *testing.T) {
	results := []result.Result{
		// Both frameworks at the headline concurrency (100). gin-gorm is stable;
		// gombit's trials (12000, 9000) vary ~20% -> must be flagged.
		crudRow("gin-gorm", 100, 1, 25000, 1, 2, 3), crudRow("gin-gorm", 100, 2, 25200, 1, 2, 3),
		crudRow("gombit", 100, 1, 12000, 2, 5, 8), crudRow("gombit", 100, 2, 9000, 2, 5, 8),
		// A lower-concurrency row that must NOT be the one published.
		crudRow("gin-gorm", 10, 1, 18000, 1, 1, 1),
	}
	prints := []footprint.Footprint{
		{Framework: "gin-gorm", Variant: footprint.VariantContainer, ColdStart: footprint.ColdStart{MedianMs: 117},
			IdleRSSBytes: 8 << 20, LoadedRSSBytes: 19 << 20, CPUPercentUnderLoad: 140, ImageSizeBytes: 22 << 20},
		{Framework: "gombit", Variant: footprint.VariantEmbedded, BinarySizeBytes: 25 << 20}, // must not appear
	}
	dirty := false
	meta := metadata.Metadata{
		GitCommit: "abcdef1234567890", GitDirty: &dirty, Timestamp: "2026-08-27T00:00:00Z",
		OS: "linux", Arch: "amd64", CPUModel: "Test CPU", LogicalCPUs: 8, RAMBytes: 16 << 30,
		Kernel: "5.15-wsl", PostgresVersion: "postgres:16.4-alpine", ResourceLimits: "enforced",
		BenchmarkTool: "grafana/k6:0.55.0", Concurrency: []int{1, 10, 100}, Trials: 3,
		DurationSeconds: 10, WarmupSeconds: 3,
	}

	// All four rungs at the headline (valid-post) scenario, with samples so the
	// median is exercised; a wrong-scenario row must be excluded.
	micro := []microbench.Row{
		{Stack: "nethttp", Scenario: "valid-post", NsPerOp: []float64{820, 800}, BytesPerOp: 1425, AllocsPerOp: 14},
		{Stack: "gin", Scenario: "valid-post", NsPerOp: []float64{900}, BytesPerOp: 1458, AllocsPerOp: 15},
		{Stack: "huma", Scenario: "valid-post", NsPerOp: []float64{1078}, BytesPerOp: 1467, AllocsPerOp: 17},
		{Stack: "gombit", Scenario: "valid-post", NsPerOp: []float64{3040, 3160}, BytesPerOp: 4901, AllocsPerOp: 51},
		{Stack: "gin", Scenario: "json", NsPerOp: []float64{500}, BytesPerOp: 64, AllocsPerOp: 1}, // wrong scenario -> excluded
	}
	out := Render(results, prints, micro, meta)

	// Framework-tax table: validated-POST ladder, median ns/op, relative column.
	// net/http median (820,800)=810 baseline; gombit median (3040,3160)=3100 -> 3.8×.
	for _, want := range []string{
		"### Framework tax", "validated typed POST",
		"| stack | ns/op | B/op | allocs/op | vs net/http |",
		"| net/http | 810 | 1425 | 14 | 1.0× |",
		"| Gombit | 3100 | 4901 | 51 | 3.8× |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("framework-tax table missing %q\n%s", want, out)
		}
	}
	if strings.Index(out, "| net/http |") > strings.Index(out, "| Gombit |") {
		t.Errorf("framework-tax rows not in ladder order:\n%s", out)
	}
	if strings.Contains(out, "| 500 |") {
		t.Errorf("wrong-scenario (json) row leaked into the framework-tax table:\n%s", out)
	}
	// CRUD table: headline concurrency, req/s + tails, ⚠ on the noisy row only.
	for _, want := range []string{
		"At **100 concurrent clients**",
		"| framework | req/s | p50 ms | p95 ms | p99 ms |",
		"| gin-gorm | 25100 | 1.0 | 2.0 | 3.0 |",
		"| gombit | 10500 ⚠ | 2.0 | 5.0 | 8.0 |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("CRUD table missing %q\n%s", want, out)
		}
	}
	// The stable gin-gorm row must not be flagged.
	if line := lineWith(out, "| gin-gorm | 25100"); strings.Contains(line, "⚠") {
		t.Errorf("stable row should not be flagged: %q", line)
	}
	// Footprint table: container only, MB conversions, embedded excluded.
	if !strings.Contains(out, "| gin-gorm | 117 | 8.0 | 19.0 | 140 | 22.0 |") {
		t.Errorf("footprint row wrong:\n%s", out)
	}
	if strings.Contains(out, "embedded") {
		t.Errorf("embedded variant leaked into the report:\n%s", out)
	}
	// CPU caption is not a "lower is better" quality claim.
	if !strings.Contains(out, "not* a quality score") {
		t.Errorf("footprint caption should not call CPU 'lower is better':\n%s", out)
	}
	// Host, commit and toolchain are published per table (writeProvenance); the
	// shared block carries only what the whole snapshot has in common.
	for _, want := range []string{"Test CPU", "8 logical CPUs", "kernel 5.15-wsl", "abcdef123456"} {
		if !strings.Contains(out, want) {
			t.Errorf("table provenance caption missing %q\n%s", want, out)
		}
	}
	for _, want := range []string{
		"postgres:16.4-alpine", "methodology.md",
		// The actual protocol must be printed so a reduced run can't wear the canonical sweep.
		"concurrency 1/10/100 VUs, 3 trials × 10s each (warm-up 3s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("methodology missing %q\n%s", want, out)
		}
	}
}

func TestFrameworkTaxFlagsNoisyRung(t *testing.T) {
	// A non-stationary Gin series (the Huma<Gin inversion class) must be flagged.
	micro := []microbench.Row{
		{Stack: "nethttp", Scenario: "valid-post", NsPerOp: []float64{800, 810, 805}},
		{Stack: "gin", Scenario: "valid-post", NsPerOp: []float64{5538, 2589, 4479, 3210}}, // ~30% CoV
		{Stack: "huma", Scenario: "valid-post", NsPerOp: []float64{3450, 3460, 3455}},
		{Stack: "gombit", Scenario: "valid-post", NsPerOp: []float64{9500, 9550, 9520}},
	}
	out := Render(nil, nil, micro, metadata.Metadata{})
	gin := lineWith(out, "| Gin |")
	if !strings.Contains(gin, "⚠") {
		t.Errorf("noisy Gin rung should be flagged: %q", gin)
	}
	if !strings.Contains(out, "varied by more than 5%") {
		t.Errorf("noise caption missing:\n%s", out)
	}
	// A stable rung must not be flagged.
	if hu := lineWith(out, "| Huma + Gin |"); strings.Contains(hu, "⚠") {
		t.Errorf("stable Huma rung should not be flagged: %q", hu)
	}
}

func TestFrameworkTaxIncompleteLadderNotPublished(t *testing.T) {
	// Only three of the four rungs -> must render "Incomplete", not a holey table.
	micro := []microbench.Row{
		{Stack: "nethttp", Scenario: "valid-post", NsPerOp: []float64{800}},
		{Stack: "gin", Scenario: "valid-post", NsPerOp: []float64{900}},
		{Stack: "huma", Scenario: "valid-post", NsPerOp: []float64{1000}},
		// gombit missing
	}
	out := Render(nil, nil, micro, metadata.Metadata{})
	if !strings.Contains(out, "Incomplete") {
		t.Errorf("a missing rung should render Incomplete, not a partial ladder:\n%s", out)
	}
	if strings.Contains(out, "| stack | ns/op") {
		t.Errorf("no table should be published for an incomplete ladder:\n%s", out)
	}
}

func TestPickConcurrencyFallsBackToMax(t *testing.T) {
	// No headline (100) level -> the highest present (500) is published.
	results := []result.Result{
		crudRow("x", 10, 1, 100, 1, 1, 1),
		crudRow("x", 500, 1, 90, 2, 2, 2),
	}
	out := Render(results, nil, nil, metadata.Metadata{})
	if !strings.Contains(out, "At **500 concurrent clients**") {
		t.Errorf("expected fallback to the max concurrency:\n%s", out)
	}
}

func TestRenderEmptyDataIsHonest(t *testing.T) {
	out := Render(nil, nil, nil, metadata.Metadata{})
	for _, want := range []string{
		"### Framework tax", "run `make benchmark-micro`", "run `make benchmark-crud-all`", "run `make benchmark-footprint`",
		"metadata not yet recorded",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("empty render missing %q\n%s", want, out)
		}
	}
}

// canonicalMeta is a metadata value whose protocol equals CanonicalProtocol and
// whose tree is clean — the shape a publishable run has. Individual tests mutate
// a copy to trip one banner condition at a time.
func canonicalMeta() metadata.Metadata {
	clean := false
	return metadata.Metadata{
		GitCommit: "abcdef1234567890", GitDirty: &clean, CPUModel: "Test CPU",
		Concurrency:     append([]int(nil), CanonicalProtocol.Concurrency...),
		Trials:          CanonicalProtocol.Trials,
		DurationSeconds: CanonicalProtocol.DurationSeconds,
		WarmupSeconds:   CanonicalProtocol.WarmupSeconds,
	}
}

func TestDirtyTreeStampsUnpublishable(t *testing.T) {
	meta := canonicalMeta()
	dirty := true
	meta.GitDirty = &dirty
	out := Render(nil, nil, nil, meta)
	if !strings.Contains(out, "UNPUBLISHABLE DEVELOPMENT RUN") {
		t.Errorf("a dirty-tree run must be stamped unpublishable:\n%s", out)
	}
	if !strings.Contains(out, "not reproducible") {
		t.Errorf("the dirty banner must say the numbers aren't reproducible:\n%s", out)
	}
	// The remediation command must be targets that exist in this tree, not the
	// all-in-one `make benchmark` (a separate slice). A banner whose fix-it
	// command 404s is a broken caption.
	if !strings.Contains(out, "make benchmark-crud-all benchmark-footprint benchmark-micro benchmark-report") {
		t.Errorf("dirty banner must name the real rerun chain:\n%s", out)
	}
	if strings.Contains(out, "make benchmark`") { // bare all-in-one target, closed immediately
		t.Errorf("dirty banner references `make benchmark`, which is not a target on this branch:\n%s", out)
	}
}

func TestCanonicalCleanRunHasNoBanner(t *testing.T) {
	out := Render(nil, nil, nil, canonicalMeta())
	for _, unwanted := range []string{"UNPUBLISHABLE", "Reduced development snapshot"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("a clean canonical run must carry no %q banner:\n%s", unwanted, out)
		}
	}
}

func TestReducedProtocolIsLabelled(t *testing.T) {
	meta := canonicalMeta()
	meta.Concurrency = []int{1, 10, 100}
	meta.Trials = 3
	meta.DurationSeconds = 10
	meta.WarmupSeconds = 3
	out := Render(nil, nil, nil, meta)
	if !strings.Contains(out, "Reduced development snapshot") {
		t.Errorf("a narrower-than-canonical run must be labelled reduced:\n%s", out)
	}
	// The banner must name what differs (both endpoints), not just say "reduced".
	for _, want := range []string{"concurrency 1/10/100", "canonical 1/10/100/500/1000", "3 trials", "10s per trial", "3s warm-up"} {
		if !strings.Contains(out, want) {
			t.Errorf("reduced banner missing diff detail %q:\n%s", want, out)
		}
	}
	// It must point at the canonical protocol's real source, NOT the metadata
	// block below (which by design prints THIS reduced run's parameters).
	if !strings.Contains(out, "methodology.md") && !strings.Contains(out, "versions.env") {
		t.Errorf("reduced banner must cite the canonical protocol's source:\n%s", out)
	}
	if strings.Contains(out, "described under \"How these were measured\"") {
		t.Errorf("reduced banner must not send the reader to the block that prints the reduced params:\n%s", out)
	}
}

// The pre-run placeholder (no protocol recorded) must NOT be mislabelled as a
// reduced snapshot — there is nothing to compare against canonical yet.
func TestEmptyProtocolIsNotReduced(t *testing.T) {
	if reduced, _ := reducedFrom(metadata.Metadata{}); reduced {
		t.Error("empty metadata should not be classified as a reduced snapshot")
	}
	out := Render(nil, nil, nil, metadata.Metadata{})
	if strings.Contains(out, "Reduced development snapshot") {
		t.Errorf("empty render must not carry the reduced banner:\n%s", out)
	}
}

// Each table must be captioned with the commit and host that produced IT.
func TestEachTableCarriesItsOwnProvenance(t *testing.T) {
	meta := canonicalMeta()
	clean := false
	host := func(commit, ts, cpu, gov string) metadata.Provenance {
		return metadata.Provenance{
			GitCommit: commit, Timestamp: ts, GitDirty: &clean,
			CPUModel: cpu, LogicalCPUs: 8, GoVersion: gov, OS: "linux", Arch: "amd64", Kernel: "7.0",
		}
	}
	meta.Groups = map[string]map[string]metadata.Provenance{
		metadata.GroupMicrobench: {},
		metadata.GroupCRUD:       {"gombit": host("2222222222222222", "2026-08-27T21:57:34Z", "CRUD CPU", "go1.25.7")},
		metadata.GroupFootprint:  {"gombit:container": host("3333333333333333", "2026-08-28T09:00:00Z", "Footprint CPU", "go1.25.7")},
	}
	for _, s := range stackLadder {
		meta.Groups[metadata.GroupMicrobench][s.key] = host("1111111111111111", "2026-09-08T10:00:00Z", "Micro CPU", "go1.26.1")
	}

	out := Render(
		[]result.Result{crudRow("gombit", 100, 1, 1000, 5, 10, 20)},
		[]footprint.Footprint{{Framework: "gombit", Variant: footprint.VariantContainer}},
		taxLadder(),
		meta,
	)

	sections := []struct{ heading, commit, cpu string }{
		{"### Framework tax", "1111111111", "Micro CPU"},
		{"### PostgreSQL CRUD read", "2222222222", "CRUD CPU"},
		{"### Operational footprint", "3333333333", "Footprint CPU"},
	}
	for i, s := range sections {
		start := strings.Index(out, s.heading)
		if start < 0 {
			t.Fatalf("missing heading %q:\n%s", s.heading, out)
		}
		end := len(out)
		if i+1 < len(sections) {
			end = strings.Index(out, sections[i+1].heading)
		}
		section := out[start:end]
		if !strings.Contains(section, s.commit) || !strings.Contains(section, s.cpu) {
			t.Errorf("%s must be captioned with %s / %s:\n%s", s.heading, s.commit, s.cpu, section)
		}
		for _, other := range sections {
			if other.commit != s.commit && strings.Contains(section, other.commit) {
				t.Errorf("%s carries another group's commit %s:\n%s", s.heading, other.commit, section)
			}
		}
	}
	if strings.Contains(out, "**Commit / date:**") {
		t.Errorf("the snapshot-wide commit line must not return:\n%s", out)
	}
	// All units within each table agree, so each caption is a single claim.
	if strings.Contains(out, "not measured together") {
		t.Errorf("uniform units must collapse to one caption per table:\n%s", out)
	}
}

// One run measures its units in sequence: `make benchmark-micro` is four
// `go test` processes that finish minutes apart. Those minutes are not different
// source states, and flagging them would make the mixed-provenance warning fire
// on every ordinary run — worthless by the time a real subset refresh happens.
// Caught by the real snapshot, not by a fixture: the first version of this code
// compared whole Provenance values, timestamps included.
func TestSequentialUnitsOfOneRunAreStillOneCaption(t *testing.T) {
	clean := false
	meta := canonicalMeta()
	for i, s := range stackLadder {
		meta = metadata.StampUnit(meta, metadata.GroupMicrobench, s.key, metadata.Provenance{
			GitCommit: "2ee9f8b1513b", GitDirty: &clean, CPUModel: "Dev Host",
			LogicalCPUs: 12, GoVersion: "go1.26.1", OS: "linux", Arch: "amd64", Kernel: "7.1",
			Timestamp: []string{
				"2026-09-15T00:32:14Z", "2026-09-15T00:33:55Z",
				"2026-09-15T00:35:32Z", "2026-09-15T00:37:00Z",
			}[i],
		})
	}
	out := Render(nil, nil, taxLadder(), meta)

	if strings.Contains(out, "not measured together") {
		t.Errorf("units of one run, differing only in clock time, must stay one caption:\n%s", out)
	}
	// The span is reported rather than one unit's clock standing in for all four.
	if !strings.Contains(out, "2026-09-15T00:32:14Z to 2026-09-15T00:37:00Z") {
		t.Errorf("caption must state the measurement span:\n%s", out)
	}
	// A different commit in the same group still breaks comparability.
	meta = metadata.StampUnit(meta, metadata.GroupMicrobench, "gombit", metadata.Provenance{
		GitCommit: "ffff99998888", GitDirty: &clean, CPUModel: "Dev Host",
		LogicalCPUs: 12, GoVersion: "go1.26.1", OS: "linux", Arch: "amd64", Kernel: "7.1",
		Timestamp: "2026-09-15T00:37:00Z",
	})
	if out := Render(nil, nil, taxLadder(), meta); !strings.Contains(out, "not measured together") {
		t.Errorf("a differing commit must still break comparability:\n%s", out)
	}
}

// THE regression this round exists for, at the rendering layer. `APPS=gombit
// make benchmark-footprint` replaces one row and preserves five; the table must
// NOT then be captioned with the re-run's commit. Without per-unit provenance
// this test cannot even be written — which is why the previous round's tests
// only proved the representation agreed with itself.
func TestSubsetRefreshIsNotPublishedUnderATableWideCaption(t *testing.T) {
	clean := false
	commitA := metadata.Provenance{GitCommit: "aaaa11112222", Timestamp: "2026-09-01T00:00:00Z", GitDirty: &clean, CPUModel: "Bench Host"}
	commitB := metadata.Provenance{GitCommit: "bbbb33334444", Timestamp: "2026-09-08T00:00:00Z", GitDirty: &clean, CPUModel: "Dev Host"}

	apps := []string{"django", "gin-gorm", "gombit", "laravel", "nestjs", "rails"}
	meta := canonicalMeta()
	rows := make([]footprint.Footprint, 0, len(apps))
	for _, app := range apps {
		row := footprint.Footprint{Framework: app, Variant: footprint.VariantContainer}
		meta = metadata.StampUnit(meta, metadata.GroupFootprint, row.ProvenanceUnit(), commitA)
		rows = append(rows, row)
	}
	// Re-measure exactly one app at a later commit.
	meta = metadata.StampUnit(meta, metadata.GroupFootprint, "gombit:container", commitB)

	out := Render(nil, rows, nil, meta)
	section := out[strings.Index(out, "### Operational footprint"):]

	if !strings.Contains(section, "not measured together") {
		t.Errorf("a table whose rows come from different commits must say so:\n%s", section)
	}
	// Both commits must appear, attributed to the right apps — and neither may
	// stand alone as the table's caption.
	if !strings.Contains(section, "gombit:container at `bbbb33334444`") {
		t.Errorf("the re-measured app must be attributed to its own commit:\n%s", section)
	}
	for _, app := range []string{"django", "rails"} {
		if !strings.Contains(section, app+":container at `aaaa11112222`") {
			t.Errorf("untouched app %q must keep its original commit:\n%s", app, section)
		}
	}
	if strings.Contains(section, "_Measured at `bbbb33334444`") {
		t.Errorf("the subset run's commit must never caption the whole table:\n%s", section)
	}
}

// The variant is part of footprint's merge key, and scripts/footprint accepts
// -variant embedded today, so measuring the embedded binary must not relabel the
// container row the README publishes. Found by auditing merge keys against
// provenance units, which is the check the first round skipped.
func TestEmbeddedVariantCannotRelabelTheContainerRow(t *testing.T) {
	clean := false
	container := footprint.Footprint{Framework: "gombit", Variant: footprint.VariantContainer}
	embedded := footprint.Footprint{Framework: "gombit", Variant: footprint.VariantEmbedded}
	if container.ProvenanceUnit() == embedded.ProvenanceUnit() {
		t.Fatalf("the two variants must not share a provenance unit: %q", container.ProvenanceUnit())
	}

	meta := canonicalMeta()
	meta = metadata.StampUnit(meta, metadata.GroupFootprint, container.ProvenanceUnit(),
		metadata.Provenance{GitCommit: "aaaa11112222", Timestamp: "2026-09-01T00:00:00Z", GitDirty: &clean, CPUModel: "Bench Host"})
	// A later embedded measurement writes a different row and a different unit.
	meta = metadata.StampUnit(meta, metadata.GroupFootprint, embedded.ProvenanceUnit(),
		metadata.Provenance{GitCommit: "bbbb33334444", Timestamp: "2026-09-08T00:00:00Z", GitDirty: &clean, CPUModel: "Dev Host"})

	out := Render(nil, []footprint.Footprint{container, embedded}, nil, meta)
	section := out[strings.Index(out, "### Operational footprint"):]

	// The published table is container-only, so it keeps the container commit.
	if !strings.Contains(section, "_Measured at `aaaa11112222`") {
		t.Errorf("the container row must keep its own commit:\n%s", section)
	}
	if strings.Contains(section, "bbbb33334444") {
		t.Errorf("the embedded run must not caption the container table:\n%s", section)
	}
}

// The same rule, applied to CRUD — which the previous round covered with a prose
// warning instead of code.
func TestCrudSubsetRefreshAlsoRefusesATableWideCaption(t *testing.T) {
	clean := false
	meta := canonicalMeta()
	meta = metadata.StampUnit(meta, metadata.GroupCRUD, "rails",
		metadata.Provenance{GitCommit: "aaaa11112222", Timestamp: "2026-09-01T00:00:00Z", GitDirty: &clean})
	meta = metadata.StampUnit(meta, metadata.GroupCRUD, "gombit",
		metadata.Provenance{GitCommit: "bbbb33334444", Timestamp: "2026-09-08T00:00:00Z", GitDirty: &clean})

	out := Render([]result.Result{
		crudRow("rails", 100, 1, 900, 5, 10, 20),
		crudRow("gombit", 100, 1, 1000, 5, 10, 20),
	}, nil, nil, meta)
	section := out[strings.Index(out, "### PostgreSQL CRUD read"):strings.Index(out, "### Operational footprint")]

	if !strings.Contains(section, "not measured together") {
		t.Errorf("a CRUD table mixing commits must say so:\n%s", section)
	}
	if !strings.Contains(section, "rails at `aaaa11112222`") || !strings.Contains(section, "gombit at `bbbb33334444`") {
		t.Errorf("each app must be attributed to its own commit:\n%s", section)
	}
}

// A snapshot written before per-unit provenance has no entry, and its top-level
// block IS every unit's provenance — one run produced everything — so it must
// keep rendering exactly as it did, with one caption per table.
func TestLegacySnapshotFallsBackToTopLevelProvenance(t *testing.T) {
	meta := canonicalMeta()
	meta.Timestamp = "2026-08-27T21:57:34Z"
	meta.GoVersion = "go1.27.0"
	out := Render(
		[]result.Result{crudRow("gombit", 100, 1, 1000, 5, 10, 20)},
		[]footprint.Footprint{{Framework: "gombit", Variant: footprint.VariantContainer}},
		taxLadder(),
		meta,
	)
	if n := strings.Count(out, "abcdef123456"); n != 3 {
		t.Errorf("legacy snapshot must caption all three tables with its one commit, got %d:\n%s", n, out)
	}
	if strings.Contains(out, "not measured together") {
		t.Errorf("a legacy snapshot is one run; it must not be reported as mixed:\n%s", out)
	}
	if !strings.Contains(out, "go1.27.0") {
		t.Errorf("legacy caption must still name the toolchain:\n%s", out)
	}
}

// After `APPS=gombit make benchmark-crud-all` the top-level block names that one
// app's commit and host. Neither the five untouched CRUD rows nor a footprint
// table nobody re-measured may be captioned with it: unrecorded units are said
// to be unrecorded (issue #266).
func TestRewrittenTopLevelNeverCaptionsUnrecordedRows(t *testing.T) {
	clean := false
	meta := canonicalMeta()
	meta.GitCommit, meta.CPUModel = "bbbb33334444", "Subset Host"
	meta = metadata.StampUnit(meta, metadata.GroupCRUD, "gombit",
		metadata.Provenance{GitCommit: "bbbb33334444", Timestamp: "2026-09-08T00:00:00Z", GitDirty: &clean, CPUModel: "Subset Host"})

	out := Render(
		[]result.Result{crudRow("rails", 100, 1, 900, 5, 10, 20), crudRow("gombit", 100, 1, 1000, 5, 10, 20)},
		[]footprint.Footprint{{Framework: "rails", Variant: footprint.VariantContainer}},
		nil, meta,
	)
	crud := out[strings.Index(out, "### PostgreSQL CRUD read"):strings.Index(out, "### Operational footprint")]
	prints := out[strings.Index(out, "### Operational footprint"):strings.Index(out, "### How these were measured")]

	if !strings.Contains(crud, "not measured together") || !strings.Contains(crud, "rails at an unrecorded commit") {
		t.Errorf("the untouched CRUD row must be reported as unrecorded, not collapsed into the subset's caption:\n%s", crud)
	}
	if strings.Contains(prints, "bbbb33334444") || strings.Contains(prints, "Subset Host") {
		t.Errorf("a footprint table the subset run never touched carries its commit/host:\n%s", prints)
	}
	if !strings.Contains(prints, "_Measured at an unrecorded commit and host") {
		t.Errorf("the unrecorded footprint table must say so:\n%s", prints)
	}
}

// A table with no data has no provenance to state.
func TestPlaceholderTablesCarryNoProvenance(t *testing.T) {
	out := Render(nil, nil, nil, canonicalMeta())
	if strings.Contains(out, "_Measured at ") {
		t.Errorf("a snapshot with no data must not caption its placeholders:\n%s", out)
	}
}

// A snapshot that has unit provenance but no shared run parameters — what
// `make benchmark-micro` alone produces in a fresh OUT_DIR — must say so, not
// render a row of em-dashes whose "0 trials" reads as a measured fact.
func TestMethodologyIsHonestWhenOnlyUnitProvenanceExists(t *testing.T) {
	meta := metadata.Metadata{}
	for _, s := range stackLadder {
		meta = metadata.StampUnit(meta, metadata.GroupMicrobench, s.key,
			metadata.Provenance{GitCommit: "abc123def456", CPUModel: "Dev CPU", Timestamp: "2026-09-08T00:00:00Z"})
	}
	out := Render(nil, nil, taxLadder(), meta)

	if !strings.Contains(out, "_Run metadata not yet recorded._") {
		t.Errorf("a snapshot with no shared run parameters must say so:\n%s", out)
	}
	if strings.Contains(out, "0 trials") {
		t.Errorf("an unrecorded protocol must not render as a measured 0:\n%s", out)
	}
	if !strings.Contains(out, "abc123def456") {
		t.Errorf("the framework-tax caption must still name the units' commit:\n%s", out)
	}
}

// One unit measured against uncommitted source taints the whole block, and the
// remediation must name only the group that has to be re-run.
func TestDirtyUnitStampsUnpublishableAndNamesOnlyThatGroupsTarget(t *testing.T) {
	meta := canonicalMeta()
	dirty, clean := true, false
	meta = metadata.StampUnit(meta, metadata.GroupMicrobench, "gin", metadata.Provenance{GitDirty: &dirty})
	meta = metadata.StampUnit(meta, metadata.GroupCRUD, "rails", metadata.Provenance{GitDirty: &clean})

	// Both units must be published for their dirt to be judged.
	banner := bannerOf(Render([]result.Result{crudRow("rails", 100, 1, 900, 5, 10, 20)}, nil, taxLadder(), meta))
	if !strings.Contains(banner, "UNPUBLISHABLE DEVELOPMENT RUN") {
		t.Errorf("a unit measured on a dirty tree must stamp the block unpublishable:\n%s", banner)
	}
	if !strings.Contains(banner, "`make benchmark-micro benchmark-report`") {
		t.Errorf("remediation must name the dirty unit's group target:\n%s", banner)
	}
	if strings.Contains(banner, "benchmark-crud-all") {
		t.Errorf("remediation must not prescribe re-running the clean CRUD sweep:\n%s", banner)
	}
}

// microbench.json also holds rows no table publishes: `make
// benchmark-micro-ablation` writes the gombit-ablation ladder and stamps its own
// unit. A dirty ablation run must not brand the published framework-tax table
// unpublishable, nor send the reader to re-run a target that would not clear it.
func TestDirtyUnpublishedUnitDoesNotStampTheBlock(t *testing.T) {
	meta := canonicalMeta()
	dirty, clean := true, false
	for _, s := range stackLadder {
		meta = metadata.StampUnit(meta, metadata.GroupMicrobench, s.key, metadata.Provenance{GitCommit: "abc123def456", GitDirty: &clean})
	}
	meta = metadata.StampUnit(meta, metadata.GroupMicrobench, "gombit-ablation", metadata.Provenance{GitCommit: "abc123def456", GitDirty: &dirty})

	if banner := bannerOf(Render(nil, nil, taxLadder(), meta)); strings.Contains(banner, "UNPUBLISHABLE") {
		t.Errorf("a dirty unit no table publishes must not stamp the block:\n%s", banner)
	}
}

// When the dirt cannot be pinned to a unit — a dirty top-level record, or a
// snapshot with no groups at all — any group may be affected, so the banner
// falls back to the full chain rather than under-prescribing.
func TestDirtyTopLevelStillPrescribesTheWholeChain(t *testing.T) {
	dirty := true
	for name, meta := range map[string]metadata.Metadata{
		"pre-groups snapshot": func() metadata.Metadata {
			m := canonicalMeta()
			m.GitDirty = &dirty
			return m
		}(),
		"dirty top level beside a clean unit": func() metadata.Metadata {
			m := canonicalMeta()
			m.GitDirty = &dirty
			clean := false
			return metadata.StampUnit(m, metadata.GroupMicrobench, "gin", metadata.Provenance{GitDirty: &clean})
		}(),
	} {
		if banner := bannerOf(Render(nil, nil, nil, meta)); !strings.Contains(banner, rerunChain) {
			t.Errorf("%s: must fall back to the full rerun chain:\n%s", name, banner)
		}
	}
}

// bannerOf returns everything before the generated-by line — i.e. the status
// banners only, excluding the tables and their placeholders.
func bannerOf(out string) string {
	if i := strings.Index(out, "_Generated by"); i >= 0 {
		return out[:i]
	}
	return out
}

func TestResourceLimitsPerFrameworkRendered(t *testing.T) {
	meta := canonicalMeta()
	// A partial verdict on one app must survive next to another app's enforced —
	// the merge-preservation bug this whole field exists to fix.
	meta.ResourceLimitsByFramework = map[string]string{
		"gin-gorm": "enforced: cpu 2.00 vCPU",
		"rails":    "partial: memory unset",
	}
	meta.PostgresResourceLimits = "enforced: cpu 2.00 vCPU, memory 2 GiB"
	meta.ResourceLimits = "should-not-be-shown-when-map-present"
	out := Render(nil, nil, nil, meta)
	for _, want := range []string{
		"per app", "gin-gorm — enforced: cpu 2.00 vCPU", "rails — partial: memory unset",
		"Postgres container limits:** enforced: cpu 2.00 vCPU, memory 2 GiB",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("resource-limits rendering missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "should-not-be-shown") {
		t.Errorf("scalar resource_limits leaked when the per-app map was present:\n%s", out)
	}
}

func TestResourceLimitsUniformNamesTheApps(t *testing.T) {
	meta := canonicalMeta()
	v := "enforced: cpu 2.00 vCPU, memory 1 GiB"
	meta.ResourceLimitsByFramework = map[string]string{"gombit": v, "gin-gorm": v}
	out := Render(nil, nil, nil, meta)
	// Collapse to one line, but name WHICH frameworks (sorted), never "all apps"
	// — a uniform map is not proof the whole suite was measured.
	if !strings.Contains(out, "(gin-gorm, gombit):** "+v) {
		t.Errorf("identical verdicts should collapse to one line naming the apps:\n%s", out)
	}
	if strings.Contains(out, "all apps") {
		t.Errorf("a uniform (possibly-subset) map must not claim whole-suite coverage:\n%s", out)
	}
	if strings.Contains(out, "per app") {
		t.Errorf("uniform verdicts should not render the per-app list form:\n%s", out)
	}
}

// The regression this PR could introduce: run-crud always writes a per-framework
// entry, so a standalone / APPS=<subset> run yields a 1-entry (uniform) map. It
// must name that one framework, never say "all apps".
func TestResourceLimitsSingleAppDoesNotClaimAllApps(t *testing.T) {
	meta := canonicalMeta()
	meta.ResourceLimitsByFramework = map[string]string{"gin-gorm": "not applied (standalone)"}
	out := Render(nil, nil, nil, meta)
	if !strings.Contains(out, "(gin-gorm):** not applied (standalone)") {
		t.Errorf("a single-app map should name that app:\n%s", out)
	}
	if strings.Contains(out, "all apps") {
		t.Errorf("a single-app run must not claim whole-suite coverage:\n%s", out)
	}
}

func TestReplaceBlockAndInSync(t *testing.T) {
	readme := "# App\n\n## Performance\n\n" + StartMarker + "\nOLD CONTENT\n" + EndMarker + "\n\n## Next\n"
	block := "NEW CONTENT\n"

	updated, err := ReplaceBlock(readme, block)
	if err != nil {
		t.Fatalf("ReplaceBlock: %v", err)
	}
	if !strings.Contains(updated, "NEW CONTENT") || strings.Contains(updated, "OLD CONTENT") {
		t.Errorf("block not replaced:\n%s", updated)
	}
	if !strings.Contains(updated, "## Performance") || !strings.Contains(updated, "## Next") {
		t.Errorf("content outside markers lost:\n%s", updated)
	}
	// InSync: true means "matches / no drift".
	sync, err := InSync(updated, block)
	if err != nil {
		t.Fatalf("InSync: %v", err)
	}
	if !sync {
		t.Error("re-rendering the same block should be in sync")
	}
	if sync, _ := InSync(readme, block); sync {
		t.Error("stale README should not be in sync")
	}
}

func TestReplaceBlockMissingMarkers(t *testing.T) {
	if _, err := ReplaceBlock("no markers here", "x"); err == nil {
		t.Error("missing markers should error")
	}
}

// CanonicalProtocol drives the "reduced snapshot" label, so it must equal the
// real source of truth (benchmarks/config/versions.env). This test parses the
// env file and fails if the two drift apart — so tightening the canonical sweep
// in versions.env forces the constant (and the label logic) to be updated with
// it, rather than silently comparing every future run against a stale target.
func TestCanonicalProtocolMatchesVersionsEnv(t *testing.T) {
	env := parseEnvFile(t, filepath.Join("..", "..", "config", "versions.env"))

	conc, err := parseIntCSV(env["CONCURRENCY"])
	if err != nil {
		t.Fatalf("CONCURRENCY %q: %v", env["CONCURRENCY"], err)
	}
	if !equalInts(conc, CanonicalProtocol.Concurrency) {
		t.Errorf("CanonicalProtocol.Concurrency %v != versions.env CONCURRENCY %v", CanonicalProtocol.Concurrency, conc)
	}
	assertIntPin(t, "TRIALS", env["TRIALS"], CanonicalProtocol.Trials)
	assertFloatPin(t, "DURATION_SECONDS", env["DURATION_SECONDS"], CanonicalProtocol.DurationSeconds)
	assertFloatPin(t, "WARMUP_SECONDS", env["WARMUP_SECONDS"], CanonicalProtocol.WarmupSeconds)
}

func assertIntPin(t *testing.T, key, raw string, want int) {
	t.Helper()
	got, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		t.Fatalf("%s %q: %v", key, raw, err)
	}
	if got != want {
		t.Errorf("CanonicalProtocol %s = %d, versions.env = %d", key, want, got)
	}
}

func assertFloatPin(t *testing.T, key, raw string, want float64) {
	t.Helper()
	got, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		t.Fatalf("%s %q: %v", key, raw, err)
	}
	if got != want {
		t.Errorf("CanonicalProtocol %s = %v, versions.env = %v", key, want, got)
	}
}

func parseEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // fixed in-repo config path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			env[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return env
}

func parseIntCSV(s string) ([]int, error) {
	var out []int
	for _, tok := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(tok))
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func lineWith(s, sub string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}
