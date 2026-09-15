// Command collect-host-info writes the reproducibility metadata for a
// benchmark run (issue #141 "Reproducibility metadata") as JSON. The host and
// toolchain fields are discovered automatically; the run-parameter fields
// (durations, concurrency, trials, resource limits, tool versions) are passed
// as flags by the orchestrator that knows them.
//
//	go run ./benchmarks/scripts/collect-host-info -out benchmarks/results/latest/metadata.json
//
// It measures nothing itself, so it never files a per-unit provenance entry and
// never deletes one: per-unit provenance is written by the producers that write
// the rows (scripts/microbench, scripts/footprint, scripts/run-crud), and this
// command carries whatever they recorded across its whole-snapshot rewrite.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/gombit-dev/gombit/benchmarks/internal/metadata"
)

func main() {
	out := flag.String("out", "", "output file for metadata.json (default: stdout)")
	postgres := flag.String("postgres-version", "", "PostgreSQL version under test")
	benchmarkTool := flag.String("benchmark-tool", "", "load generator name+version, e.g. 'k6 0.55.0'")
	resourceLimits := flag.String("resource-limits", "", "documented resource limits for this run")
	duration := flag.Float64("duration-seconds", 0, "per-trial measured duration")
	warmup := flag.Float64("warmup-seconds", 0, "warm-up duration before measuring")
	concurrency := flag.String("concurrency", "", "comma-separated concurrency levels, e.g. '1,10,100'")
	trials := flag.Int("trials", 0, "number of trials per concurrency level")
	frameworkVersions := flag.String("framework-versions", "", "comma-separated framework=version pairs, e.g. 'gombit=v0.1.0,gin-gorm=v1.11.0'")
	runtimeVersions := flag.String("runtime-versions", "", "comma-separated runtime=version pairs, e.g. 'go=1.26.0,node=24'")
	flag.Parse()

	concurrencyLevels, err := parseIntList(*concurrency)
	if err != nil {
		fatalf("-concurrency: %v", err)
	}
	frameworks, err := parseKeyVals(*frameworkVersions)
	if err != nil {
		fatalf("-framework-versions: %v", err)
	}
	runtimes, err := parseKeyVals(*runtimeVersions)
	if err != nil {
		fatalf("-runtime-versions: %v", err)
	}

	m := metadata.Collect(context.Background(), metadata.Options{
		PostgresVersion:   *postgres,
		FrameworkVersions: frameworks,
		RuntimeVersions:   runtimes,
		BenchmarkTool:     *benchmarkTool,
		ResourceLimits:    *resourceLimits,
		DurationSeconds:   *duration,
		WarmupSeconds:     *warmup,
		Concurrency:       concurrencyLevels,
		Trials:            *trials,
	})

	if *out != "" {
		if m, err = carryGroups(*out, m); err != nil {
			fatalf("%v", err)
		}
	}

	if err := write(*out, m); err != nil {
		fatalf("%v", err)
	}
}

// carryGroups preserves the per-unit provenance already on disk across a
// whole-snapshot rewrite (`make benchmark-metadata`).
//
// collect-host-info measures nothing itself, so it has no standing to delete or
// replace the record of measurements other producers did run (issue #266):
//
//   - Every unit's entry in Groups is carried forward unchanged.
//   - If the snapshot on disk records no unit at all, it predates per-unit
//     provenance and its top-level block IS every row's provenance (see
//     metadata.Metadata.UnitProvenance). That block is carried forward too:
//     replacing it would re-caption every table with the commit and host of
//     this collection. Once any unit is recorded, the top-level block describes
//     no row, and this collection's own block is written as before.
//
// Nothing else is preserved. This target's existing behavior of replacing the
// version maps, limit verdicts and run parameters is untouched here — changing
// that is a separate question from the provenance invariant.
func carryGroups(path string, collected metadata.Metadata) (metadata.Metadata, error) {
	existing, err := metadata.ReadFile(path)
	if err != nil {
		return metadata.Metadata{}, err
	}
	if !existing.RecordsUnits() && !existing.Provenance().Empty() {
		collected = collected.WithProvenance(existing.Provenance())
	}
	for group, units := range existing.Groups {
		for unit, prov := range units {
			collected = metadata.StampUnit(collected, group, unit, prov)
		}
	}
	return collected, nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "collect-host-info: "+format+"\n", args...)
	os.Exit(1)
}

// write encodes m to path (or stdout when path is empty), checking the close
// error on the output file so a full-disk / short-write failure is not lost.
func write(path string, m metadata.Metadata) error {
	if path == "" {
		return metadata.WriteJSON(os.Stdout, m)
	}
	// path is the operator-supplied -out flag (a CLI writing its own output
	// file), not untrusted input — G304 does not apply.
	f, err := os.Create(path) //nolint:gosec
	if err != nil {
		return err
	}
	if err := metadata.WriteJSON(f, m); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// parseIntList parses a comma-separated integer list. A malformed token is a
// hard error, not a silently dropped element: this metadata records what a
// benchmark run actually swept, so "1,10,abc,100" must fail rather than be
// recorded as the different (and untrue) sweep "1,10,100".
func parseIntList(s string) ([]int, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		token := strings.TrimSpace(part)
		n, err := strconv.Atoi(token)
		if err != nil {
			return nil, fmt.Errorf("invalid integer %q", token)
		}
		out = append(out, n)
	}
	return out, nil
}

// parseKeyVals parses comma-separated key=value pairs into a map. Like
// parseIntList, it fails closed: a token with no '=' (a bare 'django'), an
// empty key, or a duplicate key is an error, not a silently dropped element —
// these are the required framework_versions / runtime_versions this metadata
// records, so a forgotten '=' must fail rather than omit the version that ran.
// Empty input yields an empty (non-nil) map so downstream JSON is {} not null.
func parseKeyVals(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		token := strings.TrimSpace(part)
		if token == "" {
			// A leading/trailing/doubled comma is malformed input for a
			// record of exactly what ran, not a value to skip.
			if strings.TrimSpace(s) == "" {
				continue // wholly empty input -> empty map, no pairs
			}
			return nil, fmt.Errorf("empty key=value pair in %q", s)
		}
		key, value, ok := strings.Cut(token, "=")
		if !ok {
			return nil, fmt.Errorf("missing '=' in %q", token)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("empty key in %q", token)
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		out[key] = strings.TrimSpace(value)
	}
	return out, nil
}
