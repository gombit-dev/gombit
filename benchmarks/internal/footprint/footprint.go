// Package footprint is the schema and encoders for the operational-footprint
// half of the benchmark (issue #141 §"Operational footprint"): cold-start,
// idle/loaded memory, and CPU-under-load per implementation, plus the
// single-binary numbers (binary + image size) for the embedded-Gombit variant.
//
// It is deliberately separate from benchmarks/internal/result (throughput /
// latency): those rows answer "how fast", these answer "how heavy to deploy",
// and cold-start median/p95 has no natural home in a requests-per-second row.
// The measurement orchestration (Docker start timing, `docker stats` sampling)
// lives in benchmarks/scripts; this package owns only the schema, the
// cold-start statistics, and the JSON/CSV encoding, so those are unit-tested
// without Docker.
package footprint

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
)

// SchemaVersion is bumped on any breaking change to the row shape (issue §9).
const SchemaVersion = 1

// Variants of a measured artifact.
const (
	// VariantContainer is an app served from its normal container image.
	VariantContainer = "container"
	// VariantEmbedded is the Gombit single binary from `gombit build --embed`
	// (API + admin + frontend in one process) — the headline
	// single-binary-deployment number.
	VariantEmbedded = "embedded"
)

// ColdStart is the distribution of container-start → ready (first 200 on
// /livez) latency across repeated starts. Median and p95 are reported (issue
// requires "≥20 runs, median/p95"); Runs records how many samples backed them.
type ColdStart struct {
	MedianMs float64 `json:"median_ms"`
	P95Ms    float64 `json:"p95_ms"`
	Runs     int     `json:"runs"`
}

// Footprint is one implementation's operational-footprint row.
//
// Memory is the container's cgroup working set as reported by `docker stats`
// (usage minus reclaimable page cache) — the standard container-footprint proxy
// for RSS, recorded in bytes. BinarySizeBytes is set only for the embedded
// Gombit variant (a container app has no single deploy binary to weigh).
type Footprint struct {
	SchemaVersion       int       `json:"schema_version"`
	Framework           string    `json:"framework"`
	FrameworkVersion    string    `json:"framework_version"`
	Runtime             string    `json:"runtime"`
	RuntimeVersion      string    `json:"runtime_version"`
	Variant             string    `json:"variant"`
	ColdStart           ColdStart `json:"cold_start_ms"`
	IdleRSSBytes        int64     `json:"idle_rss_bytes"`
	LoadedRSSBytes      int64     `json:"loaded_rss_bytes"`
	CPUPercentUnderLoad float64   `json:"cpu_percent_under_load"`
	ImageSizeBytes      int64     `json:"image_size_bytes"`
	BinarySizeBytes     int64     `json:"binary_size_bytes,omitempty"`
}

// Key identifies a row for merge/replace: one implementation per variant.
func (f Footprint) Key() string { return f.Framework + "\x00" + f.Variant }

// ProvenanceUnit is this row's key in metadata.json's per-unit provenance: the
// same (framework, variant) pair Key() merges on, in a form readable in JSON.
//
// It must stay the merge key. Keying provenance on the framework alone would
// let a re-measured embedded binary relabel the container row it never touched —
// the row-merge-vs-table-provenance mismatch issue #266 exists to eliminate.
// Both the writer (scripts/footprint) and the reader (internal/report) derive
// the unit here so they cannot drift apart.
func (f Footprint) ProvenanceUnit() string { return f.Framework + ":" + f.Variant }

// ComputeColdStart returns the median, p95, and count of a set of cold-start
// samples (milliseconds). An empty set is a zero distribution.
func ComputeColdStart(samplesMs []float64) ColdStart {
	n := len(samplesMs)
	if n == 0 {
		return ColdStart{}
	}
	s := append([]float64(nil), samplesMs...)
	sort.Float64s(s)
	return ColdStart{
		MedianMs: percentile(s, 50),
		P95Ms:    percentile(s, 95),
		Runs:     n,
	}
}

