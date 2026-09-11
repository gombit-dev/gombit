package metadata

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCollectUsesInjectedRunnerAndClock(t *testing.T) {
	fixedTime := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	run := func(_ context.Context, name string, args ...string) (string, error) {
		switch {
		case name == "git" && len(args) > 0 && args[0] == "rev-parse":
			return "abc123def456", nil
		case name == "git" && len(args) > 0 && args[0] == "status":
			return " M some/file.go", nil // non-empty -> dirty
		case name == "uname":
			return "6.1.0-test", nil
		case name == "docker" && args[0] == "version":
			return "27.0.1", nil
		case name == "docker" && args[0] == "compose":
			return "v2.29.0", nil
		default:
			return "", nil
		}
	}

	m := Collect(context.Background(), Options{
		Now:             func() time.Time { return fixedTime },
		Run:             run,
		PostgresVersion: "16.4",
		BenchmarkTool:   "k6 0.55.0",
		ResourceLimits:  "app 1cpu/512m; pg 2cpu/2g",
		DurationSeconds: 30,
		WarmupSeconds:   10,
		Concurrency:     []int{1, 10, 100},
		Trials:          5,
	})

	if m.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", m.SchemaVersion, SchemaVersion)
	}
	if m.Timestamp != "2026-08-26T12:00:00Z" {
		t.Errorf("Timestamp = %q, want the injected fixed time", m.Timestamp)
	}
	if m.GitCommit != "abc123def456" {
		t.Errorf("GitCommit = %q", m.GitCommit)
	}
	if m.GitDirty == nil || !*m.GitDirty {
		t.Errorf("GitDirty = %v, want a non-nil true (git status was non-empty)", m.GitDirty)
	}
	if m.Kernel != "6.1.0-test" {
		t.Errorf("Kernel = %q", m.Kernel)
	}
	if m.DockerVersion != "27.0.1" || m.DockerComposeVersion != "v2.29.0" {
		t.Errorf("docker versions = %q / %q", m.DockerVersion, m.DockerComposeVersion)
	}
	// Deterministic host fields come straight from the Go runtime.
	if m.OS != runtime.GOOS || m.Arch != runtime.GOARCH || m.LogicalCPUs != runtime.NumCPU() {
		t.Errorf("runtime fields = %q/%q/%d", m.OS, m.Arch, m.LogicalCPUs)
	}
	if m.GoVersion != runtime.Version() {
		t.Errorf("GoVersion = %q, want %q", m.GoVersion, runtime.Version())
	}
	// Provided run-parameter fields are copied through verbatim.
	if m.PostgresVersion != "16.4" || m.BenchmarkTool != "k6 0.55.0" || m.Trials != 5 {
		t.Errorf("provided fields not copied: %+v", m)
	}
	if len(m.Concurrency) != 3 || m.Concurrency[2] != 100 {
		t.Errorf("Concurrency = %v", m.Concurrency)
	}
}

func TestCollectCleanTreeAndMissingToolsDegrade(t *testing.T) {
	// A runner that returns "" for git status (clean) and errors for docker.
	run := func(_ context.Context, name string, args ...string) (string, error) {
		if name == "git" && args[0] == "status" {
			return "", nil
		}
		if name == "docker" {
			return "", context.DeadlineExceeded // tool "unavailable"
		}
		return "ok", nil
	}
	m := Collect(context.Background(), Options{Run: run})
	if m.GitDirty == nil || *m.GitDirty {
		t.Errorf("GitDirty = %v, want a non-nil false for an empty git status", m.GitDirty)
	}
	// Missing docker must leave the field empty, not fail collection.
	if m.DockerVersion != "" || m.DockerComposeVersion != "" {
		t.Errorf("docker versions should be empty when unavailable: %q/%q", m.DockerVersion, m.DockerComposeVersion)
	}
}

// git_dirty must reflect the SOURCE tree, excluding benchmarks/results/ — a
// suite writes result files for earlier apps before Collect runs for a later
// one, and that must not flag an otherwise-clean tree dirty.
func TestCollectExcludesResultsFromDirty(t *testing.T) {
	var statusArgs []string
	run := func(_ context.Context, name string, args ...string) (string, error) {
		if name == "git" && len(args) > 0 && args[0] == "status" {
			statusArgs = args
			// A runner honoring the exclude pathspec would report clean here;
			// simulate that (only benchmarks/results changed).
			return "", nil
		}
		return "", nil
	}
	m := Collect(context.Background(), Options{Run: run, Now: time.Now})
	if m.GitDirty == nil || *m.GitDirty {
		t.Errorf("GitDirty = %v, want non-nil false (results-only changes are not source dirt)", m.GitDirty)
	}
	joined := strings.Join(statusArgs, " ")
	if !strings.Contains(joined, "benchmarks/results") || !strings.Contains(joined, "exclude") {
		t.Errorf("git status must exclude benchmarks/results, got args: %v", statusArgs)
	}
}

