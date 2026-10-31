# Isolated performance lab

This is a synthetic benchmark, not a production deployment. It uses Docker's internal network, synthetic credentials and named test volumes. Existing services are untouched. The controller is accessible via `docker exec`; no browser UI or externally reachable endpoint is required.

The Go `perf-lab` supports OpenAI Chat JSON/SSE, configurable content bytes (not total wire bytes), first-output delay, deterministic jitter, chunk count/interval, and periodic status errors. POST `/config` sets a scenario; GET `/stats` returns requests, active requests and observed cancellations. The load driver validates status, JSON/SSE, content size and `[DONE]`, records raw request latency/first-content/gaps and supports closed-loop saturation or fixed-rate arrivals with bounded queuing and dropped-arrival counts. Non-streaming first-content time means a completely parsed response, not HTTP TTFB. Streaming means the first nonempty content delta.

## Reproduce

Use a dedicated output directory outside the checkout. Build `./scripts/perf` with `CGO_ENABLED=0 GOOS=linux GOARCH=arm64` into `OUTPUT/bin/perf-lab`. Obtain and checksum the official CLIProxyAPI 8.0.47 Linux aarch64 release binary under `OUTPUT/bin/cliproxy/cli-proxy-api`. Pull the ARM64 images used in `prepare.py` and record their actual digests. The checked-in versions describe this experiment, not a rolling latest policy.

```sh
python3 scripts/perf/prepare.py OUTPUT /absolute/path/to/candidate-ferry
# First initialize the lab; do not run against an existing production Compose project.
docker compose -f OUTPUT/compose.json up -d
# Read sub2api's commitment first. The next explicit option accepts it for this instance.
docker exec ferry-perf-controller python /scripts/bootstrap.py --accept-compliance
python3 scripts/perf/run.py OUTPUT --targets ferry,ferry-opt,cliproxy,sub2api --duration 12 --repeats 3 --cases json-1k-c32,json-64k-c32,sse-small-c32
```

`bootstrap.py` is a first-install operation; do not rerun it on an already initialized lab. Retain its generated synthetic key file when resuming. `prepare.py` regenerates lab configuration and should not be used over manually modified config. For a headless candidate, use a binary built with `-tags headless`. A normal candidate may use `-headless` in its Compose command. No image is published by this workflow.

Each gateway gets 2 vCPU / 1536 MiB; sub2api additionally gets PostgreSQL 1 vCPU / 768 MiB and Redis 1 vCPU / 256 MiB. Mock and load driver each get 2 vCPU. They share one VM, so record host load and do not interpret these as dedicated hardware capacity. The runner stops other test gateways between targets; it never stops non-lab containers. It samples raw Docker cgroup counters, reports CPU including final drain observation and memory working set excluding inactive file cache (not process RSS). Mock/driver overhead must be assessed from raw samples. Sum sub2api gateway/database/cache resources when comparing full deployment cost.

Ferry retains ledger, security evidence and conversation semantics. CLIProxyAPI uses commercial mode, management disabled and usage aggregation disabled; sub2api uses simple mode, with its own usage database. These are documented configurations, **not identical feature sets**. The cross-product results describe deployment tradeoffs; only Ferry-before/after supports a same-semantics optimization claim.

The lab sets the same namespaced TCP port range and TIME_WAIT reuse policy for gateways, Mock and driver. Initial stock-network screening hit CLIProxyAPI's ephemeral-port exhaustion; its failed data must be retained separately. Do not change host-global TCP settings. Steady-state comparisons should include fixed-rate runs, because closed-loop saturation latency does not represent a common offered load.

Results contain raw per-request JSON, warmup summaries, aggregate summaries, container identities and resource samples. Ferry and sub2api finalized ledger counts are checked against successful warmup + measured requests, allowing up to 30 seconds to drain. Errors, dropped arrivals and failed ledger checks invalidate a success-only throughput claim. Initial 8–12 second runs are screening/short comparisons, not the planned long-duration release performance qualification.


Stop only this lab when finished:

```sh
docker compose -f OUTPUT/compose.json stop
```

Retain volumes and raw evidence for analysis. Delete only lab-owned volumes explicitly when they are no longer needed.

## Disk accounting

Use fresh lab volumes and the same synthetic control-store seed when comparing storage costs. Preserve file ownership when seeding; the gateway runs as an unprivileged user. Fixed arrivals (for example 200/s for 30 seconds) produce 6,000 measured requests plus 400 warmup requests; verify the actual success and persisted counts instead of assuming them. Do not compare accumulated volumes from different experiment histories.

After stopping every writer to the volume, run `python3 scripts/perf/disk.py ferry-perf-ferry-opt OUTPUT/disk.json`. This mounts only that isolated container's data volume read-only, rejects an uncheckpointed WAL and records allocated/logical file bytes, SQLite page use, persisted row counts and quick_check. It neither vacuums nor deletes data. End-state allocated bytes include the empty schema and control store; measuring incremental growth requires a separate matching pre-load snapshot. Disk bytes do not measure physical write amplification or IOPS.
