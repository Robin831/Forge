#!/usr/bin/env python3
"""Extract per-pass Assay telemetry from the daemon logs into a JSON artifact.

One row per `passes=` segment of every `Assay review completed` line whose run
STARTED (completion time minus duration_ms) in [--from, --to). Each row carries
the run-level fields (pr, bead, status, cost, duration) beside the pass fields.

    python3 -I measurements/extract-assay-passes.py \
        --from 2026-10-06T17:00:32Z --to 2026-10-14T07:26:00Z \
        --out measurements/assay-passes-20261006T170032Z-20261014T072600Z.json

Used for docs/assay-turn-budget.md (Forge-8we9m). Reads ~/.forge/logs/daemon*.log*
unless --logs is given.
"""
import argparse
import glob
import gzip
import json
import os
import re
from datetime import datetime, timedelta

LINE = re.compile(r'time=(\S+).*?msg="Assay review completed"(.*)')
FIELD = re.compile(r'\b(\w+)=([^\s,]+)')
INTS = {'turns', 'tools', 'files', 'cache_w', 'cache_r', 'primer'}
FLOATS = {'cost_usd', 'cost_est'}


def when(s):
    return datetime.fromisoformat(s.replace('Z', '+00:00'))


def typed(k, v):
    if k in INTS:
        return int(v)
    if k in FLOATS:
        return float(v)
    return v  # retry stays a string: it is `1` or `skipped`


def opener(p):
    return gzip.open(p, 'rt', errors='replace') if p.endswith('.gz') else open(p, errors='replace')


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--from', dest='lo', required=True, type=when)
    ap.add_argument('--to', dest='hi', required=True, type=when)
    ap.add_argument('--out', required=True)
    ap.add_argument('--logs', default=os.path.expanduser('~/.forge/logs/daemon*.log*'))
    a = ap.parse_args()

    rows = []
    for p in sorted(glob.glob(a.logs)):
        with opener(p) as f:
            for ln in f:
                m = LINE.search(ln)
                if not m:
                    continue
                rest = m.group(2)
                top = dict(FIELD.findall(re.sub(r'(passes|status)="[^"]*"', '', rest)))
                dur = int(top.get('duration_ms', 0))
                start = datetime.fromisoformat(m.group(1)) - timedelta(milliseconds=dur)
                if not (a.lo <= start < a.hi):
                    continue
                sm = re.search(r'status="([^"]*)"', rest)
                pm = re.search(r'passes="([^"]*)"', rest)
                run = dict(ts=m.group(1), run_start=start.isoformat(), pr=int(top['pr']),
                           bead=top.get('bead'), run_status=sm.group(1) if sm else None,
                           run_cost_usd=round(float(top.get('cost_usd', 0)), 4),
                           run_duration_ms=dur)
                for seg in (pm.group(1).split(', pass=') if pm else []):
                    seg = seg if seg.startswith('pass=') else 'pass=' + seg
                    row = dict(run)
                    row.update((k, typed(k, v)) for k, v in FIELD.findall(seg))
                    rows.append(row)
    rows.sort(key=lambda r: r['ts'])  # stable: passes keep their log order
    with open(a.out, 'w') as f:
        json.dump(rows, f, indent=1)
    print(len(rows), 'pass rows from', len({(r['ts'], r['pr']) for r in rows}), 'runs ->', a.out)


if __name__ == '__main__':
    main()
