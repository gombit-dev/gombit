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

// Collect files no unit entry: stamping is owned by whichever producer wrote the
// rows. A collection that measured nothing must claim nothing.
func TestCollectFilesNoUnitAndKeepsGroupsNonNil(t *testing.T) {
	m := Collect(context.Background(), Options{Run: func(context.Context, string, ...string) (string, error) {
		return "", nil
	}})
	if m.Groups == nil {
		t.Fatal("Groups = nil, want an empty non-nil map")
	}
	if len(m.Groups) != 0 {
		t.Errorf("Groups = %v, want empty", m.Groups)
	}
	if m.Provenance().GoVersion != runtime.Version() {
		t.Errorf("Provenance().GoVersion = %q, want %q", m.Provenance().GoVersion, runtime.Version())
	}
}

// THE regression this round exists for. Every one of these data files merges
// row-wise and subset runs are supported, so re-measuring ONE unit must leave
// every sibling's provenance exactly as it was. A group-wide stamp here is what
// let `APPS=gombit make benchmark-footprint` relabel five untouched rows.
func TestStampUnitRefreshesOneUnitAndLeavesSiblingsAlone(t *testing.T) {
	commitA := Provenance{GitCommit: "aaaa1111", CPUModel: "Bench Host", Timestamp: "2026-09-01T00:00:00Z"}
	apps := []string{"django", "gin-gorm", "gombit", "laravel", "nestjs", "rails"}

	meta := Metadata{SchemaVersion: SchemaVersion, PostgresVersion: "postgres:16.4-alpine", Trials: 5}
	for _, app := range apps {
		meta = StampUnit(meta, GroupFootprint, app, commitA)
	}
	// Another group must be untouched too.
	meta = StampUnit(meta, GroupCRUD, "gombit", commitA)

	// `APPS=gombit make benchmark-footprint` at a later commit.
	commitB := Provenance{GitCommit: "bbbb2222", CPUModel: "Dev Host", Timestamp: "2026-09-08T00:00:00Z"}
	got := StampUnit(meta, GroupFootprint, "gombit", commitB)

	if got.Groups[GroupFootprint]["gombit"].GitCommit != "bbbb2222" {
		t.Errorf("the re-measured unit = %+v, want the new commit", got.Groups[GroupFootprint]["gombit"])
	}
	for _, app := range apps {
		if app == "gombit" {
			continue
		}
		if c := got.Groups[GroupFootprint][app].GitCommit; c != "aaaa1111" {
			t.Errorf("untouched footprint unit %q = %q, want the original commit — a subset run relabelled a row it never measured", app, c)
		}
	}
	if got.Groups[GroupCRUD]["gombit"].GitCommit != "aaaa1111" {
		t.Errorf("a sibling GROUP was disturbed: %+v", got.Groups[GroupCRUD])
	}
	// Shared run parameters and the top-level block stay put.
	if got.PostgresVersion != "postgres:16.4-alpine" || got.Trials != 5 || got.GitCommit != "" {
		t.Errorf("stamping touched fields it does not own: %+v", got)
	}
	// The input value must not be mutated in place.
	if meta.Groups[GroupFootprint]["gombit"].GitCommit != "aaaa1111" {
		t.Error("StampUnit mutated its argument")
	}
}

