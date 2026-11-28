#!/usr/bin/env python3
"""Summarise one run.sh results directory: per-run CSV and markdown tables.

    python3 -I analyze.py results/<stamp>

Reads only the raw files run.sh wrote (cadvisor.csv, gate.csv, prom-*.txt, ready.json, meta.json,
cdc.log, pod-status.json). Writes runs.csv next to them and prints markdown.

Three phases per run, each analysed the same way:
  burst  the first composition's creation to the last Ready=True
  rest   the N compositions at rest after the burst, after a grace period (NOT idle at a small CPU limit)
  light  a small fleet at rest, after a grace period: the false-throttle probe
"""
import csv
import glob
import json
import os
import re
import statistics
import sys
from datetime import datetime

THRESHOLD = 50.0  # the gate's default threshold, unstructured-runtime pkg/ratelimit/saturation.go
SPAN = 30  # seconds; cAdvisor refreshes every 10-15s, so shorter windows are dominated by stale samples
JOB = 'job="composition-dynamic-controller"'  # the collector also receives core-provider's own metrics


def num(s):
    try:
        return float(s)
    except (TypeError, ValueError):
        return None


def read_csv(path):
    rows = []
    with open(path) as f:
        for r in csv.DictReader(f):
            r = {k: num(v) for k, v in r.items()}
            if r.get("epoch") is not None:
                rows.append(r)
    return rows


def buckets(path):
    """Cumulative histogram of reconcile duration, summed over series: {le: count}."""
    out = {}
    pat = re.compile(r'^unstructured_runtime_reconcile_duration_seconds_bucket\{(.*)\} (\S+)')
    for line in open(path):
        m = pat.match(line)
        if not m or JOB not in m.group(1):
            continue
        le = re.search(r'le="([^"]+)"', m.group(1))
        if le:
            k = float("inf") if le.group(1) == "+Inf" else float(le.group(1))
            out[k] = out.get(k, 0.0) + float(m.group(2))
    return out


def quantile(delta, q):
    """Prometheus histogram_quantile over cumulative bucket counts, linear inside the bucket."""
    les = sorted(delta)
    if not les or delta[les[-1]] <= 0:
        return None
    rank = q * delta[les[-1]]
    prev_le, prev_c = 0.0, 0.0
    for le in les:
        c = delta[le]
        if c >= rank:
            if le == float("inf"):
                return prev_le
            if c == prev_c:
                return le
            return prev_le + (le - prev_le) * (rank - prev_c) / (c - prev_c)
        prev_le, prev_c = le, c
    return None


def diff(a, b):
    return {k: b.get(k, 0.0) - a.get(k, 0.0) for k in set(a) | set(b)}


def pressure_windows(rows, lo, hi, span=SPAN):
    """Share of each ~span-second window in which some task waited, from the cumulative stall counter."""
    pts = [r for r in rows if lo <= r["epoch"] <= hi and r.get("pressure_waiting_s") is not None]
    out = []
    for i, a in enumerate(pts):
        for b in pts[i + 1:]:
            if b["epoch"] - a["epoch"] >= span:
                out.append(100.0 * (b["pressure_waiting_s"] - a["pressure_waiting_s"]) / (b["epoch"] - a["epoch"]))
                break
    return out


def span_delta(rows, key, lo, hi):
    pts = [r for r in rows if lo <= r["epoch"] <= hi and r.get(key) is not None]
    if len(pts) < 2:
        return None, None
    return pts[-1][key] - pts[0][key], pts[-1]["epoch"] - pts[0]["epoch"]


def log_cuts(path):
    """(epoch or None, from, to) of every 'lowering local capacity' line."""
    cuts = []
    for line in open(path):
        if "lowering local capacity" not in line:
            continue
        try:
            j = json.loads(line[line.index("{"):])
            ts = datetime.fromisoformat(str(j["timestamp"]).replace("Z", "+00:00")).timestamp()
            cuts.append((ts, j.get("from"), j.get("to")))
        except (ValueError, KeyError):
            cuts.append((None, None, None))
    return cuts


