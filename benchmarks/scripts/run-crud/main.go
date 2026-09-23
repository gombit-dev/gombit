// Command run-crud runs the headline CRUD-read workload
// (benchmarks/workloads/crud-list.js) against one already-running,
// already-seeded implementation and merges its rows into a results snapshot.
//
// It drives the pinned k6 image (the load generator runs in a container on the
// host network, on the SAME machine as the app — the issue's "another
// container on the same host" topology, recorded as such), warms up with a
// discarded run, then measures TRIALS times at each concurrency level, parses
// and validates each summary, and writes results.json/results.csv plus
// metadata.json. A failed k6 run or an invalid summary (no traffic, HTTP
// errors, failed content checks) fails the command loudly with nothing written
// (issue #141 §10).
//
// It does NOT start or resource-constrain the app; run standalone, the recorded
// resource_limits therefore says so honestly. This is the per-implementation
// engine: `make benchmark-crud-all` (benchmarks/scripts/run-crud-all.sh) brings
// all six apps up under compose with the §7 limits, records each container's
// applied-limit classification, and loops this over them.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gombit-dev/gombit/benchmarks/internal/k6"
	"github.com/gombit-dev/gombit/benchmarks/internal/metadata"
	"github.com/gombit-dev/gombit/benchmarks/internal/result"
)

const resourceLimitsNotApplied = "not applied (run-crud does not start or constrain the app; pass -resource-limits with an inspect-limits verdict to record a verified ceiling)"

type runConfig struct {
	targetURL              string
	framework              string
	benchmark              string
	frameworkVersion       string
	runtimeName            string
	runtimeVersion         string
	concurrency            []int
	duration               string
	warmup                 string
	trials                 int
	outDir                 string
	postgresVersion        string
	resourceLimits         string
	postgresResourceLimits string
	k6Image                string
}

// runParams is the protocol and load generator this run records at the top level
// of metadata.json. metadata.Collect is fed from it, and so is the merge guard,
// so what is compared can never differ from what is written.
func (c runConfig) runParams() metadata.RunParams {
	return metadata.RunParams{
		Concurrency:     c.concurrency,
		Trials:          c.trials,
		DurationSeconds: durationSeconds(c.duration),
		WarmupSeconds:   durationSeconds(c.warmup),
		// The actual load-generator image that ran, not a bare "k6" token —
		// issue #141's reproducibility metadata requires the benchmark-tool
		// version, and overriding -k6-image must be reflected here.
		BenchmarkTool: c.k6Image,
	}
}

// validateRunParams rejects a run whose recorded parameters would be incomplete
// or invalid. metadata.Merge writes every one of them whole, and
// RunParams.ConflictsWith compares them as values, so an incoming zero must be
// a real choice, never a flag that was left at zero or a string that failed to
// parse: `-trials 0` used to pass the merge guard unchecked, measure nothing,
// delete this unit's own rows, and record "0 trials" over rows measured with
// five (#361 review round 2). A zero-second warm-up is a legitimate choice and
// is accepted as a value.
func (c runConfig) validateRunParams() error {
	if c.trials < 1 {
		return fmt.Errorf("-trials must be at least 1, got %d", c.trials)
	}
	if len(c.concurrency) == 0 {
		return fmt.Errorf("-concurrency must list at least one level")
	}
	for _, vus := range c.concurrency {
		if vus < 1 {
			return fmt.Errorf("-concurrency levels must be at least 1, got %d", vus)
		}
	}
	if d, err := time.ParseDuration(c.duration); err != nil || d <= 0 {
		return fmt.Errorf("-duration must be a positive k6 duration, got %q", c.duration)
	}
	if d, err := time.ParseDuration(c.warmup); err != nil || d < 0 {
		return fmt.Errorf("-warmup must be a k6 duration of zero or more, got %q", c.warmup)
	}
	if c.k6Image == "" {
		return fmt.Errorf("-k6-image must name the load generator image")
	}
	return nil
}

