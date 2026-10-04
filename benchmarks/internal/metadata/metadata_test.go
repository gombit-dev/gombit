package metadata

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
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
	declared := base
	declared.HostClass = HostClassDedicated
	if !base.ComparableTo(declared) {
		t.Error("units differing only in their host-class declaration must be comparable")
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

// The declaration is a closed set matched exactly: a near miss is an error, so
// it can be refused before a run rather than discovered in the banner.
func TestParseHostClass(t *testing.T) {
	for _, ok := range []string{"", HostClassDedicated, HostClassDeveloper} {
		if got, err := ParseHostClass(ok); err != nil || got != ok {
			t.Errorf("ParseHostClass(%q) = %q, %v; want it accepted", ok, got, err)
		}
	}
	for _, bad := range []string{"Dedicated", "dedicaed", " dedicated", "laptop", "dedicated`", "dedicated\nx"} {
		if _, err := ParseHostClass(bad); err == nil || !strings.Contains(err.Error(), HostClassEnv) {
			t.Errorf("ParseHostClass(%q) err = %v, want a refusal naming %s", bad, err, HostClassEnv)
		}
	}
	t.Setenv(HostClassEnv, "Dedicated")
	if CheckHostClassEnv() == nil {
		t.Error("CheckHostClassEnv accepted an invalid environment value")
	}
	t.Setenv(HostClassEnv, HostClassDeveloper)
	if err := CheckHostClassEnv(); err != nil {
		t.Errorf("CheckHostClassEnv(developer) = %v", err)
	}
}

// Collect records a valid declaration, and records nothing when none was made
// or the value is invalid (producers refuse those before measuring).
func TestCollectRecordsTheDeclaredHostClass(t *testing.T) {
	run := func(context.Context, string, ...string) (string, error) { return "", nil }
	for env, want := range map[string]string{HostClassDedicated: HostClassDedicated, HostClassDeveloper: HostClassDeveloper, "": "", "laptop": ""} {
		m := Collect(context.Background(), Options{Run: run, Getenv: func(key string) string {
			if key != HostClassEnv {
				t.Errorf("Getenv(%q), want %q", key, HostClassEnv)
			}
			return env
		}})
		if m.HostClass != want || m.Provenance().HostClass != want {
			t.Errorf("env %q: HostClass = %q / provenance %q, want %q", env, m.HostClass, m.Provenance().HostClass, want)
		}
	}
	m := Metadata{}.WithProvenance(Provenance{HostClass: HostClassDeveloper})
	if m.HostClass != HostClassDeveloper {
		t.Errorf("WithProvenance dropped HostClass: %q", m.HostClass)
	}
}

// Only an explicit dedicated declaration is dedicated; an unrecorded unit, an
// empty class and any other value are not, and units outside the given set are
// not judged.
func TestNonDedicatedUnitsFailsClosed(t *testing.T) {
	m := Metadata{Groups: map[string]map[string]Provenance{
		GroupCRUD: {
			"rails":  {HostClass: HostClassDedicated},
			"gombit": {HostClass: HostClassDeveloper},
			"django": {},
			"nest":   {HostClass: "Dedicated"},
		},
	}}
	got := m.NonDedicatedUnits(GroupCRUD, []string{"rails", "gombit", "django", "nest", "laravel"})
	if strings.Join(got, ",") != "gombit,django,nest,laravel" {
		t.Errorf("NonDedicatedUnits = %v, want gombit,django,nest,laravel", got)
	}
	if got := m.NonDedicatedUnits(GroupCRUD, []string{"rails"}); len(got) != 0 {
		t.Errorf("NonDedicatedUnits(rails) = %v, want none", got)
	}
	legacy := Metadata{HostClass: HostClassDedicated}
	if got := legacy.NonDedicatedUnits(GroupCRUD, []string{"rails"}); len(got) != 0 {
		t.Errorf("a snapshot with no recorded unit is judged by its top level; got %v", got)
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

// Equal is exact on every field, and the concurrency ladder is ordered: the
// report prints it in recorded order, so a reordered list is a different
// statement of the protocol. A zero is a value, not a wildcard.
func TestRunParamsEqual(t *testing.T) {
	canonical := RunParams{
		Concurrency: []int{1, 10, 100}, Trials: 5, DurationSeconds: 30, WarmupSeconds: 10,
		BenchmarkTool: "grafana/k6:0.55.0",
	}
	if !canonical.Equal(canonical) {
		t.Error("identical parameters must be equal")
	}
	for name, mutate := range map[string]func(*RunParams){
		"concurrency": func(p *RunParams) { p.Concurrency = []int{1, 10} },
		"reordered":   func(p *RunParams) { p.Concurrency = []int{100, 10, 1} },
		"trials":      func(p *RunParams) { p.Trials = 0 },
		"duration":    func(p *RunParams) { p.DurationSeconds = 1 },
		"warm-up":     func(p *RunParams) { p.WarmupSeconds = 0 },
		"tool":        func(p *RunParams) { p.BenchmarkTool = "grafana/k6:0.99.0" },
	} {
		other := canonical
		other.Concurrency = append([]int(nil), canonical.Concurrency...)
		mutate(&other)
		if canonical.Equal(other) || other.Equal(canonical) {
			t.Errorf("parameters differing in %s must not be equal", name)
		}
	}
}

// Each unit answers for its own protocol (#377). The top level stands in only
// for a snapshot that records no unit at all, as it does for provenance.
func TestUnitRunParamsIsPerUnit(t *testing.T) {
	canonical := RunParams{Concurrency: []int{1, 10}, Trials: 5, DurationSeconds: 30, WarmupSeconds: 10, BenchmarkTool: "k6"}
	reduced := RunParams{Concurrency: []int{1}, Trials: 1, DurationSeconds: 5, WarmupSeconds: 1, BenchmarkTool: "k6"}

	legacy := Metadata{Concurrency: canonical.Concurrency, Trials: 5, DurationSeconds: 30, WarmupSeconds: 10, BenchmarkTool: "k6"}
	if got, ok := legacy.UnitRunParams(GroupCRUD, "gombit:crud-list"); !ok || !got.Equal(canonical) {
		t.Errorf("a snapshot recording no unit must fall back to its top level, got %+v, %v", got, ok)
	}
	if _, ok := (Metadata{}).UnitRunParams(GroupCRUD, "gombit:crud-list"); ok {
		t.Error("a snapshot recording nothing has no protocol to report")
	}

	// The top level now says "reduced", written by the last run; neither unit
	// was measured under it.
	m := Metadata{Concurrency: reduced.Concurrency, Trials: 1, DurationSeconds: 5, WarmupSeconds: 1, BenchmarkTool: "k6"}
	listParams := canonical
	m = StampUnit(m, GroupCRUD, "gombit:crud-list", Provenance{GitCommit: "a", Protocol: &listParams})
	m = StampUnit(m, GroupCRUD, "rails:crud-list", Provenance{GitCommit: "a"})
	if got, ok := m.UnitRunParams(GroupCRUD, "gombit:crud-list"); !ok || !got.Equal(canonical) {
		t.Errorf("a unit must report its own protocol, not the top level's, got %+v, %v", got, ok)
	}
	if _, ok := m.UnitRunParams(GroupCRUD, "rails:crud-list"); ok {
		t.Error("a unit stamped without a protocol must report none, not borrow the top level")
	}
	if _, ok := m.UnitRunParams(GroupCRUD, "django:crud-list"); ok {
		t.Error("an unrecorded unit must not borrow the rewritable top level")
	}
}

// A snapshot written before protocols were per unit described every CRUD unit
// with the top-level parameters. The first producer to rewrite the top level
// must file that on each unit first, or the next run would leave those rows
// with no protocol at all.
func TestMergeFilesTheLegacyProtocolOnEveryCRUDUnitBeforeTheTopLevelMoves(t *testing.T) {
	canonical := RunParams{Concurrency: []int{1, 10}, Trials: 5, DurationSeconds: 30, WarmupSeconds: 10, BenchmarkTool: "k6"}
	existing := Metadata{Concurrency: canonical.Concurrency, Trials: 5, DurationSeconds: 30, WarmupSeconds: 10, BenchmarkTool: "k6"}
	existing = StampUnit(existing, GroupCRUD, "rails:crud-list", Provenance{GitCommit: "a"})
	existing = StampUnit(existing, GroupMicrobench, "gin", Provenance{GitCommit: "a"})

	reduced := RunParams{Concurrency: []int{1}, Trials: 1, DurationSeconds: 5, WarmupSeconds: 0, BenchmarkTool: "k6"}
	incoming := Metadata{Concurrency: reduced.Concurrency, Trials: 1, DurationSeconds: 5, BenchmarkTool: "k6"}
	incoming = StampUnit(incoming, GroupCRUD, "gombit:auth-jwt", Provenance{GitCommit: "b", Protocol: &reduced})

	merged := Merge(existing, incoming)
	if got, ok := merged.UnitRunParams(GroupCRUD, "rails:crud-list"); !ok || !got.Equal(canonical) {
		t.Errorf("the legacy unit must keep the protocol it was measured under, got %+v, %v", got, ok)
	}
	if got, ok := merged.UnitRunParams(GroupCRUD, "gombit:auth-jwt"); !ok || !got.Equal(reduced) {
		t.Errorf("the incoming unit must keep its own protocol, got %+v, %v", got, ok)
	}
	if merged.Groups[GroupMicrobench]["gin"].Protocol != nil {
		t.Error("the top-level protocol never described microbench rows and must not be filed on them")
	}
	if !merged.RunParams().Equal(reduced) {
		t.Errorf("the top level is still last-writer-wins for older readers, got %+v", merged.RunParams())
	}
}

func TestProtocolIsOmittedFromJSONUntilRecorded(t *testing.T) {
	m := StampUnit(Metadata{}, GroupMicrobench, "gin", Provenance{GitCommit: "a"})
	var b strings.Builder
	if err := WriteJSON(&b, m); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), `"protocol"`) {
		t.Errorf("a unit with no protocol must not serialize one:\n%s", b.String())
	}

	params := RunParams{Concurrency: []int{1, 10}, Trials: 2, DurationSeconds: 3, WarmupSeconds: 0, BenchmarkTool: "k6"}
	m = StampUnit(m, GroupCRUD, "gombit:crud-list", Provenance{GitCommit: "a", Protocol: &params})
	b.Reset()
	if err := WriteJSON(&b, m); err != nil {
		t.Fatal(err)
	}
	back, err := ReadJSON(strings.NewReader(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := back.UnitRunParams(GroupCRUD, "gombit:crud-list"); !ok || !got.Equal(params) {
		t.Errorf("a unit's protocol must round-trip, got %+v, %v\n%s", got, ok, b.String())
	}
}

// Protocol is reported per unit on its own; it is not part of "same source,
// machine and toolchain".
func TestComparableToIgnoresProtocol(t *testing.T) {
	a, b := RunParams{Trials: 5}, RunParams{Trials: 1}
	if !(Provenance{GitCommit: "x", Protocol: &a}).ComparableTo(Provenance{GitCommit: "x", Protocol: &b}) {
		t.Error("units at one commit, host and toolchain must stay comparable whatever their protocols")
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
	if want := (RunParams{Concurrency: []int{10}, Trials: 2, DurationSeconds: 3, WarmupSeconds: 4, BenchmarkTool: "k6"}); !m.RunParams().Equal(want) {
		t.Errorf("RunParams must mirror the recorded fields, got %+v", m.RunParams())
	}
}

// The Makefile and the orchestration scripts refuse a bad declaration before
// measuring, from host-class.sh's own copy of the set. If the two drifted, make
// would refuse a value the producers accept, or let through one that run-crud
// refuses only after the first app is built and seeded.
func TestHostClassShellListMatchesGo(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "scripts", "host-class.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^host_classes=\((.*)\)$`).FindSubmatch(src)
	if m == nil {
		t.Fatal("host-class.sh: no host_classes=(...) line")
	}
	var shell []string
	for _, field := range strings.Fields(string(m[1])) {
		unquoted, err := strconv.Unquote(field)
		if err != nil {
			t.Fatalf("host-class.sh: entry %s is not a double-quoted string: %v", field, err)
		}
		shell = append(shell, unquoted)
	}
	if !slices.Equal(shell, HostClasses) {
		t.Errorf("host-class.sh allows %q, metadata.HostClasses is %q", shell, HostClasses)
	}
}
