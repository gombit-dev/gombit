package jobs

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Outcomes a worker records for a delivery (the `result` metric label).
const (
	ResultSucceeded   = "succeeded"   // acknowledged
	ResultRetried     = "retried"     // failed, released for another attempt
	ResultFailed      = "failed"      // given up on, kept with the failed jobs
	ResultInterrupted = "interrupted" // canceled by the worker's shutdown, released
	ResultAbandoned   = "abandoned"   // lease lost to another worker
	ResultUndecodable = "undecodable" // stored envelope does not decode, set aside
)

// unknownJobLabel is the job label of a job no handler knows (or an
// undecodable one), so an envelope's name cannot mint unbounded series.
const unknownJobLabel = "unknown"

// Metrics accumulates what a worker does, for Prometheus. The job path only
// does atomic adds on per-series counters (sync.Map lookups after warmup), so
// recording takes no lock. Labels are bounded: job names come from the
// registry (unknown ones collapse), queues from the worker's configuration.
type Metrics struct {
	inFlight sync.Map // queue -> *atomic.Int64
	series   sync.Map // metricsSeries -> *jobCounter
}

type metricsSeries struct {
	job, queue, result string
}

type jobCounter struct {
	count atomic.Int64
	run   histogram
	wait  histogram
}

// latencyBuckets are the histogram upper bounds, in seconds: job handlers
// and queue waits range from milliseconds to minutes.
var latencyBuckets = []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60, 300, 1800}

// histogram is a lock-free Prometheus histogram over latencyBuckets.
type histogram struct {
	buckets [11]atomic.Int64 // len(latencyBuckets); cumulative on render
	count   atomic.Int64
	nanos   atomic.Int64
}

func (h *histogram) observe(d time.Duration) {
	secs := d.Seconds()
	for i, bound := range latencyBuckets {
		if secs <= bound {
			h.buckets[i].Add(1)
			break
		}
	}
	h.count.Add(1)
	h.nanos.Add(int64(d))
}

// NewMetrics returns empty job metrics.
func NewMetrics() *Metrics { return &Metrics{} }

func (m *Metrics) inFlightFor(queue string) *atomic.Int64 {
	if v, ok := m.inFlight.Load(queue); ok {
		return v.(*atomic.Int64)
	}
	v, _ := m.inFlight.LoadOrStore(queue, new(atomic.Int64))
	return v.(*atomic.Int64)
}

func (m *Metrics) started(queue string)  { m.inFlightFor(queue).Add(1) }
func (m *Metrics) finished(queue string) { m.inFlightFor(queue).Add(-1) }

// record counts one delivery's outcome, how long its handler ran, and how
// long it waited in the queue.
func (m *Metrics) record(job, queue, result string, run, wait time.Duration) {
	key := metricsSeries{job: job, queue: queue, result: result}
	v, ok := m.series.Load(key)
	if !ok {
		v, _ = m.series.LoadOrStore(key, &jobCounter{})
	}
	c := v.(*jobCounter)
	c.count.Add(1)
	c.run.observe(run)
	// A wait is only known when the queue said when the job became
	// available and the clocks agree; an unknown one is not counted at all
	// rather than as zero.
	if wait > 0 {
		c.wait.observe(wait)
	}
}

// Empty reports whether nothing has been recorded.
func (m *Metrics) Empty() bool {
	empty := true
	m.series.Range(func(_, _ any) bool { empty = false; return false })
	if empty {
		m.inFlight.Range(func(_, _ any) bool { empty = false; return false })
	}
	return empty
}

