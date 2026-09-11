// Command footprint records one implementation's operational-footprint row
// (issue #141 §"Operational footprint"). The measurement orchestration —
// timing container starts, sampling `docker stats`, weighing images/binaries —
// lives in benchmarks/scripts/footprint-all.sh; this command turns those raw
// numbers into a typed benchmarks/internal/footprint row and merges it into
// OUT_DIR/footprint.{json,csv} by (framework, variant), so re-measuring one
// implementation replaces only its own row.
//
//	go run ./benchmarks/scripts/footprint \
//	  -framework gombit -framework-version v0.1.3 -runtime go -runtime-version go1.26.0 \
//	  -variant container -cold-start-ms 210,235,228 \
//	  -idle-rss-bytes 18000000 -loaded-rss-bytes 42000000 -cpu-percent 165 \
//	  -image-size-bytes 30000000 -out benchmarks/results/latest/footprint.json
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gombit-dev/gombit/benchmarks/internal/footprint"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("footprint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		framework = fs.String("framework", "", "framework name / merge key (required)")
		fwVersion = fs.String("framework-version", "", "framework version")
		runtime   = fs.String("runtime", "", "runtime, e.g. go")
		rtVersion = fs.String("runtime-version", "", "runtime version")
		variant   = fs.String("variant", footprint.VariantContainer, "container | embedded")
		coldStart = fs.String("cold-start-ms", "", "comma-separated container-start→ready samples in ms")
		idleRSS   = fs.Int64("idle-rss-bytes", 0, "idle container memory (cgroup working set), bytes")
		loadedRSS = fs.Int64("loaded-rss-bytes", 0, "container memory under load, bytes")
		cpuPct    = fs.Float64("cpu-percent", 0, "CPU percent under load (docker stats, 100 = one core)")
		imageSize = fs.Int64("image-size-bytes", 0, "container image size, bytes")
		binSize   = fs.Int64("binary-size-bytes", 0, "deploy binary size, bytes (embedded-Gombit only)")
		out       = fs.String("out", "benchmarks/results/latest/footprint.json", "output footprint.json (a sibling .csv is written too)")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *framework == "" || *variant == "" {
		_, _ = fmt.Fprintln(stderr, "footprint: -framework and -variant are required")
		return 2
	}
	samples, err := parseFloats(*coldStart)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "footprint: -cold-start-ms: %v\n", err)
		return 2
	}

	row := footprint.Footprint{
		SchemaVersion:       footprint.SchemaVersion,
		Framework:           *framework,
		FrameworkVersion:    *fwVersion,
		Runtime:             *runtime,
		RuntimeVersion:      *rtVersion,
		Variant:             *variant,
		ColdStart:           footprint.ComputeColdStart(samples),
		IdleRSSBytes:        *idleRSS,
		LoadedRSSBytes:      *loadedRSS,
		CPUPercentUnderLoad: *cpuPct,
		ImageSizeBytes:      *imageSize,
		BinarySizeBytes:     *binSize,
	}

	if err := mergeIntoFiles(*out, row); err != nil {
		_, _ = fmt.Fprintf(stderr, "footprint: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "footprint: recorded %s/%s (cold-start median %.0fms over %d runs, idle %d B) into %s\n",
		row.Framework, row.Variant, row.ColdStart.MedianMs, row.ColdStart.Runs, row.IdleRSSBytes, *out)
	return 0
}

// mergeIntoFiles reads the existing footprint.json (if any), replaces this row's
// (framework, variant), and rewrites both the .json and its sibling .csv.
func mergeIntoFiles(jsonPath string, f footprint.Footprint) error {
	if jsonPath == "" {
		return fmt.Errorf("-out is required")
	}
	existing, err := readExisting(jsonPath)
	if err != nil {
		return err
	}
	merged := footprint.Merge(existing, []footprint.Footprint{f})

	if err := os.MkdirAll(filepath.Dir(jsonPath), 0o755); err != nil { //nolint:gosec // operator-supplied output dir
		return err
	}
	if err := writeFile(jsonPath, func(w io.Writer) error { return footprint.WriteJSON(w, merged) }); err != nil {
		return err
	}
	csvPath := strings.TrimSuffix(jsonPath, ".json") + ".csv"
	return writeFile(csvPath, func(w io.Writer) error { return footprint.WriteCSV(w, merged) })
}

func readExisting(path string) ([]footprint.Footprint, error) {
	file, err := os.Open(path) //nolint:gosec // operator-supplied output path
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return footprint.ReadJSON(file)
}

func writeFile(path string, encode func(io.Writer) error) error {
	f, err := os.Create(path) //nolint:gosec // operator-supplied output path
	if err != nil {
		return err
	}
	if err := encode(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// parseFloats parses a comma-separated list of non-negative floats (the
// cold-start samples). An empty string is an empty set (zero distribution).
func parseFloats(s string) ([]float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]float64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", p)
		}
		if v < 0 {
			return nil, fmt.Errorf("%q is negative", p)
		}
		out = append(out, v)
	}
	return out, nil
}
