# 2026-10-30 benchmark evidence

`measurements.csv` contains individual selected runs; `summary.json` contains per-product/per-case medians. Do not average rows from different products or windows. `provenance.json` identifies the measured candidate binary and changed runtime-source hashes, without private Git history or deployment configuration.

CLIProxyAPI uses 3 × 12-second cases. The final Ferry candidate uses 3 × 15 seconds. sub2api with a 2-vCPU database uses a single 30-second case. Candidate data is from a dedicated headless build, not an already published release image. Read the bilingual performance guide for resources, features and limits.

Reproduction tools are in `scripts/perf/`. Original raw response timing and resource samples are retained in the local audit archive; this public source bundle intentionally contains no synthetic account configuration, environment snapshots or databases. Exact SHA public CI and release-image verification remain separate from benchmark evidence.

`peak_cpu_percent` is the median of per-run sampled CPU peaks (100% = one logical core). `cpu-intervals.json` retains sanitized per-component interval deltas and source-file hashes for independent recalculation. All three products use the same calculation. Export with `scripts/perf/export_peak_cpu.py ARCHIVE OUTPUT`.
