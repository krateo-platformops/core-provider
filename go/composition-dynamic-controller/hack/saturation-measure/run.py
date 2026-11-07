#!/usr/bin/env python3
"""Measure the cdc's CPU-saturation rule on a real controller (unstructured-runtime#66).

Main number: time to drain a fixed backlog. One run patches `marker` on every composition and waits
until every sentinel ConfigMap carries it, so completion is read from what landed in the cluster and
not from a condition the controller under test writes.

    python3 run.py setup --objects 64
    python3 run.py one --limit 250m --threshold off [--mode restart]
    python3 run.py sweep --runs 3

Two modes, reported separately so a ramp cost cannot blur into the steady-state figures:
  steady   the backlog arrives after the ramp, comparable to provider-runtime's window
  restart  the backlog is waiting when the controller starts; drain is timed from pod ready, because
           this is the case the startup ramp exists for

KUBECONFIG must point at the disposable cluster. The script refuses any other context.
"""
import argparse
import json
import os
import re
import subprocess
import sys
import tempfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
NS = "load"
PLURAL = "loadcharts"
CDC = "loadcharts-v0-2-0-controller"
ENV_CM = "loadcharts-v0-2-0-configmap"
API = "composition.krateo.io/v0-2-0"
KIND = "Loadchart"
CHART = "oci://ghcr.io/krateo-platformops/charts/core-provider"
EXPECTED_CONTEXT = "gke_integration-test-431120_us-central1-a_cpu-sat-66"
RESULTS = os.path.expanduser("~/krateo/cpu-sat-66-results/results.jsonl")
STEADY_WAIT = 45  # the issue discards the first ~40s; the backlog starts after it
DRAIN_TIMEOUT = 1800
MAX_SAMPLE_GAP = 45  # a sample takes a few seconds; more than this means the driver was not running


def sh(*args, check=True, stdin=None):
    r = subprocess.run(args, capture_output=True, text=True, input=stdin)
    if check and r.returncode != 0:
        sys.exit(f"FAILED: {' '.join(args)[:160]}\n{(r.stdout + r.stderr)[-600:]}")
    return r.stdout


def kubectl(*args, check=True, stdin=None):
    return sh("kubectl", *args, check=check, stdin=stdin)


def guard_context():
    ctx = kubectl("config", "current-context").strip()
    if ctx != EXPECTED_CONTEXT:
        sys.exit(f"refusing to run against {ctx!r}")


def manifest(i, marker, count, payload):
    return {
        "apiVersion": API, "kind": KIND,
        "metadata": {"name": f"load-{i:03d}", "namespace": NS},
        "spec": {"count": count, "payloadKB": payload, "marker": marker},
    }


def apply_all(n, marker, count, payload):
    doc = {"apiVersion": "v1", "kind": "List", "items": [manifest(i, marker, count, payload) for i in range(n)]}
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(doc, f)
    try:
        kubectl("apply", "--server-side", "--force-conflicts", "--field-manager=harness", "-f", f.name)
    finally:
        os.unlink(f.name)


def sentinels(marker):
    """How many sentinels carry `marker`, and how many exist. Both, so a zero has a denominator."""
    items = json.loads(kubectl("-n", NS, "get", "cm", "-l", "app.kubernetes.io/component=sentinel", "-o", "json"))["items"]
    return sum(1 for c in items if c.get("data", {}).get("marker") == marker), len(items)


def cdc_pod():
    pods = json.loads(kubectl("-n", NS, "get", "pods", "-o", "json"))["items"]
    live = [p for p in pods if p["metadata"]["name"].startswith(CDC) and "deletionTimestamp" not in p["metadata"]]
    if len(live) != 1:
        sys.exit(f"want one cdc pod, found {[p['metadata']['name'] for p in live]}")
    return live[0]


