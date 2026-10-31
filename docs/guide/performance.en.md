# Resource comparison

Ferry uses Go + SQLite, supports headless operation and requires no external database.

### Memory usage

| Project | 1 KiB response: peak working set | 64 KiB response: peak working set |
| --- | ---: | ---: |
| **Ferry** | **30.8 MiB** | **34.5 MiB** |
| CLIProxyAPI | 68.1 MiB | 67.5 MiB |
| sub2api + PostgreSQL + Redis | 441.9 MiB | 432.5 MiB |

In the 64 KiB case, Ferry used about **49% less working-set memory than CLIProxyAPI** and **92% less than the sub2api stack**.

![Memory usage](../assets/perf-peak_working_set_mib-en.svg)

### Peak CPU usage

| Project | 1 KiB response | 64 KiB response |
| --- | ---: | ---: |
| **Ferry** | **146.6%** | **157.1%** |
| CLIProxyAPI | 151.0% | 151.2% |
| sub2api + PostgreSQL + Redis | 390.2% | 406.3% |

![Peak CPU usage](../assets/perf-peak_cpu_percent-en.svg)

Tested 2026-10-30 on ARM64 Docker, concurrency 32, zero-delay Mock. Each gateway: 2 vCPU. sub2api additionally uses PostgreSQL 2 vCPU and Redis 1 vCPU, included in totals. Ferry headless 1.1.0 candidate: 3×15 s; CLIProxyAPI 8.0.47: 3×12 s; sub2api 0.2.19: 1×30 s per case. Features and measurement windows differ; these are resource measurements for these configurations. Memory is working set, not RSS; sub2api sums component peaks.

## Method

Sizes refer to response content bytes; input text is 128 bytes. OpenAI Chat JSON over HTTP/1.1, no compression. Apple M1, Docker VM with 8 vCPU and about 8 GiB, non-exclusive host; one gateway tested at a time.

Ferry retains authentication, ledger, sessions and request traces. CLIProxyAPI uses commercial-mode with management and usage aggregation disabled. sub2api uses simple mode with its usage database enabled.

Working set is cgroup usage minus inactive_file, sampled about once per second; repeated runs use the median of run peaks. sub2api component peaks need not occur simultaneously. Peak CPU is the CPU-time delta between consecutive monitor samples divided by the read-time delta, multiplied by 100%; 100% means one logical core. sub2api sums its gateway, PostgreSQL and Redis within each sampling interval before taking the run peak. Repeated runs use the median of run peaks. The monitoring window includes startup and ledger-drain observation, excluding Mock and load-generator costs.

[Per-run CSV](../benchmarks/2026-10-30/measurements.csv) · [All measured metrics](../benchmarks/2026-10-30/summary.json) · [Build provenance](../benchmarks/2026-10-30/provenance.json)

Downloads retain all measured metrics. This page shows memory and peak CPU for all three projects. Tools: `scripts/perf/`; figures: `scripts/build_performance_figures.py`.

[CPU intervals and calculation data](../benchmarks/2026-10-30/cpu-intervals.json), extracted from the same resource samples by `scripts/perf/export_peak_cpu.py`.