// A failed `git status` (or missing git) must leave GitDirty unknown (nil),
// never fail-open to a clean-tree claim — even when rev-parse succeeded and a
// SHA was recorded.
func TestCollectGitStatusErrorIsUnknownNotClean(t *testing.T) {
	run := func(_ context.Context, name string, args ...string) (string, error) {
		if name == "git" && args[0] == "rev-parse" {
			return "deadbeef", nil
		}
		if name == "git" && args[0] == "status" {
			return "fatal: not a git repository", context.Canceled // status failed
		}
		return "", nil
	}
	m := Collect(context.Background(), Options{Run: run})
	if m.GitCommit != "deadbeef" {
		t.Errorf("GitCommit = %q, want the SHA rev-parse returned", m.GitCommit)
	}
	if m.GitDirty != nil {
		t.Errorf("GitDirty = %v, want nil (unknown) when git status failed", *m.GitDirty)
	}
}

// A collection that names its measurement group must file the very same
// commit/host/toolchain facts under that group, so a group-aware reader and a
// flat-shape reader never disagree about what ran.
func TestCollectFilesProvenanceUnderItsGroup(t *testing.T) {
	run := func(_ context.Context, name string, args ...string) (string, error) {
		if name == "git" && args[0] == "rev-parse" {
			return "cafebabe0001", nil
		}
		return "", nil
	}
	m := Collect(context.Background(), Options{Run: run, Group: GroupMicrobench})

	prov, ok := m.Groups[GroupMicrobench]
	if !ok {
		t.Fatalf("Groups is missing the %q entry: %+v", GroupMicrobench, m.Groups)
	}
	if prov != m.Provenance() {
		t.Errorf("group provenance %+v differs from the top-level block %+v", prov, m.Provenance())
	}
	if prov.GoVersion != runtime.Version() {
		t.Errorf("group GoVersion = %q, want %q", prov.GoVersion, runtime.Version())
	}
	if len(m.Groups) != 1 {
		t.Errorf("a single-group collection must file exactly one entry, got %v", m.Groups)
	}
}

// The whole-snapshot path (`make benchmark-metadata`) attributes its collection
// to no group. Groups must still serialize as {} rather than null: "no group
// recorded" is a collected fact, not a dropped field.
func TestCollectWithoutGroupRecordsEmptyNonNilGroups(t *testing.T) {
	m := Collect(context.Background(), Options{Run: func(context.Context, string, ...string) (string, error) {
		return "", nil
	}})
	if m.Groups == nil {
		t.Fatal("Groups = nil, want an empty non-nil map")
	}
	if len(m.Groups) != 0 {
		t.Errorf("Groups = %v, want empty when no group is named", m.Groups)
	}
}

