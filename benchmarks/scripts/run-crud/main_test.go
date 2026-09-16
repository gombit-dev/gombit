package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/benchmarks/internal/metadata"
	"github.com/gombit-dev/gombit/benchmarks/internal/result"
)

func TestParseIntListRejectsInvalidToken(t *testing.T) {
	for _, in := range []string{"1,10,abc,100", "1,,10", "100o"} {
		if got, err := parseIntList(in); err == nil {
			t.Errorf("parseIntList(%q) = %v, nil; want an error", in, got)
		}
	}
}

// A run replaces exactly the rows it produced: the merge key is (framework,
// benchmark). Keyed on the framework alone, re-running crud-list silently
// deleted that app's rows for every other workload (#361).
func TestMergeRowsReplacesOnlyTheSameFrameworkAndBenchmark(t *testing.T) {
	existing := []result.Result{
		{Framework: "gombit", Benchmark: "crud-list", Concurrency: 10, Trial: 1},          // replaced
		{Framework: "gombit", Benchmark: "crud-list", Concurrency: 100, Trial: 1},         // replaced
		{Framework: "gombit", Benchmark: "auth-jwt", Concurrency: 10, Trial: 1},           // same app, other workload: kept
		{Framework: "gin-gorm", Benchmark: "crud-list", Concurrency: 10, Trial: 1},        // other app, same workload: kept
		{Framework: "gin-gorm", Benchmark: "techempower-json", Concurrency: 10, Trial: 1}, // other app, other workload: kept
	}
	newRows := []result.Result{
		{Framework: "gombit", Benchmark: "crud-list", Concurrency: 10, Trial: 1, Requests: 42},
	}

	merged := mergeRows(existing, newRows, "gombit", "crud-list")

	count := map[string]int{}
	for _, r := range merged {
		count[r.ProvenanceUnit()]++
		if r.ProvenanceUnit() == "gombit:crud-list" && r.Requests != 42 {
			t.Errorf("gombit crud-list row not the new one: %+v", r)
		}
	}
	for unit, want := range map[string]int{
		"gombit:crud-list":          1,
		"gombit:auth-jwt":           1,
		"gin-gorm:crud-list":        1,
		"gin-gorm:techempower-json": 1,
	} {
		if count[unit] != want {
			t.Errorf("%s rows = %d, want %d (merged: %+v)", unit, count[unit], want, merged)
		}
	}
}

