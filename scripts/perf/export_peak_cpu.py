#!/usr/bin/env python3
"""Export CPU interval evidence from archived Docker resource samples (no secrets)."""
import argparse
import csv
import datetime
import hashlib
import json
import statistics
from pathlib import Path


def timestamp(value):
    return datetime.datetime.fromisoformat(value.replace('Z', '+00:00')).timestamp()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('archive', type=Path)
    parser.add_argument('output', type=Path)
    args = parser.parse_args()
    measurements_path = args.output / 'measurements.csv'
    with measurements_path.open() as stream:
        reader = csv.DictReader(stream)
        fields = list(reader.fieldnames)
        measurements = list(reader)
    evidence = []
    for row in measurements:
        product = row['product']
        target = 'ferry-opt' if product.startswith('Ferry') else 'cliproxy' if product.startswith('CLIProxyAPI') else 'sub2api'
        components = [target] + (['postgres', 'redis'] if target == 'sub2api' else [])
        name = f"{target}-{row['case']}-r{row['repeat']}-resources.json"
        raw = (args.archive / row['run'] / name).read_bytes()
        samples = json.loads(raw)['samples']
        intervals = []
        for before, after in zip(samples, samples[1:]):
            values = []
            for component in components:
                a, b = before['containers'][component], after['containers'][component]
                seconds = timestamp(b['read']) - timestamp(a['read'])
                cpu_ns = b['cpu_stats']['cpu_usage']['total_usage'] - a['cpu_stats']['cpu_usage']['total_usage']
                if seconds <= 0 or cpu_ns < 0:
                    raise ValueError(f'Nonmonotonic sample: {name}')
                values.append({'component': component, 'seconds': seconds, 'cpu_ns': cpu_ns,
                               'cpu_percent': cpu_ns / 1e9 / seconds * 100})
            intervals.append({'components': values, 'stack_cpu_percent': sum(v['cpu_percent'] for v in values)})
        peak = max(v['stack_cpu_percent'] for v in intervals)
        row['peak_cpu_percent'] = peak
        evidence.append({'product': product, 'run': row['run'], 'case': row['case'],
                         'repeat': int(row['repeat']), 'source_file': name,
                         'source_sha256': hashlib.sha256(raw).hexdigest(),
                         'peak_cpu_percent': peak, 'intervals': intervals})
    if 'peak_cpu_percent' not in fields:
        fields.append('peak_cpu_percent')
    with measurements_path.open('w') as stream:
        writer = csv.DictWriter(stream, fields, lineterminator="\n")
        writer.writeheader()
        writer.writerows(measurements)
    summary_path = args.output / 'summary.json'
    summary = json.loads(summary_path.read_text())
    for row in summary:
        peaks = [r['peak_cpu_percent'] for r in evidence if r['product'] == row['product'] and r['case'] == row['case']]
        assert len(peaks) == row['repeats']
        row['peak_cpu_percent'] = statistics.median(peaks)
    summary_path.write_text(json.dumps(summary, indent=2) + '\n')
    (args.output / 'cpu-intervals.json').write_text(json.dumps({
        'unit': 'percent; 100% = one logical CPU core',
        'method': 'Consecutive monitor samples only; CPU delta / per-component read-time delta. Sum components in the same sample interval, take each run peak, then median of run peaks.',
        'runs': evidence}, indent=2) + '\n')
    print(f'Exported {len(evidence)} runs; {len(summary)} summaries')


if __name__ == '__main__':
    main()