// WritePrometheus writes the metrics in the Prometheus text format. stats,
// when given, adds each queue's depth by state.
//
//	gombit_jobs_processed_total{job,queue,result}   deliveries by outcome
//	gombit_jobs_run_seconds{job,queue}              handler time (histogram)
//	gombit_jobs_wait_seconds{job,queue}             time from available to started (histogram)
//	gombit_jobs_in_flight{queue}                    running now
//	gombit_jobs_queued{queue,state}                 ready, scheduled, reserved, failed
//	gombit_jobs_oldest_ready_seconds{queue}         age of the longest-waiting ready job
func (m *Metrics) WritePrometheus(w io.Writer, stats map[string]QueueStats, now time.Time) error {
	type row struct {
		key metricsSeries
		c   *jobCounter
	}
	var rows []row
	m.series.Range(func(k, v any) bool {
		rows = append(rows, row{k.(metricsSeries), v.(*jobCounter)})
		return true
	})
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i].key, rows[j].key
		if a.job != b.job {
			return a.job < b.job
		}
		if a.queue != b.queue {
			return a.queue < b.queue
		}
		return a.result < b.result
	})
	var b strings.Builder
	if len(rows) > 0 {
		b.WriteString("# HELP gombit_jobs_processed_total Job deliveries by outcome.\n# TYPE gombit_jobs_processed_total counter\n")
		for _, r := range rows {
			fmt.Fprintf(&b, "gombit_jobs_processed_total{job=%q,queue=%q,result=%q} %d\n", r.key.job, r.key.queue, r.key.result, r.c.count.Load())
		}
		// Durations aggregate over outcomes, per job and queue.
		type agg struct {
			run, wait           [11]int64
			runN, waitN         int64
			runNanos, waitNanos int64
		}
		totals := map[[2]string]*agg{}
		var order [][2]string
		for _, r := range rows {
			k := [2]string{r.key.job, r.key.queue}
			a, ok := totals[k]
			if !ok {
				a = &agg{}
				totals[k] = a
				order = append(order, k)
			}
			for i := range latencyBuckets {
				a.run[i] += r.c.run.buckets[i].Load()
				a.wait[i] += r.c.wait.buckets[i].Load()
			}
			a.runN += r.c.run.count.Load()
			a.waitN += r.c.wait.count.Load()
			a.runNanos += r.c.run.nanos.Load()
			a.waitNanos += r.c.wait.nanos.Load()
		}
		writeHist := func(name, help string, pick func(*agg) ([11]int64, int64, int64)) {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
			for _, k := range order {
				buckets, n, nanos := pick(totals[k])
				cumulative := int64(0)
				for i, bound := range latencyBuckets {
					cumulative += buckets[i]
					fmt.Fprintf(&b, "%s_bucket{job=%q,queue=%q,le=%q} %d\n", name, k[0], k[1], strconv.FormatFloat(bound, 'g', -1, 64), cumulative)
				}
				fmt.Fprintf(&b, "%s_bucket{job=%q,queue=%q,le=\"+Inf\"} %d\n", name, k[0], k[1], n)
				fmt.Fprintf(&b, "%s_sum{job=%q,queue=%q} %g\n", name, k[0], k[1], time.Duration(nanos).Seconds())
				fmt.Fprintf(&b, "%s_count{job=%q,queue=%q} %d\n", name, k[0], k[1], n)
			}
		}
		writeHist("gombit_jobs_run_seconds", "Time job handlers ran.", func(a *agg) ([11]int64, int64, int64) { return a.run, a.runN, a.runNanos })
		writeHist("gombit_jobs_wait_seconds", "Time jobs waited in the queue after becoming available.", func(a *agg) ([11]int64, int64, int64) { return a.wait, a.waitN, a.waitNanos })
	}
	var queues []string
	m.inFlight.Range(func(k, _ any) bool { queues = append(queues, k.(string)); return true })
	sort.Strings(queues)
	if len(queues) > 0 {
		b.WriteString("# HELP gombit_jobs_in_flight Jobs running now.\n# TYPE gombit_jobs_in_flight gauge\n")
		for _, q := range queues {
			fmt.Fprintf(&b, "gombit_jobs_in_flight{queue=%q} %d\n", q, m.inFlightFor(q).Load())
		}
	}
	if len(stats) > 0 {
		names := make([]string, 0, len(stats))
		for q := range stats {
			names = append(names, q)
		}
		sort.Strings(names)
		b.WriteString("# HELP gombit_jobs_queued Jobs in the queue by state.\n# TYPE gombit_jobs_queued gauge\n")
		for _, q := range names {
			st := stats[q]
			for _, s := range []struct {
				state string
				n     int
			}{{"ready", st.Ready}, {"scheduled", st.Scheduled}, {"reserved", st.Reserved}, {"failed", st.Failed}} {
				fmt.Fprintf(&b, "gombit_jobs_queued{queue=%q,state=%q} %d\n", q, s.state, s.n)
			}
		}
		b.WriteString("# HELP gombit_jobs_oldest_ready_seconds Age of the longest-waiting ready job.\n# TYPE gombit_jobs_oldest_ready_seconds gauge\n")
		for _, q := range names {
			age := 0.0
			if st := stats[q]; !st.OldestReady.IsZero() && now.After(st.OldestReady) {
				age = now.Sub(st.OldestReady).Seconds()
			}
			fmt.Fprintf(&b, "gombit_jobs_oldest_ready_seconds{queue=%q} %g\n", q, age)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}