func TestBenchmarkNameComesFromTheWorkloadScript(t *testing.T) {
	for in, want := range map[string]string{
		"benchmarks/workloads/crud-list.js": "crud-list",
		"/abs/workloads/auth-jwt.js":        "auth-jwt",
		"techempower-json.js":               "techempower-json",
	} {
		if got, err := benchmarkName(in); err != nil || got != want {
			t.Errorf("benchmarkName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", ".js", "/", "workloads/a:b.js"} {
		if got, err := benchmarkName(in); err == nil {
			t.Errorf("benchmarkName(%q) = %q, nil; want an error", in, got)
		}
	}
}

// okK6 is an injected k6 whose warm-up is a no-op and whose measured runs write
// the clean k6 golden.
func okK6(t *testing.T) k6Runner {
	t.Helper()
	ok, err := os.ReadFile(filepath.Join("..", "..", "internal", "k6", "testdata", "summary_ok.json")) //nolint:gosec // fixed testdata golden path
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	return func(_ int, _ string, summaryPath string) error {
		if summaryPath == "" {
			return nil
		}
		return os.WriteFile(summaryPath, ok, 0o600) //nolint:gosec // summaryPath is under t.TempDir()
	}
}

func readResultsJSON(t *testing.T, path string) []result.Result {
	t.Helper()
	rows, err := readResults(path)
	if err != nil {
		t.Fatalf("read results: %v", err)
	}
	return rows
}

// The issue's reproduction, through the real run(): a snapshot holding two
// workloads for one app plus another app's row. Re-running one workload must
// replace only its own rows, label them with the workload that ran, and leave
// the other workload's rows AND recorded provenance untouched (#361).
func TestRunReplacesOnlyItsOwnWorkload(t *testing.T) {
	dir := t.TempDir()
	clean := false
	seed := []result.Result{
		{Framework: "gombit", Benchmark: "crud-list", Concurrency: 10, Trial: 1, Requests: 1},
		{Framework: "gombit", Benchmark: "auth-jwt", Concurrency: 10, Trial: 1, Requests: 2},
		{Framework: "gin-gorm", Benchmark: "techempower-json", Concurrency: 10, Trial: 1, Requests: 3},
	}
	f, err := os.Create(filepath.Join(dir, "results.json")) //nolint:gosec // dir is t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := result.WriteJSON(f, seed); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	authProv := metadata.Provenance{GitCommit: "aaaa11112222", Timestamp: "2026-09-01T00:00:00Z", GitDirty: &clean, CPUModel: "Bench Host"}
	before := metadata.StampUnit(metadata.Metadata{}, metadata.GroupCRUD, "gombit:auth-jwt", authProv)
	writeMetadataJSON(t, filepath.Join(dir, "metadata.json"), before)

	cfg := runConfig{
		targetURL: "http://unused", framework: "gombit", benchmark: "crud-list",
		concurrency: []int{10}, duration: "1s", warmup: "1s", trials: 1, outDir: dir,
	}
	// Re-run crud-list; a second, non-default workload below proves the row
	// label follows the workload that ran.
	if err := run(cfg, okK6(t)); err != nil {
		t.Fatalf("run(crud-list): %v", err)
	}

	rows := readResultsJSON(t, filepath.Join(dir, "results.json"))
	byUnit := map[string][]result.Result{}
	for _, r := range rows {
		byUnit[r.ProvenanceUnit()] = append(byUnit[r.ProvenanceUnit()], r)
	}
	if got := byUnit["gombit:auth-jwt"]; len(got) != 1 || got[0].Requests != 2 {
		t.Errorf("re-running crud-list must keep gombit's auth-jwt row: %+v", rows)
	}
	if got := byUnit["gin-gorm:techempower-json"]; len(got) != 1 || got[0].Requests != 3 {
		t.Errorf("another app's row must be kept: %+v", rows)
	}
	if got := byUnit["gombit:crud-list"]; len(got) != 1 || got[0].Requests == 1 {
		t.Errorf("gombit's crud-list row must be replaced by the new measurement: %+v", rows)
	}

	after := readMetadataJSON(t, filepath.Join(dir, "metadata.json"))
	if after.UnitProvenance(metadata.GroupCRUD, "gombit:crud-list").Empty() {
		t.Error("the measured workload must record its own provenance")
	}
	if now := after.UnitProvenance(metadata.GroupCRUD, "gombit:auth-jwt"); !sameProvenance(authProv, now) {
		t.Errorf("recording crud-list relabelled auth-jwt's provenance: %+v -> %+v", authProv, now)
	}
	if _, ok := after.Groups[metadata.GroupCRUD]["gombit"]; ok {
		t.Error("provenance must never be filed under the bare framework again")
	}

	cfg.benchmark = "techempower-json"
	if err := run(cfg, okK6(t)); err != nil {
		t.Fatalf("run(techempower-json): %v", err)
	}
	rows = readResultsJSON(t, filepath.Join(dir, "results.json"))
	var labelled int
	for _, r := range rows {
		if r.Framework == "gombit" && r.Benchmark == "techempower-json" {
			labelled++
		}
	}
	if labelled != 1 || len(rows) != 4 {
		t.Errorf("a non-default workload's rows must carry its own benchmark and add to the snapshot: %+v", rows)
	}
}

// Without a benchmark a row can be neither merged nor attributed, so run() must
// refuse before writing anything.
func TestRunWithoutBenchmarkFailsAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	cfg := runConfig{
		targetURL: "http://unused", framework: "gombit",
		concurrency: []int{10}, duration: "1s", warmup: "1s", trials: 1, outDir: dir,
	}
	if err := run(cfg, okK6(t)); err == nil {
		t.Fatal("run() = nil, want an error for an empty benchmark")
	}
	for _, name := range []string{"results.json", "metadata.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s was written despite the missing benchmark (stat err: %v)", name, err)
		}
	}
}

// mergedMetadata must treat the postgres verdict's two "empty-ish" states
// differently, reading through the on-disk snapshot (not just the in-memory
// value): an empty string means "this run did not re-verify" and keeps whatever
// the prior snapshot claimed, while an explicit "unknown …" from a run that
// looked and could not classify OVERWRITES — so a stale enforced/partial can
// never stick across a re-run whose check failed.
func TestMergedMetadataPostgresSentinelDistinguishesNotProvidedFromVerifiedUnknown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.json")
	prior := "enforced: cpu 2.00 vCPU, memory 2 GiB"
	writeMetadataJSON(t, path, metadata.Metadata{PostgresResourceLimits: prior})

	// Empty ("not provided", e.g. standalone benchmark-crud) keeps the prior verdict.
	got, err := mergedMetadata(path, metadata.Metadata{PostgresResourceLimits: ""})
	if err != nil {
		t.Fatalf("mergedMetadata: %v", err)
	}
	if got.PostgresResourceLimits != prior {
		t.Errorf(`empty postgres verdict should keep the prior one; got %q, want %q`, got.PostgresResourceLimits, prior)
	}

	// A verified-unknown re-run overwrites the stale verdict (does not inherit it).
	unknown := "unknown (inspect-limits failed)"
	if got, err = mergedMetadata(path, metadata.Metadata{PostgresResourceLimits: unknown}); err != nil {
		t.Fatalf("mergedMetadata: %v", err)
	}
	if got.PostgresResourceLimits != unknown {
		t.Errorf("verified-unknown should overwrite the stale verdict; got %q, want %q", got.PostgresResourceLimits, unknown)
	}

	// A fresh real verdict overwrites too (the ordinary re-verify case).
	fresh := "partial: memory unset"
	if got, err = mergedMetadata(path, metadata.Metadata{PostgresResourceLimits: fresh}); err != nil {
		t.Fatalf("mergedMetadata: %v", err)
	}
	if got.PostgresResourceLimits != fresh {
		t.Errorf("a fresh verdict should overwrite; got %q, want %q", got.PostgresResourceLimits, fresh)
	}
}

