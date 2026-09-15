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

func TestMergeRowsReplacesSameFrameworkKeepsOthers(t *testing.T) {
	existing := []result.Result{
		{Framework: "gin-gorm", Concurrency: 10, Trial: 1},
		{Framework: "gombit", Concurrency: 10, Trial: 1},    // other framework, kept
		{Framework: "gin-gorm", Concurrency: 100, Trial: 1}, // old gin-gorm rows, replaced
	}
	newRows := []result.Result{
		{Framework: "gin-gorm", Concurrency: 10, Trial: 1, Requests: 42},
	}

	merged := mergeRows(existing, newRows, "gin-gorm")

	var ginGorm, gombit int
	for _, r := range merged {
		switch r.Framework {
		case "gin-gorm":
			ginGorm++
			if r.Requests != 42 {
				t.Errorf("gin-gorm row not the new one: %+v", r)
			}
		case "gombit":
			gombit++
		}
	}
	if ginGorm != 1 {
		t.Errorf("gin-gorm rows = %d, want 1 (old ones replaced)", ginGorm)
	}
	if gombit != 1 {
		t.Errorf("gombit rows = %d, want 1 (other framework kept)", gombit)
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
		targetURL: "http://unused", framework: "gombit",
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
				targetURL: "http://unused", framework: "gombit", frameworkVersion: "vB",
				concurrency: []int{10}, duration: "1s", warmup: "1s", trials: 1, outDir: dir,
			}
			if err := run(cfg, k6run); err != nil {
				t.Fatalf("run: %v", err)
			}
			after := readMetadataJSON(t, filepath.Join(dir, "metadata.json"))

			if after.UnitProvenance(metadata.GroupCRUD, "gombit").Empty() {
				t.Error("the measured app must record its own provenance")
			}
			for _, u := range allUnits() {
				if u.group == metadata.GroupCRUD && u.unit == "gombit" {
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
		units = append(units, unitRef{metadata.GroupCRUD, fw}, unitRef{metadata.GroupFootprint, fw + ":container"})
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
		targetURL: "http://unused", framework: "x",
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
			targetURL: "http://unused", framework: fw, frameworkVersion: "v" + fw,
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
