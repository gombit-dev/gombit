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

// k6Runner runs the workload once at vus concurrency for duration; a non-empty
// summaryPath is where the run's summary must be written (an empty one is a
// warm-up whose output is discarded). Injectable so run() is testable without
// docker or a live app.
type k6Runner func(vus int, duration, summaryPath string) error

func main() {
	var (
		cfg      runConfig
		workload = flag.String("workload", "benchmarks/workloads/crud-list.js", "k6 workload script")
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
		Benchmark:        "crud-list",
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

	meta := metadata.Collect(context.Background(), metadata.Options{
		// This is the CRUD sweep's own provenance: the commit and host the
		// containerized load test ran at, which the microbenchmark and footprint
		// groups sharing metadata.json are free to differ from (issue #266).
		Group:             metadata.GroupCRUD,
		PostgresVersion:   cfg.postgresVersion,
		FrameworkVersions: map[string]string{cfg.framework: cfg.frameworkVersion},
		RuntimeVersions:   map[string]string{cfg.runtimeName: cfg.runtimeVersion},
		// The actual load-generator image that ran, not a bare "k6" token —
		// issue #141's reproducibility metadata requires the benchmark-tool
		// version, and overriding -k6-image must be reflected here.
		BenchmarkTool: cfg.k6Image,
		// The scalar stays this app's verdict for back-compat; the authoritative,
		// merge-preserved field is the per-framework map, so a partial/not-applied
		// on one app is never overwritten by the next app's enforced (mergedMetadata
		// unions it like the version maps). Postgres is the shared DB container's
		// verdict, recorded once for the snapshot.
		ResourceLimits:            cfg.resourceLimits,
		ResourceLimitsByFramework: map[string]string{cfg.framework: cfg.resourceLimits},
		PostgresResourceLimits:    cfg.postgresResourceLimits,
		DurationSeconds:           durationSeconds(cfg.duration),
		WarmupSeconds:             durationSeconds(cfg.warmup),
		Concurrency:               cfg.concurrency,
		Trials:                    cfg.trials,
	})
	if err := writeOutputs(cfg.outDir, cfg.framework, rows, meta); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "run-crud: merged %d %s rows into %s\n", len(rows), cfg.framework, cfg.outDir)
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
			"-v", workloadAbs + ":/workload/crud-list.js:ro",
		}
		if summaryPath != "" {
			args = append(args,
				"-e", "SUMMARY_OUT=/out/summary.json",
				"-v", filepath.Dir(summaryPath)+":/out",
			)
		}
		args = append(args, image, "run", "--quiet", "/workload/crud-list.js")

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
// truncating it: re-running one framework replaces that framework's rows,
// while running each framework in turn accumulates all six. metadata's version
// maps are unioned the same way so a multi-framework snapshot records every
// implementation that contributed.
func writeOutputs(outDir, framework string, newRows []result.Result, meta metadata.Metadata) error {
	rows, err := mergedResults(filepath.Join(outDir, "results.json"), newRows, framework)
	if err != nil {
		return err
	}
	meta = mergedMetadata(filepath.Join(outDir, "metadata.json"), meta)

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

func mergedResults(path string, newRows []result.Result, framework string) ([]result.Result, error) {
	existing, err := readResults(path)
	if err != nil {
		return nil, err
	}
	return mergeRows(existing, newRows, framework), nil
}

// mergeRows drops any existing rows for framework (a re-run replaces them) and
// appends the new ones; rows for other frameworks are kept.
func mergeRows(existing, newRows []result.Result, framework string) []result.Result {
	merged := make([]result.Result, 0, len(existing)+len(newRows))
	for _, r := range existing {
		if r.Framework != framework {
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

// mergedMetadata folds this run's metadata into whatever an earlier app's run
// already wrote, preserving every app's contribution (metadata.Merge documents
// the per-field rules). An unreadable or corrupt file is treated as "no prior
// snapshot": this run's own record still gets written.
func mergedMetadata(path string, meta metadata.Metadata) metadata.Metadata {
	f, err := os.Open(path) //nolint:gosec // path composed from the operator-supplied out-dir
	if err != nil {
		return meta
	}
	defer func() { _ = f.Close() }()
	existing, err := metadata.ReadJSON(f)
	if err != nil {
		return meta
	}
	return metadata.Merge(existing, meta)
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

// durationSeconds converts a k6 duration string ("30s", "1m30s") to seconds
// for the metadata fields; unparseable input is 0.
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