// Merge is the multi-app path: each run-crud invocation writes the whole record,
// so an earlier app's contributions — including its unit provenance — must
// survive the next app's write.
func TestMergeUnionsMapsAndKeepsEveryUnit(t *testing.T) {
	existing := Metadata{
		FrameworkVersions:         map[string]string{"rails": "8.1.3.1"},
		RuntimeVersions:           map[string]string{"ruby": "3.3.12"},
		ResourceLimitsByFramework: map[string]string{"rails": "partial: memory unset"},
		PostgresResourceLimits:    "enforced",
		Groups: map[string]map[string]Provenance{
			GroupCRUD:      {"rails": {GitCommit: "aaaa1111"}, "gombit": {GitCommit: "aaaa1111"}},
			GroupFootprint: {"rails": {GitCommit: "cccc3333"}},
		},
	}
	incoming := Metadata{
		FrameworkVersions:         map[string]string{"gombit": "v0.1.3"},
		RuntimeVersions:           map[string]string{"go": "go1.26.1"},
		ResourceLimitsByFramework: map[string]string{"gombit": "enforced"},
		Groups:                    map[string]map[string]Provenance{GroupCRUD: {"gombit": {GitCommit: "dddd4444"}}},
	}

	got := Merge(existing, incoming)

	if got.FrameworkVersions["rails"] != "8.1.3.1" || got.FrameworkVersions["gombit"] != "v0.1.3" {
		t.Errorf("FrameworkVersions = %v, want both apps", got.FrameworkVersions)
	}
	if got.RuntimeVersions["ruby"] != "3.3.12" || got.RuntimeVersions["go"] != "go1.26.1" {
		t.Errorf("RuntimeVersions = %v, want both runtimes", got.RuntimeVersions)
	}
	if got.ResourceLimitsByFramework["rails"] != "partial: memory unset" {
		t.Errorf("ResourceLimitsByFramework = %v, want rails' partial preserved", got.ResourceLimitsByFramework)
	}
	if got.PostgresResourceLimits != "enforced" {
		t.Errorf("PostgresResourceLimits = %q, want the prior verdict preserved", got.PostgresResourceLimits)
	}
	// Only the unit the incoming producer measured moves.
	if got.Groups[GroupCRUD]["gombit"].GitCommit != "dddd4444" {
		t.Errorf("crud/gombit = %+v, want the incoming commit", got.Groups[GroupCRUD]["gombit"])
	}
	if got.Groups[GroupCRUD]["rails"].GitCommit != "aaaa1111" {
		t.Errorf("crud/rails = %+v, want the untouched prior commit", got.Groups[GroupCRUD]["rails"])
	}
	if got.Groups[GroupFootprint]["rails"].GitCommit != "cccc3333" {
		t.Errorf("footprint/rails = %+v, want the untouched prior commit", got.Groups[GroupFootprint]["rails"])
	}
	// Merge must not alias the input maps.
	existing.Groups[GroupCRUD]["rails"] = Provenance{GitCommit: "mutated"}
	if got.Groups[GroupCRUD]["rails"].GitCommit != "aaaa1111" {
		t.Error("Merge aliased the existing Groups map instead of copying it")
	}
}

// UnitsProvenance is what the renderer asks: are these rows comparable with each
// other, and if not, what did each one actually run at?
func TestUnitsProvenanceReportsUniformityAcrossTheRenderedUnits(t *testing.T) {
	a := Provenance{GitCommit: "aaaa1111", CPUModel: "Host"}
	meta := StampUnit(StampUnit(Metadata{}, GroupFootprint, "rails", a), GroupFootprint, "gombit", a)

	if _, uniform := meta.UnitsProvenance(GroupFootprint, []string{"rails", "gombit"}); !uniform {
		t.Error("two units measured at the same commit must be uniform")
	}

	b := Provenance{GitCommit: "bbbb2222", CPUModel: "Host"}
	meta = StampUnit(meta, GroupFootprint, "gombit", b)
	provs, uniform := meta.UnitsProvenance(GroupFootprint, []string{"rails", "gombit"})
	if uniform {
		t.Error("units measured at different commits must NOT be uniform")
	}
	if provs["rails"].GitCommit != "aaaa1111" || provs["gombit"].GitCommit != "bbbb2222" {
		t.Errorf("per-unit provenance = %+v", provs)
	}

	// Clean-tree pointers collected separately must compare equal: uniformity is
	// about the recorded values, not about *bool identity.
	c1, c2 := false, false
	meta = StampUnit(Metadata{}, GroupCRUD, "rails", Provenance{GitCommit: "x", GitDirty: &c1})
	meta = StampUnit(meta, GroupCRUD, "gombit", Provenance{GitCommit: "x", GitDirty: &c2})
	if _, uniform := meta.UnitsProvenance(GroupCRUD, []string{"rails", "gombit"}); !uniform {
		t.Error("identical provenance with distinct *bool addresses must count as uniform")
	}
}

// Comparability is about source state, host and toolchain — never the clock.
func TestComparableToIgnoresTimestampButNotTheRest(t *testing.T) {
	clean, dirty := false, true
	base := Provenance{GitCommit: "aaaa1111", GitDirty: &clean, CPUModel: "Host", GoVersion: "go1.26.1"}

	later := base
	later.Timestamp = "2026-09-15T00:37:00Z"
	if !base.ComparableTo(later) {
		t.Error("units of one run differing only in clock time must be comparable")
	}
	for name, other := range map[string]Provenance{
		"commit":    {GitCommit: "bbbb2222", GitDirty: &clean, CPUModel: "Host", GoVersion: "go1.26.1"},
		"host":      {GitCommit: "aaaa1111", GitDirty: &clean, CPUModel: "Other", GoVersion: "go1.26.1"},
		"toolchain": {GitCommit: "aaaa1111", GitDirty: &clean, CPUModel: "Host", GoVersion: "go1.25.7"},
		"dirtiness": {GitCommit: "aaaa1111", GitDirty: &dirty, CPUModel: "Host", GoVersion: "go1.26.1"},
	} {
		if base.ComparableTo(other) {
			t.Errorf("a differing %s must break comparability", name)
		}
	}
	// Unknown dirtiness is not the same as known-clean.
	unknown := base
	unknown.GitDirty = nil
	if base.ComparableTo(unknown) {
		t.Error("known-clean and unknown dirtiness must not be comparable")
	}
}

