// Package metadata collects the reproducibility metadata every full benchmark
// run must capture (issue #141 "Reproducibility metadata"): enough about the
// host, toolchain, and run parameters that a published table can be
// reproduced. "Never publish a table without enough metadata to reproduce
// it."
package metadata

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// SchemaVersion is bumped when the Metadata shape changes incompatibly.
const SchemaVersion = 1

// The measurement groups a results/latest snapshot holds. They look like one
// artifact but are produced by three different targets at wildly different
// costs — `make benchmark-micro` is minutes of pure Go, `benchmark-crud-all` is
// hours of containers and k6, `benchmark-footprint` is minutes of Docker — so
// in practice they are measured at different commits, on different days, and
// potentially on different hosts and toolchains. One snapshot-wide git_commit
// cannot describe all three without misattributing at least two of them, which
// is how the published framework-tax table came to advertise numbers from
// before the PERF-1..4 optimizations (issue #266).
const (
	GroupMicrobench = "microbench"
	GroupCRUD       = "crud"
	GroupFootprint  = "footprint"
)

// KnownGroups is every valid group name. Producers validate against it so a
// typo'd group writes a phantom entry no reader ever looks at, instead of
// silently leaving the table it meant to stamp on the stale fallback.
var KnownGroups = []string{GroupMicrobench, GroupCRUD, GroupFootprint}

// ValidGroup reports whether name is one of KnownGroups.
func ValidGroup(name string) bool {
	for _, g := range KnownGroups {
		if g == name {
			return true
		}
	}
	return false
}

// Provenance is what one measurement group can answer about itself: which
// source state ran, when, on which machine, under which toolchain.
//
// The Go toolchain is recorded here, not only at the top level, because it
// moves the numbers on its own: the framework-tax ladder's net/http rung —
// whose code is stdlib plus the harness, untouched by any framework change —
// reported 18 allocs/op on one toolchain and 21 on another with no source
// change between them. Rows are only comparable against rows produced by the
// same toolchain, so each group has to say which one it used.
type Provenance struct {
	Timestamp string `json:"timestamp"`
	GitCommit string `json:"git_commit"`
	// GitDirty is a pointer for the same reason as Metadata.GitDirty: null is
	// an honest "could not determine", which a plain bool would report clean.
	GitDirty    *bool  `json:"git_dirty"`
	OS          string `json:"os"`
	Kernel      string `json:"kernel"`
	Arch        string `json:"arch"`
	CPUModel    string `json:"cpu_model"`
	LogicalCPUs int    `json:"logical_cpus"`
	RAMBytes    int64  `json:"ram_bytes"`
	GoVersion   string `json:"go_version"`
}

// Empty reports whether nothing at all was recorded — a pre-run placeholder,
// which a caller renders as "unknown" rather than as a row of dashes.
func (p Provenance) Empty() bool {
	return p.GitCommit == "" && p.CPUModel == "" && p.Timestamp == ""
}

