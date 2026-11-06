#!/usr/bin/env python3
"""Reproduce compositions that stop being reconciled after a spec change during the startup backlog.

Seen first as a calibration run that never drained: 19 of 64 compositions took one helm pass and were
then silent for 30 minutes, while the other 45 were re-observed. Debug logs show every enqueue and
dedup decision, so this streams them from before the pod starts.

    CHART_VERSION=2.13.70 python3 repro_freeze.py [--limit 250m] [--wait 420]
"""
import argparse
import json
import os
import subprocess
import sys
import time

import run

OUT = os.path.expanduser("~/krateo/cpu-sat-66-results/freeze")
os.makedirs(OUT, exist_ok=True)


def stale(marker):
    cm = json.loads(run.kubectl("-n", run.NS, "get", "cm", "-l", "app.kubernetes.io/component=sentinel", "-o", "json"))["items"]
    seen = {c["metadata"]["name"].removesuffix("-sentinel"): c["data"]["marker"] for c in cm}
    crs = json.loads(run.kubectl("-n", run.NS, "get", run.PLURAL, "-o", "json"))["items"]
    out = []
    for o in crs:
        rel = o["metadata"].get("labels", {}).get("krateo.io/release-name", o["metadata"]["name"])
        if seen.get(rel) != marker:
            out.append(o["metadata"]["name"])
    return out, len(crs)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--limit", default="250m")
    ap.add_argument("--wait", type=int, default=420)
    a = ap.parse_args()
    run.guard_context()

    run.sh("helm", "upgrade", "core-provider", run.CHART, "--version", os.environ["CHART_VERSION"], "-n", "krateo-system",
           "-f", f"{run.HERE}/values-base.yaml", "--set", f"cdc.resources.limits.cpu={a.limit}",
           "--set-string", "cdc.env.COMPOSITION_CONTROLLER_DEBUG=true", "--wait", "--timeout", "5m")
    deadline = time.time() + 400
    while True:
        data = json.loads(run.kubectl("-n", run.NS, "get", "cm", run.ENV_CM, "-o", "json")).get("data", {})
        if data.get("COMPOSITION_CONTROLLER_DEBUG") == "true" and data.get("OTEL_ENABLED") == "true":
            break
        if time.time() > deadline:
            sys.exit(f"the settings never reached the controller: {sorted(data)[:6]}")
        time.sleep(5)

    run.kubectl("-n", run.NS, "rollout", "restart", f"deploy/{run.CDC}")
    run.kubectl("-n", run.NS, "rollout", "status", f"deploy/{run.CDC}", "--timeout=300s")
    pod = run.cdc_pod()
    name = pod["metadata"]["name"]
    print("pod", name, "image", pod["spec"]["containers"][0]["image"], flush=True)

    path = f"{OUT}/{name}.log"
    log = open(path, "w")
    stream = subprocess.Popen(["kubectl", "-n", run.NS, "logs", "-f", name, "-c", pod["spec"]["containers"][0]["name"]],
                              stdout=log, stderr=subprocess.STDOUT)

    time.sleep(run.STEADY_WAIT)
    marker = f"{int(time.time())}"
    run.apply_all(64, marker, 40, 4)
    print("patched at", time.strftime("%H:%M:%SZ", time.gmtime()), "marker", marker, flush=True)

    t0 = time.time()
    while time.time() - t0 < a.wait:
        s, n = stale(marker)
        print(f"  {int(time.time() - t0)}s stale {len(s)} of {n}", flush=True)
        if not s:
            break
        time.sleep(30)

    s, n = stale(marker)
    json.dump({"pod": name, "marker": marker, "stale": s, "of": n, "log": path}, open(f"{OUT}/state.json", "w"), indent=1)
    print(f"RESULT stale {len(s)} of {n}; log {path}", flush=True)
    stream.terminate()


main()