// A corrupt metadata.json must fail the run with results.json untouched. It used
// to be treated as "no prior snapshot" and overwritten with this one app's
// record, erasing every other app's versions and limit verdicts and every unit's
// provenance, the microbench and footprint groups included.
func TestRunFailsWithoutWritingWhenMetadataIsCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err := os.ReadFile(filepath.Join("..", "..", "internal", "k6", "testdata", "summary_ok.json")) //nolint:gosec // fixed testdata golden path
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	k6run := func(_ int, _ string, summaryPath string) error {
		if summaryPath == "" {
			return nil
		}
		return os.WriteFile(summaryPath, ok, 0o600) //nolint:gosec // summaryPath is under t.TempDir()
	}
	cfg := runConfig{
		targetURL: "http://unused", framework: "gombit", benchmark: "crud-list",
		concurrency: []int{10}, duration: "1s", warmup: "1s", trials: 1, outDir: dir,
	}
	if err := run(cfg, k6run); err == nil {
		t.Fatal("run() = nil, want an error for a corrupt metadata.json")
	}
	if _, err := os.Stat(filepath.Join(dir, "results.json")); !os.IsNotExist(err) {
		t.Errorf("results.json was written despite the metadata failure (stat err: %v)", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "metadata.json")); string(data) != "{not json" { //nolint:gosec // test-owned temp path
		t.Errorf("the corrupt metadata.json was overwritten: %q", data)
	}
}

