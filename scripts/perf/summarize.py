"""Export comparable summaries without discarding failed runs."""

import csv
import json
import pathlib
import sys

lab = pathlib.Path(sys.argv[1]).resolve()
rows = []
for source in sorted(lab.glob("*/summary.json")):
    for item in json.loads(source.read_text()):
        target = item["target"]
        resource = item["resources"].get(target, {})
        stack = [v for k, v in item["resources"].items() if k != "mock"]
        rows.append(
            {
                "run": source.parent.name,
                "target": target,
                "case": item["case"],
                "repeat": item["repeat"],
                "rate": item["rate"],
                "duration_seconds": item["arrival_seconds"],
                "success": item["success"],
                "success_rps": item["success_rps"],
                "errors": sum(item["errors"].values()),
                "dropped_arrivals": item["dropped_arrivals"],
                "p50_ms": item["latency_ms"].get("p50"),
                "p95_ms": item["latency_ms"].get("p95"),
                "p99_ms": item["latency_ms"].get("p99"),
                "first_content_p99_ms": item["first_content_ms"].get("p99"),
                "gateway_cpu_us_per_success": resource.get("cpu_us_per_success"),
                "stack_cpu_us_per_success": sum(v["cpu_us_per_success"] for v in stack),
                "gateway_peak_working_set_mib": resource.get("peak_working_set_mib"),
                "sum_component_peak_working_set_mib": sum(
                    v["peak_working_set_mib"] for v in stack
                ),
                "ledger_passed": item.get("ledger", {}).get("passed"),
                "drain_seconds": item.get("ledger", {}).get("drain_seconds"),
            }
        )
with (lab / "comparison.csv").open("w", newline="") as f:
    writer = csv.DictWriter(f, fieldnames=list(rows[0]))
    writer.writeheader()
    writer.writerows(rows)
print("Exported", len(rows), "case summaries to", lab / "comparison.csv")