def set_arm(limit, threshold):
    on = threshold != "off"
    sh("helm", "upgrade", "core-provider", CHART, "--version", os.environ["CHART_VERSION"], "-n", "krateo-system",
       "-f", f"{HERE}/values-base.yaml",
       "--set", f"cdc.resources.limits.cpu={limit}",
       "--set-string", f"cdc.env.COMPOSITION_CONTROLLER_SATURATION_ENABLED={'true' if on else 'false'}",
       "--set-string", f"cdc.env.COMPOSITION_CONTROLLER_CPU_PRESSURE_THRESHOLD={threshold if on else '0'}",
       "--wait", "--timeout", "5m")

    # core-provider applies this on its own reconcile, so wait for what the cdc will actually read.
    # Ready stays True while a bad value blocks every rollout, so Synced and the ConfigMap are the
    # evidence: a sweep once ran with the OTEL settings stuck behind an invalid Deployment.
    want = {"COMPOSITION_CONTROLLER_SATURATION_ENABLED": "true" if on else "false",
            "COMPOSITION_CONTROLLER_CPU_PRESSURE_THRESHOLD": threshold if on else "0",
            "OTEL_ENABLED": "true", "OTEL_EXPORT_INTERVAL": "5s"}
    deadline = time.time() + 300
    while True:
        data = json.loads(kubectl("-n", NS, "get", "cm", ENV_CM, "-o", "json")).get("data", {})
        dep = json.loads(kubectl("-n", NS, "get", "deploy", CDC, "-o", "json"))
        got = dep["spec"]["template"]["spec"]["containers"][0]["resources"]["limits"]["cpu"]
        conds = json.loads(kubectl("-n", NS, "get", "compositiondefinition", "loadchart", "-o", "json"))["status"]["conditions"]
        synced = next((c for c in conds if c["type"] == "Synced"), {})
        if all(data.get(k) == v for k, v in want.items()) and normal_cpu(got) == normal_cpu(limit) and synced.get("status") == "True":
            break
        if time.time() > deadline:
            sys.exit(f"arm never applied: want {want} limit {limit}, env {[data.get(k) for k in want]} limit {got}, "
                     f"Synced {synced.get('status')}: {synced.get('message', '')[:200]}")
        time.sleep(5)

    # The env arrives by ConfigMap, which a running pod does not re-read.
    kubectl("-n", NS, "rollout", "restart", f"deploy/{CDC}")
    kubectl("-n", NS, "rollout", "status", f"deploy/{CDC}", "--timeout=300s")


def normal_cpu(v):
    return int(v[:-1]) if str(v).endswith("m") else int(float(v) * 1000)


def cadvisor(node):
    return kubectl("get", "--raw", f"/api/v1/nodes/{node}/proxy/metrics/cadvisor")


def series(text, name, pod, container):
    for line in text.splitlines():
        if line.startswith(name + "{") and f'pod="{pod}"' in line and f'container="{container}"' in line:
            return float(line.rsplit("}", 1)[1].split()[0])
    return None


def node_usage(text):
    for line in text.splitlines():
        if line.startswith("container_cpu_usage_seconds_total{") and 'id="/"' in line:
            return float(line.rsplit("}", 1)[1].split()[0])
    return None


def collector():
    return kubectl("get", "--raw", "/api/v1/namespaces/otel/services/otel:8889/proxy/metrics", check=False)


def gauge(text, name):
    vals = [float(l.rsplit(" ", 1)[1]) for l in text.splitlines() if re.match(name + r"[{ ]", l)]
    return vals[0] if vals else None


def deferred(text):
    """Deferrals by cause, summed over keys."""
    out = {}
    for line in text.splitlines():
        m = re.match(r'unstructured_runtime_ratelimit_deferrals_total\{.*cause="(\w+)".*\} (\S+)', line)
        if m:
            out[m.group(1)] = out.get(m.group(1), 0) + float(m.group(2))
    return out


def reconciled(text):
    return sum(float(l.rsplit(" ", 1)[1]) for l in text.splitlines()
               if l.startswith("unstructured_runtime_reconcile_success_total"))


def sample(node, pod, container):
    text = cadvisor(node)
    other = collector()
    return {
        "t": time.time(),
        "usage": series(text, "container_cpu_usage_seconds_total", pod, container),
        "waiting": series(text, "container_pressure_cpu_waiting_seconds_total", pod, container),
        "periods": series(text, "container_cpu_cfs_periods_total", pod, container),
        "throttled": series(text, "container_cpu_cfs_throttled_periods_total", pod, container),
        "node": node_usage(text),
        "local": gauge(other, "unstructured_runtime_ratelimit_local_capacity"),
        "input": gauge(other, r"unstructured_runtime_ratelimit_saturation_input(_percent)?"),
        "deferred": deferred(other),
        "reconciled": reconciled(other),
    }


