# benchmarks/results

Generated benchmark output (issue #141 §9). **Nothing here is hand-edited** —
Markdown is never the canonical data source; it is generated from the
structured results.

A full run writes a `latest/` snapshot:

```text
results/
└── latest/
    ├── metadata.json   reproducibility metadata (benchmarks/internal/metadata)
    ├── raw/            per-trial raw k6 summaries, <framework>_<benchmark>_c<concurrency>_t<trial>.json
    ├── results.json    the canonical structured throughput results (benchmarks/internal/result)
    ├── results.csv     flat CSV derived from results.json
    ├── summary.md      human-readable tables derived from results.json
    ├── footprint.json  operational-footprint rows (benchmarks/internal/footprint)
    ├── footprint.csv   flat CSV derived from footprint.json
    └── microbench.json framework-tax ns/op·B/op·allocs/op (benchmarks/internal/microbench)
```

`results.json` / `results.csv` are the machine-readable schema defined in
[`benchmarks/internal/result`](../internal/result); `metadata.json` is the
[`benchmarks/internal/metadata`](../internal/metadata) shape, collected by
[`benchmarks/scripts/collect-host-info`](../scripts/collect-host-info).

`latest/` **is committed** (including `raw/`, the per-trial k6 summaries, so a
later parser fix can reconstruct the numbers; each is named for the framework
and workload whose rows it backs, so recording one workload never overwrites
another's evidence), so the published README
performance tables are always regenerable from a committed snapshot. The current
committed snapshot is a **reduced run on a single developer host** (a contended
WSL2 laptop, not dedicated benchmark hardware) — `metadata.json` records the exact
host, kernel, and the protocol that ran (concurrency levels, trials, duration),
and the generated README block prints them so the numbers can't be mistaken for
the canonical sweep. Re-running the full sweep on quiet, dedicated hardware and
committing that snapshot remains open. The orchestration that fills `latest/`:
`make benchmark-crud-all` (CRUD throughput across all six apps),
`make benchmark-footprint` (cold-start/RSS/CPU → `footprint.json`),
`make benchmark-micro` (framework tax → `microbench.json`), and
`make benchmark-summary`/`make benchmark-report` render `summary.md` and the
README block.
