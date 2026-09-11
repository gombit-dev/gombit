// Command collect-host-info writes the reproducibility metadata for a
// benchmark run (issue #141 "Reproducibility metadata") as JSON. The host and
// toolchain fields are discovered automatically; the run-parameter fields
// (durations, concurrency, trials, resource limits, tool versions) are passed
// as flags by the orchestrator that knows them.
//
//	go run ./benchmarks/scripts/collect-host-info -out benchmarks/results/latest/metadata.json
//
// With -group it switches to stamp mode: it records the current commit, host
// and toolchain as that measurement group's provenance in an existing
// metadata.json and changes nothing else. That is what lets a target which
// produces only one of the three groups — `make benchmark-micro`,
// `make benchmark-footprint` — say when and where its own numbers came from
// without restamping the hours-long CRUD sweep it shares the file with
// (issue #266):
//
//	go run ./benchmarks/scripts/collect-host-info -group microbench \
//	  -out benchmarks/results/latest/metadata.json
//
// Either way, per-group provenance already on disk survives: this command
// measures nothing itself, so it never deletes the record of measurements other
// targets did run.
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
	group := flag.String("group", "", "stamp this measurement group's provenance into an existing -out file, leaving every other field untouched ("+strings.Join(metadata.KnownGroups, "|")+")")
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

	// Fail closed on an unknown group: a typo'd -group would otherwise write a
	// phantom entry no reader looks at, leaving the table it meant to stamp
	// silently on the stale top-level fallback — the exact failure mode
	// per-group provenance exists to end.
	if *group != "" {
		if !metadata.ValidGroup(*group) {
			fatalf("-group %q: must be one of %s", *group, strings.Join(metadata.KnownGroups, ", "))
		}
		if *out == "" {
			fatalf("-group requires -out: stamping a group means updating an existing metadata.json in place")
		}
	}

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
		Group:             *group,
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

	// Whichever path runs, what is already on disk survives it: a group stamp
	// changes only its own group, and a whole-snapshot rewrite still carries
	// every group's provenance forward.
	switch {
	case *group != "":
		m, err = stampGroup(*out, *group, m)
	case *out != "":
		m, err = carryGroups(*out, m)
	}
	if err != nil {
		fatalf("%v", err)
	}

	if err := write(*out, m); err != nil {
		fatalf("%v", err)
	}
}

// stampGroup reduces a full collection to just its group entry, applied on top
// of the snapshot already at path. Everything else in that file — the top-level
// block, the CRUD run parameters, the other groups — is preserved verbatim, so
// a cheap single-group refresh cannot re-caption measurements it did not run.
func stampGroup(path, group string, collected metadata.Metadata) (metadata.Metadata, error) {
	existing, err := readSnapshot(path)
	if err != nil {
		return metadata.Metadata{}, err
	}
	return metadata.StampGroup(existing, group, collected.Provenance()), nil
}

// carryGroups preserves the per-group provenance already on disk across a
// whole-snapshot rewrite (the no -group path, `make benchmark-metadata`).
//
// collect-host-info measures nothing itself, so it has no standing to delete
// the record of measurements other targets did run. Dropping Groups here would
// not merely lose data: the report's fallback treats a group with no entry as
// "this snapshot predates per-group provenance, so the top-level block IS its
// provenance", which is only exact when one run produced everything. After a
// wipe that premise is false but the fallback still fires, re-captioning all
// three tables with the host and commit of a collection that measured nothing
// (issue #266).
//
// It deliberately preserves ONLY Groups. This target's existing behavior of
// replacing the version maps and limit verdicts is untouched here — changing
// that is a separate question from the provenance invariant.
func carryGroups(path string, collected metadata.Metadata) (metadata.Metadata, error) {
	existing, err := readSnapshot(path)
	if err != nil {
		return metadata.Metadata{}, err
	}
	for name, prov := range existing.Groups {
		collected = metadata.StampGroup(collected, name, prov)
	}
	return collected, nil
}

// readSnapshot returns the metadata.json already at path.
//
// A missing file is not an error: the first target to run in a fresh OUT_DIR
// starts a new record. A file that exists but does not parse IS an error —
// silently replacing a corrupt snapshot would discard whatever hours-long run
// produced it.
func readSnapshot(path string) (metadata.Metadata, error) {
	// path is the operator-supplied -out flag, not untrusted input — G304 does
	// not apply.
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		if os.IsNotExist(err) {
			return metadata.Metadata{SchemaVersion: metadata.SchemaVersion}, nil
		}
		return metadata.Metadata{}, fmt.Errorf("read %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	existing, err := metadata.ReadJSON(f)
	if err != nil {
		return metadata.Metadata{}, fmt.Errorf("read %s: %w", path, err)
	}
	return existing, nil
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