def delta(a, b, k):
    return None if a[k] is None or b[k] is None else b[k] - a[k]


def wait_no_pods():
    """The backlog must arrive with no controller alive, or the old pod starts working it."""
    deadline = time.time() + 180
    while time.time() < deadline:
        pods = json.loads(kubectl("-n", NS, "get", "pods", "-o", "json"))["items"]
        if not [p for p in pods if p["metadata"]["name"].startswith(CDC)]:
            return
        time.sleep(1)
    sys.exit("the old controller never went away")


def wait_ready_pod():
    """The new controller, once Ready. Polls rather than waits on a name that does not exist yet."""
    deadline = time.time() + 300
    while time.time() < deadline:
        pods = json.loads(kubectl("-n", NS, "get", "pods", "-o", "json"))["items"]
        for p in pods:
            if not p["metadata"]["name"].startswith(CDC) or "deletionTimestamp" in p["metadata"]:
                continue
            if any(c["type"] == "Ready" and c["status"] == "True" for c in p["status"].get("conditions", [])):
                return p
        time.sleep(1)
    sys.exit("the controller never became Ready")


def one(n, limit, threshold, run, count, payload, mode="steady"):
    set_arm(limit, threshold)
    marker = f"{int(time.time())}"
    if mode == "restart":
        # Stop the controller, queue the backlog, then start it into the backlog. core-provider
        # restores a Deployment scaled by hand, so it has to stop first or the pod comes back.
        kubectl("-n", "krateo-system", "scale", "deploy/core-provider", "--replicas=0")
        kubectl("-n", "krateo-system", "rollout", "status", "deploy/core-provider", "--timeout=120s")
        kubectl("-n", NS, "scale", f"deploy/{CDC}", "--replicas=0")
        wait_no_pods()
        apply_all(n, marker, count, payload)
        kubectl("-n", NS, "scale", f"deploy/{CDC}", "--replicas=1")
        pod = wait_ready_pod()
    else:
        pod = cdc_pod()
        time.sleep(STEADY_WAIT)
    name = pod["metadata"]["name"]
    node = pod["spec"]["nodeName"]
    container = pod["spec"]["containers"][0]["name"]

    start = sample(node, name, container)
    t0 = time.time()
    if mode == "steady":
        apply_all(n, marker, count, payload)
    samples = [start]
    done, total = 0, 0
    while time.time() - t0 < DRAIN_TIMEOUT:
        done, total = sentinels(marker)
        if done == n:
            break
        samples.append(sample(node, name, container))
        time.sleep(3)
    t1 = time.time()
    end = sample(node, name, container)
    samples.append(end)
    if mode == "restart":
        kubectl("-n", "krateo-system", "scale", "deploy/core-provider", "--replicas=1")
        kubectl("-n", "krateo-system", "rollout", "status", "deploy/core-provider", "--timeout=120s")

    # The driver runs on a laptop. A sleep during a run stops the sampling but not the controller, so
    # the drain time and every rate below would be wrong while the run still looks complete.
    gaps = [b["t"] - a["t"] for a, b in zip(samples, samples[1:])]
    max_gap = max(gaps) if gaps else 0

    wall = end["t"] - start["t"]
    limit_cores = normal_cpu(limit) / 1000
    usage, waiting = delta(start, end, "usage"), delta(start, end, "waiting")
    periods, throttled = delta(start, end, "periods"), delta(start, end, "throttled")
    pressures = [s["input"] for s in samples if s["input"] is not None]
    # The collector keeps a series after its pod is gone, so an off arm would read the last on arm's.
    locals_ = [s["local"] for s in samples if s["local"] is not None] if threshold != "off" else []
    if threshold == "off":
        pressures = []
    if threshold != "off" and not locals_:
        sys.exit(f"an on-arm collected no `local` samples: the collector is not receiving ({limit}, {threshold})")
    live = cdc_pod()
    unhealthy = json.loads(kubectl("-n", NS, "get", "events", "--field-selector",
                                   f"involvedObject.name={name},reason=Unhealthy", "-o", "json"))["items"]
    result = {
        "mode": mode, "limit": limit, "threshold": threshold, "run": run,
        "objects": n, "count": count, "payloadKB": payload,
        "chart": os.environ["CHART_VERSION"], "drained": done, "sentinels": total, "complete": done == n,
        "drain_s": round(t1 - t0, 1),
        # A restarted controller counts from zero, and the start sample may hold the old pod's total.
        "reconciles": end["reconciled"] if mode == "restart" else delta(start, end, "reconciled"),
        "pressure_mean_pct": round(100 * waiting / wall, 1) if waiting is not None else None,
        "psi_avg10_max": max(pressures) if pressures else None,
        "local_mean": round(sum(locals_) / len(locals_), 2) if locals_ else None,
        "local_samples": len(locals_),
        "quota_unused_pct": round(100 * (1 - usage / (limit_cores * wall)), 1) if usage is not None else None,
        "throttled_pct": round(100 * throttled / periods, 1) if periods else None,
        "deferred": {k: end["deferred"].get(k, 0) - start["deferred"].get(k, 0) for k in end["deferred"]},
        "restarts": sum(c["restartCount"] for c in live["status"].get("containerStatuses", [])),
        "same_pod": live["metadata"]["name"] == name,
        "readiness_failures": len(unhealthy),  # the cdc has a readiness probe and no liveness probe
        "node_cores": round(delta(start, end, "node") / wall, 2) if delta(start, end, "node") is not None else None,
        "samples": len(samples),
        "max_sample_gap_s": round(max_gap, 1),
        "discard": f"driver suspended: {max_gap:.0f}s between samples" if max_gap > MAX_SAMPLE_GAP else None,
    }
    os.makedirs(os.path.dirname(RESULTS), exist_ok=True)
    with open(RESULTS, "a") as f:
        f.write(json.dumps(result) + "\n")
    print(json.dumps(result), flush=True)
    return result


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("cmd", choices=["setup", "one", "sweep", "remeasure"])
    ap.add_argument("--objects", type=int, default=64)
    ap.add_argument("--count", type=int, default=40)
    ap.add_argument("--payload", type=int, default=4)
    ap.add_argument("--limit", default="250m")
    ap.add_argument("--threshold", default="off")
    ap.add_argument("--runs", type=int, default=3)
    ap.add_argument("--drain-timeout", type=int, default=DRAIN_TIMEOUT)
    ap.add_argument("--mode", default="steady", choices=["steady", "restart"])
    a = ap.parse_args()
    guard_context()
    globals()["DRAIN_TIMEOUT"] = a.drain_timeout

    if a.cmd == "setup":
        apply_all(a.objects, "setup", a.count, a.payload)
        deadline = time.time() + 1800
        while time.time() < deadline:
            done, total = sentinels("setup")
            print(f"setup: {done} of {a.objects} sentinels", flush=True)
            if done == a.objects:
                return
            time.sleep(15)
        sys.exit("setup did not converge")
    if a.cmd == "one":
        one(a.objects, a.limit, a.threshold, 0, a.count, a.payload, a.mode)
        return

    done = {}
    if os.path.exists(RESULTS):
        for line in open(RESULTS):
            r = json.loads(line)
            if r.get("chart") == os.environ["CHART_VERSION"]:
                key = (r["mode"], r["limit"], r["threshold"], r["run"])
                done[key] = done.get(key, 0) + 1

    def planned(mode, limit, threshold, run):
        key = (mode, limit, threshold, run)
        if done.get(key, 0) > 0:
            done[key] -= 1
            return
        one(a.objects, limit, threshold, run, a.count, a.payload, mode)

    if a.cmd == "remeasure":
        # #72: the same arms that carried the 3.3x, on the fixed build, nothing else.
        for run in range(1, a.runs + 1):
            for mode in ("steady", "restart"):
                for threshold in ("off", "50"):
                    planned(mode, "250m", threshold, run)
        return

    # Alternated within each repetition, so drift over the morning cannot sit in one arm.
    for run in range(1, a.runs + 1):
        for limit in ("250m", "1000m"):
            for threshold in ("off", "50", "off", "25", "off", "75"):
                planned("steady", limit, threshold, run)
            # The ramp's own case: threshold 50 against off, kept apart from the steady figures.
            for threshold in ("off", "50"):
                planned("restart", limit, threshold, run)


if __name__ == "__main__":
    main()