// Metadata is the machine-readable run environment (written to
// results/latest/metadata.json). Discovered fields come from the host and
// toolchain; the rest are run parameters the orchestrator supplies via
// Options because they are choices, not facts about the machine.
//
// # Which field answers "when was this measured?"
//
// Groups does, per measurement UNIT. It is the authoritative provenance: each
// unit records the commit, host and toolchain ITS OWN rows were produced at,
// and the report captions each table from the units that table publishes.
//
// The top-level discovered fields (Timestamp, GitCommit, GitDirty, the host
// block, GoVersion) describe whichever collection last rewrote the whole
// record: every run-crud invocation (including an `APPS=` subset of one app)
// and every `make benchmark-metadata`. They are NOT a summary of the newest
// unit, and stamping a unit through StampUnitFile does not advance them. Two
// consequences worth stating plainly:
//
//   - The top-level block is rewritable by producers that did not measure most
//     of the rows beside it, so it can never stand in for a unit's provenance
//     once units are being recorded. After `make benchmark-micro` the top-level
//     GitCommit is older than the microbench units'; after
//     `APPS=gombit make benchmark-crud-all` it is newer than five of the six
//     CRUD rows. Neither is a caption for any table.
//   - Ask Groups, never the top level, when you want a table's provenance.
//     UnitProvenance and UnitsProvenance do this for you. They fall back to the
//     top level only for a snapshot that records no unit at all — one written
//     before Groups existed, where one run produced everything — and report
//     every other unrecorded unit as unrecorded (see UnitProvenance).
//
// The shape is additive, so SchemaVersion stays 1 and older readers keep
// parsing: they see the flat fields they always saw, plus a `groups` object
// they can ignore.
type Metadata struct {
	SchemaVersion int    `json:"schema_version"`
	Timestamp     string `json:"timestamp"`

	GitCommit string `json:"git_commit"`
	// GitDirty is a pointer, not a bool, so the metadata can honestly say "I
	// did not determine this" (null) when `git status` failed or git was
	// missing — a plain bool's zero value (false = clean) would claim a
	// clean tree on the exact runs where the check could not run.
	GitDirty *bool `json:"git_dirty"`

	OS          string `json:"os"`
	Kernel      string `json:"kernel"`
	Arch        string `json:"arch"`
	CPUModel    string `json:"cpu_model"`
	LogicalCPUs int    `json:"logical_cpus"`
	RAMBytes    int64  `json:"ram_bytes"`

	GoVersion            string `json:"go_version"`
	DockerVersion        string `json:"docker_version"`
	DockerComposeVersion string `json:"docker_compose_version"`
	PostgresVersion      string `json:"postgres_version"`

	FrameworkVersions map[string]string `json:"framework_versions"`
	RuntimeVersions   map[string]string `json:"runtime_versions"`
	BenchmarkTool     string            `json:"benchmark_tool"`

	// ResourceLimits is the single scalar the standalone/host-only path records
	// (an intended budget). For a multi-app run the authoritative field is
	// ResourceLimitsByFramework: the *applied* verdict verified per app (from
	// inspect-limits), preserved by merge so a partial/not-applied on one app is
	// never overwritten by an enforced on the next. PostgresResourceLimits is the
	// database container's verified verdict.
	ResourceLimits            string            `json:"resource_limits"`
	ResourceLimitsByFramework map[string]string `json:"resource_limits_by_framework"`
	PostgresResourceLimits    string            `json:"postgres_resource_limits"`
	DurationSeconds           float64           `json:"duration_seconds"`
	WarmupSeconds             float64           `json:"warmup_seconds"`
	Concurrency               []int             `json:"concurrency"`
	Trials                    int               `json:"trials"`

	// Groups maps a group name (Group*) to the provenance of each UNIT it
	// measured. A unit is that group's merge key — the thing a single run can
	// replace on its own:
	//
	//	microbench -> stack namespace   (microbench.MergeStack replaces it whole)
	//	crud       -> framework         (run-crud replaces one app's rows)
	//	footprint  -> framework:variant (footprint.Merge keys on both; see
	//	                                 Footprint.ProvenanceUnit)
	//
	// Provenance is per unit, not per group, because every one of those files is
	// merged row-wise and subset runs are supported (`APPS="gin-gorm gombit"`).
	// A group-wide stamp would let a run that replaced ONE row relabel the whole
	// table: six rows measured at A, `APPS=gombit make benchmark-footprint` at B,
	// and five untouched rows would be captioned B. Keying provenance to the same
	// unit the data merges on makes that state unrepresentable rather than merely
	// detectable.
	//
	// A unit with no entry has no recorded provenance. Only when no unit at all
	// is recorded (a pre-Groups snapshot) do readers use the top-level block
	// instead; see UnitProvenance for why that fallback cannot be per unit.
	// Empty ({}) rather than null so "nothing recorded" is a collected fact, not
	// a dropped field.
	Groups map[string]map[string]Provenance `json:"groups"`
}

// Provenance returns the top-level commit/host/toolchain block as a Provenance
// value — what the collection that wrote this record measured.
func (m Metadata) Provenance() Provenance {
	return Provenance{
		Timestamp:   m.Timestamp,
		GitCommit:   m.GitCommit,
		GitDirty:    m.GitDirty,
		OS:          m.OS,
		Kernel:      m.Kernel,
		Arch:        m.Arch,
		CPUModel:    m.CPUModel,
		LogicalCPUs: m.LogicalCPUs,
		RAMBytes:    m.RAMBytes,
		GoVersion:   m.GoVersion,
	}
}