// k6Runner runs the workload once at vus concurrency for duration; a non-empty
// summaryPath is where the run's summary must be written (an empty one is a
// warm-up whose output is discarded). Injectable so run() is testable without
// docker or a live app.
type k6Runner func(vus int, duration, summaryPath string) error

func main() {
	var (
		cfg      runConfig
		workload = flag.String("workload", "benchmarks/workloads/crud-list.js", "k6 workload script; its file name without .js is recorded as the rows' benchmark (the README CRUD table publishes crud-list)")
		conc     = flag.String("concurrency", "1,10,100", "comma-separated concurrency levels (VUs)")
	)
	flag.StringVar(&cfg.k6Image, "k6-image", "grafana/k6:0.55.0", "pinned k6 image (the load generator); recorded as benchmark_tool")
	flag.StringVar(&cfg.targetURL, "target-url", "", "the app's GET list endpoint, e.g. http://127.0.0.1:8081/api/projects?page=1&limit=20")
	flag.StringVar(&cfg.framework, "framework", "", "framework name for the result rows, e.g. gin-gorm")
	flag.StringVar(&cfg.frameworkVersion, "framework-version", "", "framework version")
	flag.StringVar(&cfg.runtimeName, "runtime", "", "runtime, e.g. go")
	flag.StringVar(&cfg.runtimeVersion, "runtime-version", "", "runtime version")
	flag.StringVar(&cfg.duration, "duration", "30s", "measured per-trial duration (k6 duration string)")
	flag.StringVar(&cfg.warmup, "warmup", "10s", "warm-up duration, discarded")
	flag.IntVar(&cfg.trials, "trials", 5, "measured trials per concurrency level")
	flag.StringVar(&cfg.outDir, "out-dir", "benchmarks/results/latest", "output directory")
	flag.StringVar(&cfg.postgresVersion, "postgres-version", "", "PostgreSQL version, for metadata")
	flag.StringVar(&cfg.resourceLimits, "resource-limits", resourceLimitsNotApplied,
		"this app's applied-limit verdict, recorded per framework; defaults to an honest 'not applied' since this command does not constrain the app")
	flag.StringVar(&cfg.postgresResourceLimits, "postgres-resource-limits", "",
		"the database container's applied-limit verdict (from inspect-limits), recorded once for the snapshot")
	flag.Parse()

	if cfg.targetURL == "" || cfg.framework == "" {
		fatalf("-target-url and -framework are required")
	}
	levels, err := parseIntList(*conc)
	if err != nil {
		fatalf("-concurrency: %v", err)
	}
	if len(levels) == 0 {
		fatalf("-concurrency must list at least one level")
	}
	cfg.concurrency = levels

	cfg.benchmark, err = benchmarkName(*workload)
	if err != nil {
		fatalf("-workload: %v", err)
	}
	workloadAbs, err := filepath.Abs(*workload)
	if err != nil {
		fatalf("resolve workload path: %v", err)
	}
	cfg.outDir, err = filepath.Abs(cfg.outDir)
	if err != nil {
		fatalf("resolve out dir: %v", err)
	}

	if err := run(cfg, dockerK6Runner(cfg.k6Image, workloadAbs, cfg.targetURL)); err != nil {
		fatalf("%v", err)
	}
}