// A snapshot written before per-unit provenance has no entry, and its top-level
// block IS every unit's provenance — one run produced the whole file — so the
// fallback is exact, not a guess, and such a snapshot still renders uniform.
func TestUnitProvenanceFallsBackToTopLevelForLegacySnapshots(t *testing.T) {
	legacy := Metadata{GitCommit: "aaaa1111", CPUModel: "Old Bench Host", GoVersion: "go1.27.0"}
	if got := legacy.UnitProvenance(GroupCRUD, "rails"); got != legacy.Provenance() {
		t.Errorf("UnitProvenance = %+v, want the top-level block", got)
	}
	if _, uniform := legacy.UnitsProvenance(GroupCRUD, []string{"rails", "gombit"}); !uniform {
		t.Error("a legacy snapshot must render as one uniform caption, exactly as before")
	}
}

// Once any unit is recorded, the top-level block stops being every unit's
// provenance: run-crud and collect-host-info rewrite it, including for a
// one-app subset. So an unrecorded unit must come back unrecorded — in its own
// group and in every other — never as whatever commit last rewrote the top level.
func TestUnrecordedUnitNeverBorrowsTheRewritableTopLevel(t *testing.T) {
	// The top level has just been rewritten by a one-app CRUD run at bbbb2222.
	m := Metadata{GitCommit: "bbbb2222", CPUModel: "Dev Host"}
	m = StampUnit(m, GroupCRUD, "gombit", Provenance{GitCommit: "bbbb2222", CPUModel: "Dev Host"})

	if got := m.UnitProvenance(GroupCRUD, "gombit"); got.GitCommit != "bbbb2222" {
		t.Errorf("a recorded unit must use its own provenance, got %+v", got)
	}
	for _, c := range []struct{ group, unit string }{
		{GroupCRUD, "rails"},                 // untouched sibling in the same group
		{GroupFootprint, "gombit:container"}, // a group this run never touched
	} {
		if got := m.UnitProvenance(c.group, c.unit); !got.Empty() {
			t.Errorf("%s/%s: unrecorded unit borrowed %+v; want Empty", c.group, c.unit, got)
		}
	}
	if _, comparable := m.UnitsProvenance(GroupCRUD, []string{"gombit", "rails"}); comparable {
		t.Error("a recorded unit and an unrecorded one must not collapse into one caption")
	}
	// An empty-but-present group map is still "no unit recorded".
	legacy := Metadata{GitCommit: "aaaa1111", Groups: map[string]map[string]Provenance{GroupCRUD: {}}}
	if got := legacy.UnitProvenance(GroupCRUD, "rails"); got.GitCommit != "aaaa1111" {
		t.Errorf("a snapshot with no recorded unit must still use its top level, got %+v", got)
	}
}

