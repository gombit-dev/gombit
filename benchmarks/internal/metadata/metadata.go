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
// costs — `make benchmark-micro` is seconds of pure Go, `benchmark-crud-all` is
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
// typo'd -group writes a phantom entry no reader ever looks at, instead of
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
// Groups does, per measurement group. It is the authoritative provenance: each
// group records the commit, host and toolchain ITS OWN data was produced at,
// and the report captions each table from it.
//
// The top-level discovered fields (Timestamp, GitCommit, GitDirty, the host
// block, GoVersion) describe the collection that wrote them — in practice the
// last whole-snapshot producer, which is the CRUD sweep. They are deliberately
// NOT a summary of the newest group, and a group-only stamp does not advance
// them (see StampGroup). Two consequences worth stating plainly:
//
//   - After a group-only refresh, the top-level GitCommit is OLDER than the
//     refreshed group's. That is correct, not stale bookkeeping: it still names
//     the state the whole-snapshot collection ran at, and re-pointing it at the
//     microbenchmark's commit would make the CRUD and footprint tables claim a
//     commit and host they never ran on — trading one wrong caption for another.
//   - Ask Groups, never the top level, when you want a specific table's
//     provenance. GroupProvenance does this for you, falling back to the top
//     level only for snapshots written before Groups existed, where the fallback
//     is exact because one run produced everything.
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

	// Groups maps a group name (Group*) to the provenance of that group's own
	// measurement. A group with no entry predates per-group stamping; readers
	// fall back to the top-level block, which for a single-run snapshot is
	// exactly that group's provenance. Empty ({}) rather than null so "no group
	// recorded" is a collected fact, not a dropped field.
	Groups map[string]Provenance `json:"groups"`
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

// GroupProvenance returns the provenance to attribute group's data to. A
// snapshot written before per-group stamping has no entry, and its top-level
// block *is* that group's provenance — one run produced the whole file — so the
// fallback is exact, not a guess.
func (m Metadata) GroupProvenance(group string) Provenance {
	if p, ok := m.Groups[group]; ok {
		return p
	}
	return m.Provenance()
}

// AnyDirty reports whether the top-level record or any group was measured on a
// dirty working tree. It fails loud: one group measured against uncommitted
// source is enough to make the whole snapshot unpublishable, since the reader
// cannot tell from a table which group it is looking at.
func (m Metadata) AnyDirty() bool {
	if m.GitDirty != nil && *m.GitDirty {
		return true
	}
	for _, p := range m.Groups {
		if p.GitDirty != nil && *p.GitDirty {
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

	// Group, when set to one of Group*, additionally files this collection's
	// provenance under that measurement group. Empty means the collection is
	// not attributed to any single group (the whole-snapshot path,
	// `make benchmark-metadata`).
	Group string

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

		Groups: map[string]Provenance{},
	}
	// The group entry is the same facts as the top-level block, filed under the
	// measurement it belongs to. Recording both keeps a group-aware reader exact
	// without breaking one that only knows the flat shape.
	if opts.Group != "" {
		m.Groups[opts.Group] = m.Provenance()
	}
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

// StampGroup records prov as group's provenance in meta and changes nothing
// else — not the top-level block, not the CRUD run parameters, not another
// group's entry. That restraint is the point of per-group provenance: the
// seconds-long microbenchmark refresh shares metadata.json with an hours-long
// CRUD sweep, and must be able to record its own commit without restamping, or
// silently re-attributing, measurements it did not run (issue #266).
func StampGroup(meta Metadata, group string, prov Provenance) Metadata {
	meta.Groups = mergeGroups(meta.Groups, map[string]Provenance{group: prov})
	return meta
}

// mergeGroups returns a new map with b's entries layered over a's: a group is
// replaced only by a producer of that same group.
func mergeGroups(a, b map[string]Provenance) map[string]Provenance {
	out := make(map[string]Provenance, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
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