// run executes the sweep and, only if every trial is a clean measurement,
// merges the rows into the snapshot. On any failure it returns an error with
// nothing written — a failed implementation must not leave a partial or bogus
// snapshot behind.
func run(cfg runConfig, k6run k6Runner) error {
	if cfg.benchmark == "" {
		return fmt.Errorf("no benchmark name: rows cannot be merged or attributed without one")
	}
	if err := cfg.validateRunParams(); err != nil {
		return err
	}
	// Refuse an incompatible merge before hours of measurement, not after: every
	// input to the check is known now. writeOutputs checks again on the snapshot it
	// actually merges into (see refuseIncompatible).
	before, err := readSnapshot(cfg.outDir)
	if err != nil {
		return err
	}
	if err := refuseIncompatible(before, cfg.outDir, cfg.framework, cfg.benchmark, cfg.runParams()); err != nil {
		return err
	}
	rawDir := filepath.Join(cfg.outDir, "raw")
	if err := os.MkdirAll(rawDir, 0o750); err != nil {
		return fmt.Errorf("create out dir: %w", err)
	}

	base := result.Result{
		SchemaVersion:    result.SchemaVersion,
		Framework:        cfg.framework,
		FrameworkVersion: cfg.frameworkVersion,
		Runtime:          cfg.runtimeName,
		RuntimeVersion:   cfg.runtimeVersion,
		Benchmark:        cfg.benchmark,
		Database:         "postgresql",
	}

	var rows []result.Result
	for _, vus := range cfg.concurrency {
		if err := k6run(vus, cfg.warmup, ""); err != nil {
			return fmt.Errorf("warm-up (vus=%d): %w", vus, err)
		}
		for trial := 1; trial <= cfg.trials; trial++ {
			summaryPath := filepath.Join(rawDir, fmt.Sprintf("%s_c%d_t%d.json", cfg.framework, vus, trial))
			if err := k6run(vus, cfg.duration, summaryPath); err != nil {
				return fmt.Errorf("k6 run (vus=%d trial=%d): %w", vus, trial, err)
			}
			summary, err := parseSummaryFile(summaryPath)
			if err != nil {
				return fmt.Errorf("parse summary (vus=%d trial=%d): %w", vus, trial, err)
			}
			if err := summary.Validate(); err != nil {
				return fmt.Errorf("%s vus=%d trial=%d: %w", cfg.framework, vus, trial, err)
			}
			row := base
			row.Concurrency = vus
			row.Trial = trial
			rows = append(rows, summary.Merge(row))
			fmt.Fprintf(os.Stderr, "run-crud: %s vus=%d trial=%d -> %.0f rps, p95=%.1fms, errors=%d\n",
				cfg.framework, vus, trial, summary.RequestsPerSecond, summary.LatencyMs.P95, summary.Errors)
		}
	}

	params := cfg.runParams()
	meta := metadata.Collect(context.Background(), metadata.Options{
		PostgresVersion:   cfg.postgresVersion,
		FrameworkVersions: map[string]string{cfg.framework: cfg.frameworkVersion},
		RuntimeVersions:   map[string]string{cfg.runtimeName: cfg.runtimeVersion},
		BenchmarkTool:     params.BenchmarkTool,
		// The scalar stays this app's verdict for back-compat; the authoritative,
		// merge-preserved field is the per-framework map, so a partial/not-applied
		// on one app is never overwritten by the next app's enforced (metadata.Merge
		// unions it like the version maps). Postgres is the shared DB container's
		// verdict, recorded once for the snapshot.
		ResourceLimits:            cfg.resourceLimits,
		ResourceLimitsByFramework: map[string]string{cfg.framework: cfg.resourceLimits},
		PostgresResourceLimits:    cfg.postgresResourceLimits,
		DurationSeconds:           params.DurationSeconds,
		WarmupSeconds:             params.WarmupSeconds,
		Concurrency:               params.Concurrency,
		Trials:                    params.Trials,
	})
	// This run's own provenance, filed under this (app, workload) alone. run-crud
	// replaces exactly those rows and preserves the others, and APPS= subsetting
	// is a supported run, so stamping the whole crud group here would caption
	// every other app's rows with a commit they never ran at (issue #266, round
	// 2), and stamping the app alone would do the same to its other workloads
	// (#361).
	meta = metadata.StampUnit(meta, metadata.GroupCRUD, result.ProvenanceUnit(cfg.framework, cfg.benchmark), meta.Provenance())

	if err := writeOutputs(cfg.outDir, cfg.framework, cfg.benchmark, rows, meta); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "run-crud: merged %d %s %s rows into %s\n", len(rows), cfg.framework, cfg.benchmark, cfg.outDir)
	return nil
}