// `APPS=gombit make benchmark-crud-all` is one run-crud invocation into an
// existing snapshot, and it rewrites the whole top-level block. That rewrite must
// not re-attribute a single row it did not measure — not a sibling CRUD app, and
// not a footprint or microbench row in another group. The only move allowed for
// another unit is from recorded to unrecorded (a legacy snapshot losing its
// one-run fallback), never to a different recorded provenance.
//
// It starts from the committed snapshot and from the shapes that snapshot has
// had, because every earlier test pre-stamped all units first and so never
// exercised the fallback the real file relied on (issue #266).
func TestSubsetRunNeverReattributesRowsItDidNotMeasure(t *testing.T) {
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

	ok, err := os.ReadFile(filepath.Join("..", "..", "internal", "k6", "testdata", "summary_ok.json")) //nolint:gosec // fixed testdata golden path
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	k6run := func(_ int, _ string, summaryPath string) error {
		if summaryPath == "" {
			return nil
		}
		return os.WriteFile(summaryPath, ok, 0o600) //nolint:gosec // summaryPath is under t.TempDir()
	}

	for name, before := range map[string]metadata.Metadata{
		"committed snapshot":        snapshot,
		"microbench units only":     microOnly,
		"legacy snapshot, no units": legacy,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeMetadataJSON(t, filepath.Join(dir, "metadata.json"), before)
			cfg := runConfig{
				targetURL: "http://unused", framework: "gombit", benchmark: "crud-list", frameworkVersion: "vB",
				concurrency: []int{10}, duration: "1s", warmup: "1s", trials: 1, outDir: dir,
			}
			if err := run(cfg, k6run); err != nil {
				t.Fatalf("run: %v", err)
			}
			after := readMetadataJSON(t, filepath.Join(dir, "metadata.json"))

			if after.UnitProvenance(metadata.GroupCRUD, "gombit:crud-list").Empty() {
				t.Error("the measured app must record its own provenance")
			}
			for _, u := range allUnits() {
				if u.group == metadata.GroupCRUD && u.unit == "gombit:crud-list" {
					continue
				}
				was := before.UnitProvenance(u.group, u.unit)
				now := after.UnitProvenance(u.group, u.unit)
				if now.Empty() || sameProvenance(was, now) {
					continue
				}
				t.Errorf("%s/%s was re-attributed by a run that did not measure it: %s at %s -> %s at %s",
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

func readMetadataJSON(t *testing.T, path string) metadata.Metadata {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var m metadata.Metadata
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse metadata: %v", err)
	}
	return m
}

func writeMetadataJSON(t *testing.T, path string, meta metadata.Metadata) {
	t.Helper()
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
}

// A trial that fails Validate (here the real all-failed k6 golden) must make
// run() return an error AND leave no results.json — a failed implementation
// must not write a partial or bogus snapshot (issue #141 §10).
func TestRunFailsAndWritesNothingOnValidateFailure(t *testing.T) {
	dir := t.TempDir()
	allFailed, err := os.ReadFile(filepath.Join("..", "..", "internal", "k6", "testdata", "summary_all_failed.json")) //nolint:gosec // fixed testdata golden path
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}

	// Injected k6: warm-up (empty summaryPath) is a no-op; the measured run
	// writes the all-failed golden.
	k6run := func(_ int, _ string, summaryPath string) error {
		if summaryPath == "" {
			return nil
		}
		return os.WriteFile(summaryPath, allFailed, 0o600) //nolint:gosec // summaryPath is under t.TempDir()
	}

	cfg := runConfig{
		targetURL: "http://unused", framework: "x", benchmark: "crud-list",
		concurrency: []int{1}, duration: "1s", warmup: "1s", trials: 1,
		outDir: dir,
	}

	if err := run(cfg, k6run); err == nil {
		t.Fatal("run() = nil, want an error for an all-failed trial")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "results.json")); !os.IsNotExist(statErr) {
		t.Errorf("results.json exists after a failed run; want nothing written (stat err: %v)", statErr)
	}
}

// A clean injected run writes the snapshot, and a second framework's run merges
// rather than truncating.
func TestRunWritesAndAccumulatesAcrossFrameworks(t *testing.T) {
	dir := t.TempDir()
	ok, err := os.ReadFile(filepath.Join("..", "..", "internal", "k6", "testdata", "summary_ok.json")) //nolint:gosec // fixed testdata golden path
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	k6run := func(_ int, _ string, summaryPath string) error {
		if summaryPath == "" {
			return nil
		}
		return os.WriteFile(summaryPath, ok, 0o600) //nolint:gosec // summaryPath is under t.TempDir()
	}

	for _, fw := range []string{"gin-gorm", "gombit"} {
		cfg := runConfig{
			targetURL: "http://unused", framework: fw, benchmark: "crud-list", frameworkVersion: "v" + fw,
			concurrency: []int{10}, duration: "1s", warmup: "1s", trials: 1,
			outDir: dir, k6Image: "grafana/k6:0.55.0",
			// Distinct per-app verdicts to prove the merge preserves both,
			// plus a shared postgres verdict.
			resourceLimits: "limit-" + fw, postgresResourceLimits: "pg-enforced",
		}
		if err := run(cfg, k6run); err != nil {
			t.Fatalf("run(%s) = %v", fw, err)
		}
	}

	f, err := os.Open(filepath.Join(dir, "results.json")) //nolint:gosec // dir is t.TempDir()
	if err != nil {
		t.Fatalf("open results.json: %v", err)
	}
	defer func() { _ = f.Close() }()
	rows, err := result.ReadJSON(f)
	if err != nil {
		t.Fatalf("ReadJSON: %v", err)
	}
	frameworks := map[string]bool{}
	for _, r := range rows {
		frameworks[r.Framework] = true
	}
	if !frameworks["gin-gorm"] || !frameworks["gombit"] {
		t.Errorf("second run truncated the first: frameworks = %v", frameworks)
	}

	// metadata records the ACTUAL k6 image that ran (not a bare "k6"), and the
	// version maps accumulate both frameworks across the two runs.
	meta, err := os.ReadFile(filepath.Join(dir, "metadata.json")) //nolint:gosec // dir is t.TempDir()
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}
	s := string(meta)
	if !strings.Contains(s, `"benchmark_tool": "grafana/k6:0.55.0"`) {
		t.Errorf("metadata benchmark_tool is not the k6 image that ran:\n%s", s)
	}
	if !strings.Contains(s, `"gin-gorm": "vgin-gorm"`) || !strings.Contains(s, `"gombit": "vgombit"`) {
		t.Errorf("metadata framework_versions did not accumulate both runs:\n%s", s)
	}

	// The bug this guards: resource_limits_by_framework must preserve EVERY
	// app's applied-limit verdict across the merge, not just the last writer's.
	// The scalar resource_limits is allowed to be last-write, but the per-app
	// map is authoritative and must carry both.
	if !strings.Contains(s, `"gin-gorm": "limit-gin-gorm"`) || !strings.Contains(s, `"gombit": "limit-gombit"`) {
		t.Errorf("resource_limits_by_framework did not preserve both apps' verdicts:\n%s", s)
	}
	if !strings.Contains(s, `"postgres_resource_limits": "pg-enforced"`) {
		t.Errorf("postgres_resource_limits was not recorded/preserved:\n%s", s)
	}
}