// UnitProvenance returns the provenance to attribute one unit's data to, or an
// Empty Provenance when none was recorded for it.
//
// The top-level block is used only for a snapshot that records no unit at all.
// Such a snapshot predates per-unit stamping, one run produced the whole file,
// and its top-level block is exactly every unit's provenance.
//
// The fallback is deliberately NOT per unit. Once any unit is recorded, the
// top-level block no longer belongs to the unrecorded ones: run-crud and
// collect-host-info rewrite it on every invocation, including an `APPS=` subset
// that measured one app. Borrowing it would caption five untouched CRUD rows —
// and a footprint table nobody re-measured — with that one app's commit and
// host (issue #266). An unrecorded unit is reported as
// unrecorded, which is true in every order the producers can run in.
func (m Metadata) UnitProvenance(group, unit string) Provenance {
	if p, ok := m.Groups[group][unit]; ok {
		return p
	}
	if m.RecordsUnits() {
		return Provenance{}
	}
	return m.Provenance()
}

// WithProvenance returns m with its top-level commit/host/toolchain block
// replaced by prov, leaving the run parameters and Groups untouched. It is the
// inverse of Provenance.
func (m Metadata) WithProvenance(prov Provenance) Metadata {
	m.Timestamp = prov.Timestamp
	m.GitCommit = prov.GitCommit
	m.GitDirty = prov.GitDirty
	m.OS = prov.OS
	m.Kernel = prov.Kernel
	m.Arch = prov.Arch
	m.CPUModel = prov.CPUModel
	m.LogicalCPUs = prov.LogicalCPUs
	m.RAMBytes = prov.RAMBytes
	m.GoVersion = prov.GoVersion
	return m
}

// RecordsUnits reports whether any unit of any group has recorded provenance,
// i.e. whether this snapshot is past the point where its top-level block could
// describe every row.
func (m Metadata) RecordsUnits() bool {
	for _, units := range m.Groups {
		if len(units) > 0 {
			return true
		}
	}
	return false
}

// UnitsProvenance returns each unit's provenance and whether the units are
// comparable with one another. Callers rendering a table pass the units that
// table actually publishes: comparable means one honest caption covers every
// row, and anything else means the rows were not measured together and must be
// captioned individually.
func (m Metadata) UnitsProvenance(group string, units []string) (map[string]Provenance, bool) {
	out := make(map[string]Provenance, len(units))
	comparable := true
	var first Provenance
	for i, u := range units {
		p := m.UnitProvenance(group, u)
		out[u] = p
		switch {
		case i == 0:
			first = p
		case !p.ComparableTo(first):
			comparable = false
		}
	}
	return out, comparable
}

// ComparableTo reports whether two units' numbers may be read against each
// other: same source state, same machine, same toolchain.
//
// Timestamp is deliberately excluded. One run measures its units in sequence —
// `make benchmark-micro` is four `go test` processes and finishes each stack a
// minute or two apart — so comparing timestamps would flag every ordinary run as
// "not measured together". A warning that fires on every run is worth nothing
// when the real case arrives. What makes rows comparable is the commit, the
// host and the toolchain; the clock is provenance to report, not a difference to
// act on.
//
// GitDirty is a pointer, so it is compared by what it points at: two separately
// collected records of the same clean tree are the same state, not different
// ones because they hold different addresses.
func (p Provenance) ComparableTo(other Provenance) bool {
	if (p.GitDirty == nil) != (other.GitDirty == nil) {
		return false
	}
	if p.GitDirty != nil && *p.GitDirty != *other.GitDirty {
		return false
	}
	p.GitDirty, other.GitDirty = nil, nil
	p.Timestamp, other.Timestamp = "", ""
	return p == other
}

// AnyUnitDirty reports whether any of the given units of group was measured on
// a dirty working tree, judged through UnitProvenance so a snapshot that
// records no unit is judged by its top-level block. Unknown dirtiness (nil) is
// not dirt.
//
// Callers pass the units they publish, not every unit on file: a snapshot's
// data files also hold rows no table renders (the `gombit-ablation` ladder in
// microbench.json, an embedded footprint variant), and a dirty diagnostic run
// of those says nothing about the numbers a reader is looking at.
func (m Metadata) AnyUnitDirty(group string, units []string) bool {
	for _, u := range units {
		if p := m.UnitProvenance(group, u); p.GitDirty != nil && *p.GitDirty {
			return true
		}
	}
	return false
}