// dockerK6Runner returns a k6Runner that runs the pinned k6 image against
// targetURL. --network host lets the containerized load generator reach an app
// listening on the host; --user runs k6 as the invoking uid so the summary it
// writes into the mounted output dir is ours.
func dockerK6Runner(image, workloadAbs, targetURL string) k6Runner {
	return func(vus int, duration, summaryPath string) error {
		args := []string{
			"run", "--rm", "--network", "host",
			"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
			"-e", "TARGET_URL=" + targetURL,
			"-e", "VUS=" + strconv.Itoa(vus),
			"-e", "DURATION=" + duration,
			"-v", workloadAbs + ":/workload/script.js:ro",
		}
		if summaryPath != "" {
			args = append(args,
				"-e", "SUMMARY_OUT=/out/summary.json",
				"-v", filepath.Dir(summaryPath)+":/out",
			)
		}
		args = append(args, image, "run", "--quiet", "/workload/script.js")

		cmd := exec.CommandContext(context.Background(), "docker", args...) //nolint:gosec // fixed argv, operator-supplied target only
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		if summaryPath != "" {
			if err := os.Rename(filepath.Join(filepath.Dir(summaryPath), "summary.json"), summaryPath); err != nil {
				return fmt.Errorf("k6 produced no summary: %w", err)
			}
		}
		return nil
	}
}

func parseSummaryFile(path string) (k6.Summary, error) {
	f, err := os.Open(path) //nolint:gosec // path composed from the operator-supplied out-dir
	if err != nil {
		return k6.Summary{}, err
	}
	defer func() { _ = f.Close() }()
	return k6.ParseSummary(f)
}

// writeOutputs merges this run's rows into any existing snapshot rather than
// truncating it: re-running one framework's workload replaces those rows,
// while running each framework (or workload) in turn accumulates them all.
// metadata's version maps are unioned the same way so a multi-framework
// snapshot records every implementation that contributed.
func writeOutputs(outDir, framework, benchmark string, newRows []result.Result, meta metadata.Metadata) error {
	// Read once, before writing anything, and both check and merge that same
	// snapshot, so an unreadable snapshot or a refused merge fails the run with
	// the files untouched, and what was checked is what is merged.
	snap, err := readSnapshot(outDir)
	if err != nil {
		return err
	}
	if err := refuseIncompatible(snap, outDir, framework, benchmark, meta.RunParams()); err != nil {
		return err
	}
	rows := mergeRows(snap.rows, newRows, framework, benchmark)
	meta = metadata.Merge(snap.meta, meta)

	if err := writeFile(filepath.Join(outDir, "results.json"), func(f *os.File) error {
		return result.WriteJSON(f, rows)
	}); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(outDir, "results.csv"), func(f *os.File) error {
		return result.WriteCSV(f, rows)
	}); err != nil {
		return err
	}
	return writeFile(filepath.Join(outDir, "metadata.json"), func(f *os.File) error {
		return metadata.WriteJSON(f, meta)
	})
}

// snapshot is an OUT_DIR's results and metadata as read at one point in time.
type snapshot struct {
	rows []result.Result
	meta metadata.Metadata
}

// readSnapshot reads the snapshot a run merges into. A missing file starts the
// record; an unreadable or corrupt one is an error. Treating a corrupt
// metadata.json as "no prior snapshot" would overwrite every other app's
// versions and limit verdicts and every unit's provenance — the microbench and
// footprint groups included — with this one app's record.
func readSnapshot(outDir string) (snapshot, error) {
	rows, err := readResults(filepath.Join(outDir, "results.json"))
	if err != nil {
		return snapshot{}, err
	}
	meta, err := metadata.ReadFile(filepath.Join(outDir, "metadata.json"))
	if err != nil {
		return snapshot{}, err
	}
	return snapshot{rows: rows, meta: meta}, nil
}