// The invariant the whole per-group design rests on: a cheap single-group
// refresh records its own provenance and touches nothing else. If this ever
// regresses, refreshing the seconds-long microbenchmark silently re-captions
// the hours-long CRUD sweep it shares metadata.json with (issue #266).
func TestStampGroupLeavesEveryOtherFieldAlone(t *testing.T) {
	clean := false
	crud := Provenance{GitCommit: "aaaa1111", CPUModel: "Old Bench Host", GitDirty: &clean}
	existing := Metadata{
		SchemaVersion:             SchemaVersion,
		Timestamp:                 "2026-08-27T21:57:34Z",
		GitCommit:                 "aaaa1111",
		CPUModel:                  "Old Bench Host",
		PostgresVersion:           "postgres:16.4-alpine",
		FrameworkVersions:         map[string]string{"rails": "8.1.3.1"},
		ResourceLimitsByFramework: map[string]string{"rails": "enforced"},
		PostgresResourceLimits:    "enforced",
		DurationSeconds:           30,
		WarmupSeconds:             10,
		Concurrency:               []int{1, 10, 100, 500, 1000},
		Trials:                    5,
		Groups:                    map[string]Provenance{GroupCRUD: crud},
	}

	got := StampGroup(existing, GroupMicrobench, Provenance{GitCommit: "bbbb2222", CPUModel: "New Dev Host"})

	if got.Groups[GroupCRUD] != crud {
		t.Errorf("the CRUD group was disturbed: %+v, want %+v", got.Groups[GroupCRUD], crud)
	}
	if got.Groups[GroupMicrobench].GitCommit != "bbbb2222" {
		t.Errorf("microbench group = %+v, want the stamped commit", got.Groups[GroupMicrobench])
	}
	// Every field the microbench run knows nothing about must be untouched.
	if got.GitCommit != "aaaa1111" || got.Timestamp != "2026-08-27T21:57:34Z" || got.CPUModel != "Old Bench Host" {
		t.Errorf("the top-level block was restamped: %+v", got)
	}
	if got.Trials != 5 || got.DurationSeconds != 30 || len(got.Concurrency) != 5 {
		t.Errorf("the CRUD protocol was overwritten: %+v", got)
	}
	if got.PostgresVersion != "postgres:16.4-alpine" || got.PostgresResourceLimits != "enforced" ||
		got.FrameworkVersions["rails"] != "8.1.3.1" || got.ResourceLimitsByFramework["rails"] != "enforced" {
		t.Errorf("shared run parameters were overwritten: %+v", got)
	}
	// The input must not be mutated in place — callers hold the value they read.
	if len(existing.Groups) != 1 {
		t.Errorf("StampGroup mutated its argument's Groups: %v", existing.Groups)
	}
}

// Merge is the multi-app path: each run-crud invocation writes the whole record,
// so an earlier app's contributions — including its group provenance — must
// survive the next app's write.
func TestMergeUnionsMapsAndKeepsEveryGroup(t *testing.T) {
	existing := Metadata{
		FrameworkVersions:         map[string]string{"rails": "8.1.3.1"},
		RuntimeVersions:           map[string]string{"ruby": "3.3.12"},
		ResourceLimitsByFramework: map[string]string{"rails": "partial: memory unset"},
		PostgresResourceLimits:    "enforced",
		Groups: map[string]Provenance{
			GroupCRUD:      {GitCommit: "aaaa1111"},
			GroupFootprint: {GitCommit: "cccc3333"},
		},
	}
	incoming := Metadata{
		FrameworkVersions:         map[string]string{"gombit": "v0.1.3"},
		RuntimeVersions:           map[string]string{"go": "go1.26.1"},
		ResourceLimitsByFramework: map[string]string{"gombit": "enforced"},
		Groups:                    map[string]Provenance{GroupCRUD: {GitCommit: "dddd4444"}},
	}

	got := Merge(existing, incoming)

	if got.FrameworkVersions["rails"] != "8.1.3.1" || got.FrameworkVersions["gombit"] != "v0.1.3" {
		t.Errorf("FrameworkVersions = %v, want both apps", got.FrameworkVersions)
	}
	if got.RuntimeVersions["ruby"] != "3.3.12" || got.RuntimeVersions["go"] != "go1.26.1" {
		t.Errorf("RuntimeVersions = %v, want both runtimes", got.RuntimeVersions)
	}
	// A partial verdict on one app must not be erased by another's enforced.
	if got.ResourceLimitsByFramework["rails"] != "partial: memory unset" {
		t.Errorf("ResourceLimitsByFramework = %v, want rails' partial preserved", got.ResourceLimitsByFramework)
	}
	// An empty Postgres verdict means "this run did not re-check", not "unknown".
	if got.PostgresResourceLimits != "enforced" {
		t.Errorf("PostgresResourceLimits = %q, want the prior verdict preserved", got.PostgresResourceLimits)
	}
	// A group is replaced only by a producer of that same group.
	if got.Groups[GroupCRUD].GitCommit != "dddd4444" {
		t.Errorf("crud group = %+v, want the incoming producer's commit", got.Groups[GroupCRUD])
	}
	if got.Groups[GroupFootprint].GitCommit != "cccc3333" {
		t.Errorf("footprint group = %+v, want the untouched prior commit", got.Groups[GroupFootprint])
	}
}