// Runner runs a command and returns its trimmed stdout. It exists so tests can
// stub git/uname/docker without those tools being installed; production uses
// execRunner.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// Options carries the injectable seams (Now, Run) and the run-parameter fields
// the orchestrator knows but the host does not.
type Options struct {
	Now func() time.Time
	Run Runner

	PostgresVersion           string
	FrameworkVersions         map[string]string
	RuntimeVersions           map[string]string
	BenchmarkTool             string
	ResourceLimits            string
	ResourceLimitsByFramework map[string]string
	PostgresResourceLimits    string
	DurationSeconds           float64
	WarmupSeconds             float64
	Concurrency               []int
	Trials                    int
}

// Collect gathers the metadata. Discovery is best-effort: a missing tool
// (docker on a CI runner without it, git outside a checkout) leaves its field
// empty rather than failing the whole collection — a benchmark run should
// still record everything it *can*.
func Collect(ctx context.Context, opts Options) Metadata {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now() }
	}
	run := opts.Run
	if run == nil {
		run = execRunner
	}

	commit, commitErr := run(ctx, "git", "rev-parse", "HEAD")
	if commitErr != nil {
		commit = ""
	}
	kernel, _ := run(ctx, "uname", "-r")
	dockerVersion, _ := run(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	composeVersion, _ := run(ctx, "docker", "compose", "version", "--short")

	// git_dirty answers "what source code ran?", so it excludes
	// benchmarks/results/ — a suite writes result files for earlier apps before
	// Collect runs for a later one, which would otherwise flag every multi-app
	// run dirty even on a clean source tree. It is set only when the porcelain
	// status actually succeeded; a failed check (or missing git) leaves it nil
	// -> JSON null -> "unknown".
	var gitDirty *bool
	if status, statusErr := run(ctx, "git", "status", "--porcelain", "--", ".", ":(exclude)benchmarks/results"); statusErr == nil {
		dirty := strings.TrimSpace(status) != ""
		gitDirty = &dirty
	}

	// Never serialize the required version maps / concurrency as JSON null:
	// an empty {} / [] is the honest "collected, nothing to record" shape,
	// distinct from a field that was dropped.
	frameworkVersions := opts.FrameworkVersions
	if frameworkVersions == nil {
		frameworkVersions = map[string]string{}
	}
	runtimeVersions := opts.RuntimeVersions
	if runtimeVersions == nil {
		runtimeVersions = map[string]string{}
	}
	concurrency := opts.Concurrency
	if concurrency == nil {
		concurrency = []int{}
	}
	limitsByFramework := opts.ResourceLimitsByFramework
	if limitsByFramework == nil {
		limitsByFramework = map[string]string{}
	}

	m := Metadata{
		SchemaVersion: SchemaVersion,
		Timestamp:     now().UTC().Format(time.RFC3339),

		GitCommit: commit,
		GitDirty:  gitDirty,

		OS:          runtime.GOOS,
		Kernel:      kernel,
		Arch:        runtime.GOARCH,
		CPUModel:    cpuModel(),
		LogicalCPUs: runtime.NumCPU(),
		RAMBytes:    ramBytes(),

		GoVersion:            runtime.Version(),
		DockerVersion:        dockerVersion,
		DockerComposeVersion: composeVersion,
		PostgresVersion:      opts.PostgresVersion,

		FrameworkVersions: frameworkVersions,
		RuntimeVersions:   runtimeVersions,
		BenchmarkTool:     opts.BenchmarkTool,

		ResourceLimits:            opts.ResourceLimits,
		ResourceLimitsByFramework: limitsByFramework,
		PostgresResourceLimits:    opts.PostgresResourceLimits,
		DurationSeconds:           opts.DurationSeconds,
		WarmupSeconds:             opts.WarmupSeconds,
		Concurrency:               concurrency,
		Trials:                    opts.Trials,

		Groups: map[string]map[string]Provenance{},
	}
	// Collect deliberately does not file a unit entry. Stamping is a post-step
	// owned by whichever program wrote the rows (StampUnitFile), so Collect never
	// has to guess which unit a collection belongs to — and a collection that
	// measured nothing, like `make benchmark-metadata`, files nothing.
	return m
}