// AnyUnitDirty judges exactly the units it is given: a dirty unit among them is
// dirt even under a clean top level, a dirty unit outside them is not, and
// unknown dirtiness is unknown rather than dirty.
func TestAnyUnitDirtyJudgesOnlyTheGivenUnits(t *testing.T) {
	clean, dirty := false, true
	m := Metadata{GitDirty: &clean, Groups: map[string]map[string]Provenance{
		GroupCRUD:       {"rails": {GitDirty: &clean}},
		GroupMicrobench: {"gin": {GitDirty: &dirty}, "gombit-ablation": {GitDirty: &dirty}},
	}}
	if !m.AnyUnitDirty(GroupMicrobench, []string{"nethttp", "gin"}) {
		t.Error("AnyUnitDirty = false, want true when a given unit was measured dirty")
	}
	if m.AnyUnitDirty(GroupCRUD, []string{"rails"}) {
		t.Error("AnyUnitDirty = true for a clean group; dirt in another group leaked in")
	}
	m.Groups[GroupMicrobench]["gin"] = Provenance{GitDirty: &clean}
	if m.AnyUnitDirty(GroupMicrobench, []string{"gin"}) {
		t.Error("AnyUnitDirty = true; the dirty gombit-ablation unit was not among the given units")
	}
	// Unknown (nil) is not dirt — it is unknown.
	m.Groups[GroupMicrobench]["gin"] = Provenance{}
	if m.AnyUnitDirty(GroupMicrobench, []string{"gin"}) {
		t.Error("AnyUnitDirty = true, want false when dirtiness is unknown")
	}
	// A snapshot that records no unit is judged by its top level.
	legacy := Metadata{GitDirty: &dirty}
	if !legacy.AnyUnitDirty(GroupCRUD, []string{"rails"}) {
		t.Error("a snapshot with no recorded unit must be judged by its dirty top level")
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

// The five run parameters are recorded once per snapshot, and Merge writes the
// incoming ones whole, so the comparison is directional: an incoming zero is a
// value, and only a recorded side that states nothing at all is absent (#361
// review round 2).
func TestRunParamsConflictsWith(t *testing.T) {
	canonical := RunParams{
		Concurrency: []int{1, 10, 100}, Trials: 5, DurationSeconds: 30, WarmupSeconds: 10,
		BenchmarkTool: "grafana/k6:0.55.0",
	}

	if got := canonical.ConflictsWith(canonical); got != nil {
		t.Errorf("identical parameters must not conflict, got %v", got)
	}
	if got := (RunParams{}).ConflictsWith(canonical); got != nil {
		t.Errorf("a snapshot recording nothing cannot conflict, got %v", got)
	}

	// The review's case: a zero-valued incoming field is not a wildcard, because
	// Merge would write it over rows measured with five trials.
	zeroTrials := canonical
	zeroTrials.Trials = 0
	if got := canonical.ConflictsWith(zeroTrials); len(got) != 1 || got[0] != "trials 5 -> 0" {
		t.Errorf("an incoming zero must conflict with a recorded value, got %v", got)
	}

	// A zero warm-up is a value on both sides: recorded 0 vs incoming 0 is the
	// same protocol, recorded 10s vs incoming 0 is a different one.
	noWarmup := canonical
	noWarmup.WarmupSeconds = 0
	if got := noWarmup.ConflictsWith(noWarmup); got != nil {
		t.Errorf("a deliberate zero warm-up must match itself, got %v", got)
	}
	if got := canonical.ConflictsWith(noWarmup); len(got) != 1 || got[0] != "warm-up 10s -> 0s" {
		t.Errorf("dropping the warm-up must conflict, got %v", got)
	}
	if got := noWarmup.ConflictsWith(canonical); len(got) != 1 || got[0] != "warm-up 0s -> 10s" {
		t.Errorf("adding a warm-up to a zero-warm-up snapshot must conflict, got %v", got)
	}

	// Every field, in a fixed order.
	reduced := RunParams{
		Concurrency: []int{1}, Trials: 1, DurationSeconds: 1, WarmupSeconds: 1,
		BenchmarkTool: "grafana/k6:0.99.0",
	}
	want := []string{
		"concurrency 1/10/100 -> 1",
		"trials 5 -> 1",
		"duration per trial 30s -> 1s",
		"warm-up 10s -> 1s",
		"benchmark tool grafana/k6:0.55.0 -> grafana/k6:0.99.0",
	}
	got := canonical.ConflictsWith(reduced)
	if len(got) != len(want) {
		t.Fatalf("conflicts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("conflict[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// The report prints the ladder in recorded order, so a reordered list is a
	// different statement of the protocol, not the same one.
	reordered := canonical
	reordered.Concurrency = []int{100, 10, 1}
	if got := canonical.ConflictsWith(reordered); len(got) != 1 {
		t.Errorf("a reordered concurrency list must conflict, got %v", got)
	}

	// A partial record (collect-host-info given only -trials) compares its zeros
	// as values and fails closed.
	partial := RunParams{Trials: 5}
	if got := partial.ConflictsWith(canonical); len(got) != 4 {
		t.Errorf("a partial record must conflict on every field it leaves at zero, got %v", got)
	}
}

// Presence is judged for the whole record: any one field makes it recorded.
func TestRunParamsRecorded(t *testing.T) {
	if (RunParams{}).Recorded() {
		t.Error("an empty record must not count as recorded")
	}
	for name, p := range map[string]RunParams{
		"concurrency": {Concurrency: []int{1}},
		"trials":      {Trials: 1},
		"duration":    {DurationSeconds: 1},
		"warm-up":     {WarmupSeconds: 1},
		"tool":        {BenchmarkTool: "k6"},
	} {
		if !p.Recorded() {
			t.Errorf("a record stating only %s must count as recorded", name)
		}
	}
}

func TestMetadataRunParamsReadsTheTopLevelFields(t *testing.T) {
	m := Metadata{Concurrency: []int{10}, Trials: 2, DurationSeconds: 3, WarmupSeconds: 4, BenchmarkTool: "k6"}
	if diffs := m.RunParams().ConflictsWith(RunParams{Concurrency: []int{10}, Trials: 2, DurationSeconds: 3, WarmupSeconds: 4, BenchmarkTool: "k6"}); diffs != nil {
		t.Errorf("RunParams must mirror the recorded fields, got %v", diffs)
	}
}