// A snapshot written before per-group provenance existed has no groups entry,
// and its top-level block IS every group's provenance — one run produced the
// whole file — so the fallback is exact, not a guess.
func TestGroupProvenanceFallsBackToTopLevelForLegacySnapshots(t *testing.T) {
	legacy := Metadata{GitCommit: "aaaa1111", CPUModel: "Old Bench Host", GoVersion: "go1.27.0"}
	for _, g := range KnownGroups {
		if got := legacy.GroupProvenance(g); got != legacy.Provenance() {
			t.Errorf("GroupProvenance(%q) = %+v, want the top-level block", g, got)
		}
	}
	// Once a group is stamped, only that group leaves the fallback.
	stamped := StampGroup(legacy, GroupMicrobench, Provenance{GitCommit: "bbbb2222"})
	if stamped.GroupProvenance(GroupMicrobench).GitCommit != "bbbb2222" {
		t.Error("a stamped group must use its own provenance, not the fallback")
	}
	if stamped.GroupProvenance(GroupCRUD).GitCommit != "aaaa1111" {
		t.Error("an unstamped group must still fall back to the top-level block")
	}
}

// One group measured against uncommitted source taints the whole block: a
// reader skimming a table cannot tell which group produced it.
func TestAnyDirtySeesGroupDirtEvenWhenTopLevelIsClean(t *testing.T) {
	clean, dirty := false, true
	m := Metadata{GitDirty: &clean, Groups: map[string]Provenance{
		GroupCRUD:       {GitDirty: &clean},
		GroupMicrobench: {GitDirty: &dirty},
	}}
	if !m.AnyDirty() {
		t.Error("AnyDirty = false, want true when a group was measured dirty")
	}
	m.Groups[GroupMicrobench] = Provenance{GitDirty: &clean}
	if m.AnyDirty() {
		t.Error("AnyDirty = true, want false when nothing is dirty")
	}
	// Unknown (nil) is not dirt — it is unknown, and the reduced/unknown case is
	// not what this banner is for.
	m.Groups[GroupMicrobench] = Provenance{}
	if m.AnyDirty() {
		t.Error("AnyDirty = true, want false when a group's dirtiness is unknown")
	}
}

func TestValidGroupRejectsUnknownNames(t *testing.T) {
	for _, g := range KnownGroups {
		if !ValidGroup(g) {
			t.Errorf("ValidGroup(%q) = false, want true", g)
		}
	}
	for _, bad := range []string{"", "microbnech", "Microbench", "crud-all"} {
		if ValidGroup(bad) {
			t.Errorf("ValidGroup(%q) = true, want false", bad)
		}
	}
}

func TestParseCPUModelArmFallback(t *testing.T) {
	// aarch64 /proc/cpuinfo: no "model name"; devicetree "Model" is the label.
	arm := "processor\t: 0\nBogoMIPS\t: 108.00\nCPU implementer\t: 0x41\nCPU part\t: 0xd0b\nModel\t\t: Raspberry Pi 5 Model B Rev 1.0\n"
	if got := parseCPUModel(arm); got != "Raspberry Pi 5 Model B Rev 1.0" {
		t.Errorf("parseCPUModel(arm) = %q, want the Model line", got)
	}
	// A bare ARM cpuinfo with only CPU part codes records unknown, not junk.
	bare := "processor\t: 0\nCPU implementer\t: 0x41\nCPU part\t: 0xd0b\n"
	if got := parseCPUModel(bare); got != "" {
		t.Errorf("parseCPUModel(bare arm) = %q, want empty (unknown)", got)
	}
}

func TestParseCPUModel(t *testing.T) {
	cpuinfo := "processor\t: 0\nvendor_id\t: GenuineIntel\nmodel name\t: Intel(R) Core(TM) i7-9750H CPU @ 2.60GHz\ncpu MHz\t: 2600\n"
	if got := parseCPUModel(cpuinfo); got != "Intel(R) Core(TM) i7-9750H CPU @ 2.60GHz" {
		t.Errorf("parseCPUModel = %q", got)
	}
	if got := parseCPUModel("no model here\n"); got != "" {
		t.Errorf("parseCPUModel(no model) = %q, want empty", got)
	}
}

func TestParseMemTotalBytes(t *testing.T) {
	meminfo := "MemTotal:       16311072 kB\nMemFree:         1234567 kB\n"
	if got := parseMemTotalBytes(meminfo); got != 16311072*1024 {
		t.Errorf("parseMemTotalBytes = %d, want %d", got, int64(16311072)*1024)
	}
	if got := parseMemTotalBytes("MemFree: 100 kB\n"); got != 0 {
		t.Errorf("parseMemTotalBytes(no MemTotal) = %d, want 0", got)
	}
}