// Merge folds an incoming whole-snapshot collection into what earlier producers
// already wrote to metadata.json. A snapshot is written by several producers in
// turn — one run-crud invocation per app — so the last writer must not erase
// what the earlier ones recorded: the version maps and the per-framework
// applied-limit verdicts are unioned, a Postgres verdict this run did not
// re-check is preserved, and every group's provenance survives.
//
// A non-empty PostgresResourceLimits — including an explicit "unknown …" from a
// check that looked but could not classify — is authoritative for this run and
// overwrites, so a stale enforced/partial never sticks across a re-run whose
// check failed.
func Merge(existing, incoming Metadata) Metadata {
	incoming.FrameworkVersions = union(existing.FrameworkVersions, incoming.FrameworkVersions)
	incoming.RuntimeVersions = union(existing.RuntimeVersions, incoming.RuntimeVersions)
	incoming.ResourceLimitsByFramework = union(existing.ResourceLimitsByFramework, incoming.ResourceLimitsByFramework)
	incoming.Groups = mergeGroups(existing.Groups, incoming.Groups)
	if incoming.PostgresResourceLimits == "" {
		incoming.PostgresResourceLimits = existing.PostgresResourceLimits
	}
	return incoming
}

// StampUnit records prov as one unit's provenance in meta and changes nothing
// else — not the top-level block, not the shared run parameters, not another
// unit's entry, not even a sibling unit of the same group.
//
// That last restraint is the whole point. These data files merge row-wise and
// subset runs are supported, so a producer that re-measured one app must record
// exactly one app's provenance. Stamping the group instead would let it relabel
// rows it never touched (issue #266, review round 2).
func StampUnit(meta Metadata, group, unit string, prov Provenance) Metadata {
	meta.Groups = mergeGroups(meta.Groups, map[string]map[string]Provenance{group: {unit: prov}})
	return meta
}

// mergeGroups layers b over a, unit by unit: a unit is replaced only by a
// producer of that same unit, so merging never disturbs a sibling.
func mergeGroups(a, b map[string]map[string]Provenance) map[string]map[string]Provenance {
	out := make(map[string]map[string]Provenance, len(a)+len(b))
	for group, units := range a {
		copied := make(map[string]Provenance, len(units))
		for unit, p := range units {
			copied[unit] = p
		}
		out[group] = copied
	}
	for group, units := range b {
		if out[group] == nil {
			out[group] = make(map[string]Provenance, len(units))
		}
		for unit, p := range units {
			out[group][unit] = p
		}
	}
	return out
}

// union returns a new map with b's entries layered over a's.
func union(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func execRunner(ctx context.Context, name string, args ...string) (string, error) {
	// The command names are fixed literals at the call sites (git, uname,
	// docker), never user input — G204 does not apply.
	out, err := exec.CommandContext(ctx, name, args...).Output() //nolint:gosec
	return strings.TrimSpace(string(out)), err
}

// cpuModel and ramBytes read Linux's /proc directly (best-effort); on other
// platforms, or if the files are unreadable, they return the zero value. The
// parsing is split into pure functions so it's unit-tested without /proc.
func cpuModel() string {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	return parseCPUModel(string(data))
}

func ramBytes() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	return parseMemTotalBytes(string(data))
}

// parseCPUModel reads the CPU model from /proc/cpuinfo text. x86 uses
// "model name"; ARM/aarch64 typically has no such line, so it falls back to
// the devicetree "Model" and then "Hardware" keys. On a host that exposes
// none of them (a bare ARM /proc/cpuinfo with only "CPU part" hex codes) it
// returns "" — recorded as unknown rather than a fabricated string.
func parseCPUModel(cpuinfo string) string {
	found := map[string]string{}
	for _, line := range strings.Split(cpuinfo, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k := strings.TrimSpace(key)
		if _, seen := found[k]; !seen {
			found[k] = strings.TrimSpace(value)
		}
	}
	for _, key := range []string{"model name", "Model", "Hardware"} {
		if v := found[key]; v != "" {
			return v
		}
	}
	return ""
}

// parseMemTotalBytes reads the "MemTotal:" line of /proc/meminfo, which is in
// kibibytes ("MemTotal:  16311072 kB"), and returns bytes.
func parseMemTotalBytes(meminfo string) int64 {
	for _, line := range strings.Split(meminfo, "\n") {
		rest, ok := strings.CutPrefix(line, "MemTotal:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0
		}
		kib, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0
		}
		return kib * 1024
	}
	return 0
}