def phase(r, name, cad, gate, cuts, lo, hi, hist, lim, workers):
    """Metrics of one phase window [lo, hi] into r with prefix `name`."""
    win = pressure_windows(cad, lo, hi)
    waiting, secs = span_delta(cad, "pressure_waiting_s", lo, hi)
    usage, _ = span_delta(cad, "usage_s", lo, hi)
    per, _ = span_delta(cad, "periods", lo, hi)
    thr, _ = span_delta(cad, "throttled_periods", lo, hi)
    caps = [g["capacity"] for g in gate if g.get("capacity") is not None and lo <= g["epoch"] <= hi]
    inp = [g["input_pct"] for g in gate if g.get("input_pct") is not None and lo <= g["epoch"] <= hi]
    stamped = [c for c in cuts if c[0] is not None]
    r.update({
        f"{name}_s": hi - lo,
        f"{name}_reconciles": hist.get(float("inf")) if hist else None,
        f"{name}_p50_s": quantile(hist, 0.5) if hist else None,
        f"{name}_p95_s": quantile(hist, 0.95) if hist else None,
        f"{name}_pressure_mean_pct": 100 * waiting / secs if secs else None,
        f"{name}_pressure_maxwin_pct": max(win) if win else None,
        f"{name}_windows_over": sum(1 for w in win if w > THRESHOLD),
        f"{name}_windows": len(win),
        f"{name}_cpu_cores": usage / secs if secs else None,
        f"{name}_cpu_of_limit_pct": 100 * usage / secs / lim if secs else None,
        f"{name}_throttled_pct": 100 * thr / per if per else None,
        f"{name}_capacity_min": min(caps) if caps else None,
        f"{name}_capacity_mean": statistics.mean(caps) if caps else None,
        f"{name}_gate_samples": len(caps),
        f"{name}_gate_samples_below_workers": sum(1 for c in caps if c < workers),
        f"{name}_input_max_pct": max(inp) if inp else None,
        f"{name}_cuts": sum(1 for c in stamped if lo <= c[0] <= hi),
    })


def analyse(d):
    meta = json.load(open(f"{d}/meta.json"))
    ready = json.load(open(f"{d}/ready.json"))
    cad = read_csv(f"{d}/cadvisor.csv")
    gate = read_csv(f"{d}/gate.csv")
    cuts = log_cuts(f"{d}/cdc.log")
    first = ready.get("first_create") or meta["t0"]
    last = ready.get("last_ready")
    complete = bool(last) and ready["ready"] == ready["total"] and not meta["timed_out"]
    burst_end = last if complete else meta["t_ready_poll"]
    lim = float(str(meta["cpu_limit"]).rstrip("m")) / 1000.0
    w = meta["workers"]
    r = dict(meta)
    r.update(wall_s=(last - first) if complete else None, ready_of=f'{ready["ready"]}/{ready["total"]}',
             cuts_total=len(cuts), cut_floor_reached=any(c[2] == 1 for c in cuts), cuts_unstamped=len(cuts) - len([c for c in cuts if c[0] is not None]))
    hs, hr, he = buckets(f"{d}/prom-start.txt"), buckets(f"{d}/prom-ready.txt"), buckets(f"{d}/prom-end.txt")
    ls, le_ = buckets(f"{d}/prom-light-start.txt"), buckets(f"{d}/prom-light-end.txt")
    phase(r, "burst", cad, gate, cuts, first, burst_end, diff(hs, hr), lim, w)
    phase(r, "rest", cad, gate, cuts, meta["t_ready_poll"] + meta["idle_grace"], meta["t_end"], diff(hr, he), lim, w)
    phase(r, "light", cad, gate, cuts, meta["t_light_start"], meta["t_light_end"], diff(ls, le_), lim, w)
    ps = json.load(open(f"{d}/pod-status.json"))
    r.update(restarts=ps["restarts"], oomkilled=ps["oomkilled"], image_digest=meta["image_id"].split("@")[-1])
    return r


def fmt(v, p=1):
    return "-" if v is None else (f"{v:.{p}f}" if isinstance(v, float) else str(v))


def spread(vals):
    vals = [v for v in vals if v is not None]
    if not vals:
        return "-"
    sd = statistics.stdev(vals) if len(vals) > 1 else 0.0
    return f"{statistics.mean(vals):.1f} (min {min(vals):.1f}, max {max(vals):.1f}, sd {sd:.1f}, n={len(vals)})"


