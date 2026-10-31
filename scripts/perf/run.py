"""Reproducible isolated comparisons with raw request and Docker cgroup evidence."""

import argparse, datetime, http.client, json, pathlib, random, socket, subprocess, threading, time

p = argparse.ArgumentParser()
p.add_argument("lab")
p.add_argument("--duration", type=int, default=10)
p.add_argument("--repeats", type=int, default=1)
p.add_argument("--targets", default="direct,ferry,cliproxy,sub2api")
p.add_argument("--cases", default="")
p.add_argument("--rate", type=float, default=0)
p.add_argument("--label", default="screen")
a = p.parse_args()
lab = pathlib.Path(a.lab).resolve()
out = lab / (a.label + "-" + datetime.datetime.now().strftime("%H%M%S"))
out.mkdir()
keys = json.loads((lab / "lab-keys.json").read_text())
compose = ["docker", "compose", "-f", str(lab / "compose.json")]
host = (
    subprocess.check_output(
        ["docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"],
        text=True,
    )
    .strip()
    .removeprefix("unix://")
)


class Conn(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(15)
        self.sock.connect(host)


def engine(path):
    c = Conn("localhost")
    c.request("GET", "/v1.47/" + path)
    r = c.getresponse()
    data = r.read()
    c.close()
    if r.status != 200:
        raise RuntimeError(data[:200])
    return json.loads(data)


def stats(name):
    return engine(f"containers/ferry-perf-{name}/stats?stream=false&one-shot=true")


def call_mock(cfg):
    code = (
        'import urllib.request; print(urllib.request.urlopen(urllib.request.Request("http://mock:8080/config",data='
        + repr(json.dumps(cfg).encode())
        + ',headers={"Content-Type":"application/json"})).read().decode())'
    )
    subprocess.run(
        ["docker", "exec", "ferry-perf-controller", "python", "-c", code],
        check=True,
        capture_output=True,
    )


cases = [
    ("json-1k-c32", 0, 1024, 1, 0, 32, 128, False),
    ("json-64k-c32", 0, 65536, 1, 0, 32, 128, False),
    ("json-50ms-c32", 50, 1024, 1, 0, 32, 128, False),
    ("json-200ms-c64", 200, 1024, 1, 0, 64, 128, False),
    ("sse-small-c32", 20, 1024, 16, 5, 32, 128, True),
    ("sse-large-c16", 50, 262144, 256, 1, 16, 65536, True),
]
if a.cases:
    cases = [c for c in cases if c[0] in a.cases.split(",")]
urls = {
    "direct": "mock:8080",
    "ferry": "ferry:7890",
    "ferry-opt": "ferry-opt:7890",
    "cliproxy": "cliproxy:8317",
    "sub2api": "sub2api:8080",
}
targets = a.targets.split(",")
rows = []
resources = ["mock", "ferry", "ferry-opt", "cliproxy", "sub2api", "postgres", "redis"]
meta = {
    "created": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "parameters": vars(a),
    "docker": engine("info"),
    "containers": {
        n: engine("containers/ferry-perf-" + n + "/json") for n in resources
    },
    "cases": cases,
    "scope": "same-host Linux ARM64 Docker; synthetic upstream; closed loop unless rate > 0; no production traffic",
}
# Runtime environment contains ONLY this lab's synthetic credentials.
(out / "environment.json").write_text(json.dumps(meta, indent=2))


def ledger_count(target):
    if target.startswith("ferry"):
        code = (
            'import sqlite3,json;c=sqlite3.connect("file:/'
            + target
            + '-data/ledger.db?mode=ro",uri=True);print(json.dumps([c.execute("select count(*) from logical_requests").fetchone()[0],c.execute("select count(*) from live_requests").fetchone()[0]]))'
        )
        return json.loads(
            subprocess.check_output(
                ["docker", "exec", "ferry-perf-controller", "python", "-c", code],
                text=True,
            )
        )
    if target == "sub2api":
        value = subprocess.check_output(
            [
                "docker",
                "exec",
                "ferry-perf-postgres",
                "psql",
                "-U",
                "bench",
                "-d",
                "bench",
                "-tAc",
                "select count(*) from usage_logs",
            ],
            text=True,
        )
        return [int(value.strip()), 0]
    return None


for repeat in range(a.repeats):
    order = targets.copy()
    random.Random(47 + repeat).shuffle(order)
    for target in order:
        inactive = [
            n for n in ["ferry", "ferry-opt", "cliproxy", "sub2api"] if n != target
        ]
        subprocess.run(
            ["docker", "stop", *["ferry-perf-" + n for n in inactive]],
            check=True,
            capture_output=True,
        )
        if target == "sub2api":
            subprocess.run(
                [
                    "docker",
                    "start",
                    "ferry-perf-postgres",
                    "ferry-perf-redis",
                    "ferry-perf-sub2api",
                ],
                check=True,
                capture_output=True,
            )
        else:
            subprocess.run(
                ["docker", "stop", "ferry-perf-postgres", "ferry-perf-redis"],
                check=True,
                capture_output=True,
            )
            if target != "direct":
                subprocess.run(
                    ["docker", "start", "ferry-perf-" + target],
                    check=True,
                    capture_output=True,
                )
        time.sleep(3)
        for name, delay, size, chunks, interval, c, prompt, stream in cases:
            ident = f"{target}-{name}-r{repeat + 1}"
            rel = out.relative_to(lab)
            raw = out / (ident + ".json")
            call_mock(
                {
                    "delay_ms": delay,
                    "content_bytes": size,
                    "chunks": chunks,
                    "interval_ms": interval,
                }
            )
            args = [
                "load",
                "--url",
                "http://" + urls[target] + "/v1/chat/completions",
                "--key",
                keys[target],
                "--concurrency",
                str(c),
                "--prompt-bytes",
                str(prompt),
                "--expected-bytes",
                str(size),
                "--stream=" + str(stream).lower(),
                "--rate",
                str(a.rate),
            ]
            before_ledger = ledger_count(target)
            warm = (
                compose
                + ["run", "--rm", "-T", "--name", "ferry-perf-load-warm", "load"]
                + args
                + ["--duration", "2s", "--out", "/results/" + str(rel) + "/warm.json"]
            )
            subprocess.run(warm, check=True, capture_output=True)
            w = json.loads((out / "warm.json").read_text())
            (out / (ident + "-warm.json")).write_text(
                json.dumps({k: v for k, v in w.items() if k != "samples"})
            )
            if w["errors"] or not w["success"]:
                raise RuntimeError(f"{ident} warmup failed {w['errors']}")
            measured = (
                ["mock"]
                + ([] if target == "direct" else [target])
                + (["postgres", "redis"] if target == "sub2api" else [])
            )
            initial = {n: stats(n) for n in measured}
            samples = []
            stop = threading.Event()
            problems = []

            def monitor():
                while not stop.is_set():
                    point = {"time": time.time(), "containers": {}}
                    for n in measured + ["load-run"]:
                        try:
                            point["containers"][n] = stats(n)
                        except Exception as e:
                            if n != "load-run":
                                problems.append(str(e))
                    samples.append(point)
                    stop.wait(1)

            thread = threading.Thread(target=monitor, daemon=True)
            thread.start()
            beg = time.monotonic()
            run = subprocess.run(
                compose
                + ["run", "--rm", "-T", "--name", "ferry-perf-load-run", "load"]
                + args
                + [
                    "--duration",
                    str(a.duration) + "s",
                    "--out",
                    "/results/" + str(rel) + "/" + raw.name,
                ],
                capture_output=True,
                text=True,
            )
            if run.returncode:
                stop.set()
                thread.join()
                (out / (ident + "-failure.txt")).write_text(run.stderr)
                raise RuntimeError(run.stderr)
            result = json.loads(raw.read_text())
            expected_count = None
            after_ledger = None
            drain_start = time.monotonic()
            if before_ledger is not None:
                expected_count = before_ledger[0] + w["success"] + result["success"]
                while True:
                    after_ledger = ledger_count(target)
                    if after_ledger[0] == expected_count and after_ledger[1] == 0:
                        break
                    if time.monotonic() - drain_start > 30:
                        break
                    time.sleep(0.5)
            drain_seconds = time.monotonic() - drain_start
            elapsed = time.monotonic() - beg
            stop.set()
            thread.join()
            final = {n: stats(n) for n in measured}
            if run.returncode:
                raise RuntimeError(run.stderr)
            result = json.loads(raw.read_text())
            summary = {k: v for k, v in result.items() if k != "samples"}
            summary.update(
                target=target,
                case=name,
                repeat=repeat + 1,
                resource_window_seconds=elapsed,
            )
            usage = {}
            for n in measured:
                readings = [x["containers"][n] for x in samples if n in x["containers"]]

                def working(s):
                    m = s.get("memory_stats", {})
                    return max(
                        0,
                        m.get("usage", 0) - m.get("stats", {}).get("inactive_file", 0),
                    )

                cpu = (
                    final[n]["cpu_stats"]["cpu_usage"]["total_usage"]
                    - initial[n]["cpu_stats"]["cpu_usage"]["total_usage"]
                ) / 1e9
                usage[n] = {
                    "cpu_seconds": cpu,
                    "cpu_us_per_success": cpu / max(1, result["success"]) * 1e6,
                    "peak_working_set_mib": max(
                        [working(x) for x in readings]
                        + [working(initial[n]), working(final[n])]
                    )
                    / 2**20,
                    "after_working_set_mib": working(final[n]) / 2**20,
                }
            summary["ledger"] = {
                "before": before_ledger,
                "after": after_ledger,
                "expected_finalized": expected_count,
                "drain_seconds": drain_seconds,
                "passed": None
                if before_ledger is None
                else after_ledger[0] == expected_count and after_ledger[1] == 0,
            }
            summary["resources"] = usage
            summary["resource_errors"] = problems
            rows.append(summary)
            (out / (ident + "-resources.json")).write_text(
                json.dumps({"initial": initial, "samples": samples, "final": final})
            )
            (out / "summary.json").write_text(json.dumps(rows, indent=2))
            print(
                json.dumps(
                    {
                        k: summary[k]
                        for k in [
                            "target",
                            "case",
                            "repeat",
                            "success_rps",
                            "latency_ms",
                            "first_content_ms",
                            "errors",
                            "resources",
                        ]
                    }
                ),
                flush=True,
            )
status = {
    "passed": all(
        not row["errors"]
        and row["dropped_arrivals"] == 0
        and row["ledger"]["passed"] is not False
        and not row["resource_errors"]
        for row in rows
    ),
    "cases": len(rows),
}
(out / "status.json").write_text(json.dumps(status, indent=2))
print("RESULTS", out, flush=True)
if not status["passed"]:
    raise SystemExit(
        "Benchmark contains errors, dropped arrivals, incomplete ledger or missing telemetry"
    )