// percentile returns the p-th percentile (0-100) of a sorted slice using linear
// interpolation between closest ranks. sorted must be non-empty.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 1 {
		return sorted[0]
	}
	rank := (p / 100) * float64(n-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo]
	}
	frac := rank - float64(lo)
	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}

// Merge returns existing with the given framework+variant rows replaced by
// incoming (rows for other keys are kept), so re-measuring one implementation
// updates only its own rows. The result is sorted deterministically.
func Merge(existing, incoming []Footprint) []Footprint {
	replaced := make(map[string]bool, len(incoming))
	for _, f := range incoming {
		replaced[f.Key()] = true
	}
	out := make([]Footprint, 0, len(existing)+len(incoming))
	for _, f := range existing {
		if !replaced[f.Key()] {
			out = append(out, f)
		}
	}
	out = append(out, incoming...)
	sortRows(out)
	return out
}

func sortRows(rows []Footprint) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Framework != rows[j].Framework {
			return rows[i].Framework < rows[j].Framework
		}
		return rows[i].Variant < rows[j].Variant
	})
}

// WriteJSON encodes rows as indented JSON (the canonical footprint output).
func WriteJSON(w io.Writer, rows []Footprint) error {
	sorted := append([]Footprint(nil), rows...)
	sortRows(sorted)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(sorted)
}

// ReadJSON decodes rows written by WriteJSON.
func ReadJSON(r io.Reader) ([]Footprint, error) {
	var rows []Footprint
	if err := json.NewDecoder(r).Decode(&rows); err != nil {
		return nil, fmt.Errorf("decode footprint json: %w", err)
	}
	return rows, nil
}

var csvHeader = []string{
	"framework", "framework_version", "runtime", "runtime_version", "variant",
	"cold_start_median_ms", "cold_start_p95_ms", "cold_start_runs",
	"idle_rss_bytes", "loaded_rss_bytes", "cpu_percent_under_load",
	"image_size_bytes", "binary_size_bytes",
}

// WriteCSV writes rows as CSV with a stable header and deterministic order.
func WriteCSV(w io.Writer, rows []Footprint) error {
	sorted := append([]Footprint(nil), rows...)
	sortRows(sorted)
	if _, err := fmt.Fprintln(w, joinCSV(csvHeader)); err != nil {
		return err
	}
	for _, f := range sorted {
		rec := []string{
			f.Framework, f.FrameworkVersion, f.Runtime, f.RuntimeVersion, f.Variant,
			formatFloat(f.ColdStart.MedianMs), formatFloat(f.ColdStart.P95Ms), strconv.Itoa(f.ColdStart.Runs),
			strconv.FormatInt(f.IdleRSSBytes, 10), strconv.FormatInt(f.LoadedRSSBytes, 10),
			formatFloat(f.CPUPercentUnderLoad),
			strconv.FormatInt(f.ImageSizeBytes, 10), strconv.FormatInt(f.BinarySizeBytes, 10),
		}
		if _, err := fmt.Fprintln(w, joinCSV(rec)); err != nil {
			return err
		}
	}
	return nil
}

func formatFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// joinCSV joins fields with commas, quoting any that need it. Footprint fields
// are simple (identifiers/numbers), but quote defensively for versions like
// "v0.1.3-59-g...".
func joinCSV(fields []string) string {
	out := make([]byte, 0, 64)
	for i, f := range fields {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, csvField(f)...)
	}
	return string(out)
}

func csvField(s string) string {
	needQuote := false
	for _, r := range s {
		if r == ',' || r == '"' || r == '\n' || r == '\r' {
			needQuote = true
			break
		}
	}
	if !needQuote {
		return s
	}
	q := make([]byte, 0, len(s)+2)
	q = append(q, '"')
	for _, r := range s {
		if r == '"' {
			q = append(q, '"')
		}
		q = append(q, string(r)...)
	}
	q = append(q, '"')
	return string(q)
}