def phase_table(runs, name, title):
    print(f"\n### {title}\n")
    print(f"| run | arm | reconciles | p50 / p95 (s) | pressure mean / max {SPAN}s window (%) | {SPAN}s windows over {THRESHOLD:.0f} | throttled (%) | CPU (cores, % of limit) | gate capacity min / mean | gate samples below workers | cuts (log) |")
    print("|---|---|---|---|---|---|---|---|---|---|---|")
    for r in runs:
        g = lambda k: r[f"{name}_{k}"]  # noqa: E731
        print(f"| {r['run']} | {r['arm']} | {fmt(g('reconciles'), 0)} | {fmt(g('p50_s'), 2)} / {fmt(g('p95_s'), 2)} | {fmt(g('pressure_mean_pct'))} / {fmt(g('pressure_maxwin_pct'))} | "
              f"{g('windows_over')}/{g('windows')} | {fmt(g('throttled_pct'))} | {fmt(g('cpu_cores'), 3)}, {fmt(g('cpu_of_limit_pct'))} | "
              f"{fmt(g('capacity_min'), 0)} / {fmt(g('capacity_mean'))} | {g('gate_samples_below_workers')}/{g('gate_samples')} | {g('cuts')} |")


def main():
    out = sys.argv[1]
    runs = [analyse(os.path.dirname(p)) for p in sorted(glob.glob(f"{out}/run-*/meta.json"), key=lambda p: int(re.search(r"run-(\d+)-", p).group(1)))]
    keys = sorted({k for r in runs for k in r})
    with open(f"{out}/runs.csv", "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=keys)
        w.writeheader()
        w.writerows(runs)

    print("### Time to all-Ready, per run\n")
    print("| run | arm | Ready | all-Ready (s) | timed out | cdc pod | node | image digest | restarts / OOM |")
    print("|---|---|---|---|---|---|---|---|---|")
    for r in runs:
        print(f"| {r['run']} | {r['arm']} | {r['ready_of']} | {fmt(r['wall_s'], 0)} | {r['timed_out']} | `{r['pod']}` | {r['node'].split('-')[-1]} | `{r['image_digest'][:19]}` | {r['restarts']} / {r['oomkilled']} |")

    phase_table(runs, "burst", f"Burst: {runs[0]['n']} compositions created at once")
    print("\n### Burst, per arm\n")
    print("| metric | off | on |")
    print("|---|---|---|")
    arms = {a: [r for r in runs if r["arm"] == a] for a in ("off", "on")}
    for label, key in (("time to all-Ready (s)", "wall_s"), ("reconcile p50 (s)", "burst_p50_s"), ("reconcile p95 (s)", "burst_p95_s"),
                       ("reconciles run", "burst_reconciles"),
                       ("cpu.pressure, whole-burst mean (%)", "burst_pressure_mean_pct"), (f"cpu.pressure, max {SPAN}s window (%)", "burst_pressure_maxwin_pct"),
                       ("throttled periods (%)", "burst_throttled_pct"), ("CPU used (% of limit)", "burst_cpu_of_limit_pct"),
                       ("gate capacity, mean", "burst_capacity_mean"), ("gate capacity, min", "burst_capacity_min"), ("gate cuts (log lines)", "burst_cuts")):
        print(f"| {label} | " + " | ".join(spread([r[key] for r in arms[a]]) if arms[a] else "-" for a in ("off", "on")) + " |")

    phase_table(runs, "rest", f"At rest after the burst ({runs[0]['n']} compositions, after a grace period)")
    phase_table(runs, "light", f"Light load ({runs[0]['light_n']} compositions at rest): the false-throttle probe")
    print("\n### Instrument and control\n")
    for r in runs:
        for ph in ("burst", "rest", "light"):
            if r[f"{ph}_reconciles"] is not None and r[f"{ph}_reconciles"] < 0:
                print(f"- INVALID: run {r['run']} {ph} reconcile delta is negative ({r[f'{ph}_reconciles']:.0f}): the collector held another pod's counters")
        print(f"- run {r['run']} ({r['arm']}): gate samples {r['burst_gate_samples']} in the burst, max gate input {fmt(r['burst_input_max_pct'])}%, "
              f"cut log lines {r['cuts_total']} ({r['cuts_unstamped']} without a timestamp), floor (capacity 1) reached: {r['cut_floor_reached']}")


if __name__ == "__main__":
    main()