// refuseIncompatible refuses a run whose protocol or load generator differs from
// what snap records while rows this run will not replace remain in it.
//
// The rows are per (framework, benchmark) and so is their provenance, but the
// protocol and load generator are recorded once for the whole snapshot, and
// metadata.Merge writes the incoming values whole. Merging such a run would
// leave the README describing the surviving rows — another app's, or another
// workload's — with parameters they were not measured under, and nothing would
// say so (#361 review round 1; the framework axis has had the same gap since
// run-crud began merging). Incoming parameters are complete by the time they
// get here (validateRunParams), so every incoming field is compared as a value.
//
// run() checks the snapshot as it stands before the sweep, so a doomed run
// fails before hours of measurement. writeOutputs checks the snapshot it then
// merges into, which catches one another producer rewrote while this run was
// measuring. Neither is concurrency control: no producer locks OUT_DIR, and a
// write that overlaps this run's own read-merge-write can still interleave with
// it. Running two producers against one OUT_DIR at the same time is
// unsupported, as it was before this check existed. raw/ summaries are written
// during the sweep, before either check can refuse the merge.
//
// It needs no per-unit protocol record: the state it forbids is the one that
// could not be described honestly. A run that replaces every row in the file,
// a fresh out dir, or a snapshot that records no parameters has nothing to
// misdescribe and passes. There is no incremental way to adopt a new protocol
// on a populated snapshot (each app's run is refused while the others' rows
// remain), so the way out is a separate OUT_DIR or starting the snapshot over.
func refuseIncompatible(snap snapshot, outDir, framework, benchmark string, params metadata.RunParams) error {
	kept := 0
	for _, r := range snap.rows {
		// The exact negation of what mergeRows replaces.
		if r.Framework != framework || r.Benchmark != benchmark {
			kept++
		}
	}
	if kept == 0 {
		return nil
	}
	diffs := snap.meta.RunParams().ConflictsWith(params)
	if len(diffs) == 0 {
		return nil
	}
	return fmt.Errorf("refusing to record %s:%s under different parameters from the snapshot in %s (%s): "+
		"the %d rows it would keep would then be reported under parameters they were not measured with. "+
		"Write to a separate OUT_DIR, or remove the old snapshot to start over under the new parameters "+
		"(the other rows cannot be re-measured first: each of their runs is refused the same way). "+
		"results.json, results.csv and metadata.json were not modified",
		framework, benchmark, outDir, strings.Join(diffs, "; "), kept)
}

// mergeRows drops any existing rows for (framework, benchmark) (a re-run
// replaces them) and appends the new ones; rows for other frameworks, and for
// this framework's other workloads, are kept. The key must match the provenance
// unit run() stamps (result.ProvenanceUnit), or recording one workload would
// relabel another's rows (#361).
func mergeRows(existing, newRows []result.Result, framework, benchmark string) []result.Result {
	merged := make([]result.Result, 0, len(existing)+len(newRows))
	for _, r := range existing {
		if r.Framework != framework || r.Benchmark != benchmark {
			merged = append(merged, r)
		}
	}
	return append(merged, newRows...)
}

func readResults(path string) ([]result.Result, error) {
	f, err := os.Open(path) //nolint:gosec // path composed from the operator-supplied out-dir
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return result.ReadJSON(f)
}

func writeFile(path string, encode func(*os.File) error) error {
	f, err := os.Create(path) //nolint:gosec // path composed from operator-supplied out-dir
	if err != nil {
		return err
	}
	if err := encode(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// benchmarkName derives a row's benchmark from the workload script that ran:
// benchmarks/workloads/<benchmark>.js. A row labelled with a constant instead
// would let a different workload replace, and be published as, crud-list (#361).
// ":" is rejected because it separates framework from benchmark in the
// provenance unit.
func benchmarkName(workloadPath string) (string, error) {
	name := strings.TrimSuffix(filepath.Base(workloadPath), ".js")
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "", fmt.Errorf("cannot derive a benchmark name from %q", workloadPath)
	}
	if strings.Contains(name, ":") {
		return "", fmt.Errorf("benchmark name %q must not contain ':'", name)
	}
	return name, nil
}

// durationSeconds converts a k6 duration string ("30s", "1m30s") to seconds
// for the metadata fields. Callers pass input validateRunParams accepted; an
// unparseable string would be 0, which is why that validation exists.
func durationSeconds(s string) float64 {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d.Seconds()
}

func parseIntList(s string) ([]int, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("invalid integer %q", strings.TrimSpace(part))
		}
		out = append(out, n)
	}
	return out, nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "run-crud: "+format+"\n", args...)
	os.Exit(1)
}
