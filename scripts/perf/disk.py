"""Read-only disk accounting for a STOPPED isolated Ferry benchmark container."""

import argparse
import json
import pathlib
import subprocess

p = argparse.ArgumentParser()
p.add_argument("container")
p.add_argument("output")
a = p.parse_args()
if not a.container.startswith("ferry-perf-"):
    raise SystemExit("Only isolated ferry-perf containers are supported")
d = json.loads(subprocess.check_output(["docker", "inspect", a.container]))[0]
if d["State"]["Running"]:
    raise SystemExit(
        "Stop the container first; immutable SQLite reads require a quiescent volume"
    )
m = next(
    m for m in d["Mounts"] if m["Destination"] == "/data" and m["Type"] == "volume"
)
code = """import json,os,sqlite3
root='/measure'
files={f:{'logical_bytes':os.stat(root+'/'+f).st_size,'allocated_bytes':os.stat(root+'/'+f).st_blocks*512} for f in os.listdir(root) if os.path.isfile(root+'/'+f)}
if any(f.endswith('-wal') and s['logical_bytes'] for f,s in files.items()): raise SystemExit('Uncheckpointed WAL: retain files, do not use immutable reads')
c=sqlite3.connect('file:/measure/ledger.db?mode=ro&immutable=1',uri=True)
counts={t:c.execute('select count(*) from '+t).fetchone()[0] for t in ['logical_requests','attempts','decision_traces','live_requests']}
print(json.dumps({'files':files,'counts':counts,'integrity_check':c.execute('pragma quick_check').fetchone()[0], 'page_size':c.execute('pragma page_size').fetchone()[0], 'free_pages':c.execute('pragma freelist_count').fetchone()[0], 'table_bytes':dict(c.execute('select name,sum(pgsize) from dbstat group by name'))}))
"""
result = json.loads(
    subprocess.check_output(
        [
            "docker",
            "run",
            "--rm",
            "--network",
            "none",
            "--mount",
            f"type=volume,src={m['Name']},dst=/measure,readonly",
            "python:3.12-alpine",
            "python",
            "-c",
            code,
        ]
    )
)
result["container"] = a.container
result["volume"] = m["Name"]
pathlib.Path(a.output).write_text(json.dumps(result, indent=2) + "\n")
print(
    json.dumps(
        {
            "counts": result["counts"],
            "ledger_bytes": result["files"]["ledger.db"]["allocated_bytes"],
            "integrity_check": result["integrity_check"],
        }
    )
)
