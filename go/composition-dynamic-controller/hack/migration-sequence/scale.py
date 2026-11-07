#!/usr/bin/env python3
"""Does a manual scale to 0 hold across resyncs, with and without each pause annotation?

The chart renders replicas: 1, so 0 is a change away from what the chart wants. probe.py's chart
rendered 0, which made its version of this question unable to fail. The no-pause arm is the control:
helm's three-way merge may leave a manual scale alone by itself, and then a pause is not what holds it.
"""
import json, os, subprocess, sys, time

NS, GROUP, CTX = "seq1", "composition.krateo.io", "gke_integration-test-431120_us-central1-a_cpu-sat-66"
BASE = "http://chartrepo.chartrepo.svc.cluster.local"
RESYNC_WAIT = 150


def k(*a, check=True, stdin=None):
    r = subprocess.run(["kubectl", *a], capture_output=True, text=True, input=stdin)
    if check and r.returncode != 0:
        sys.exit(f"FAILED kubectl {' '.join(a)[:130]}\n{(r.stdout + r.stderr)[-400:]}")
    return r


def wait(what, ok, secs):
    end = time.time() + secs
    while time.time() < end:
        v = ok()
        if v:
            return v
        time.sleep(3)
    sys.exit(f"TIMED OUT: {what}")


def replicas(rel):
    r = k("-n", NS, "get", "sts", f"{rel}-db", "-o", "jsonpath={.spec.replicas}", check=False)
    return int(r.stdout) if r.returncode == 0 and r.stdout else None


def main():
    if k("config", "current-context").stdout.strip() != CTX:
        sys.exit("refusing: wrong cluster")
    k("create", "namespace", NS, check=False)
    k("apply", "-f", "-", stdin=json.dumps({"apiVersion": "core.krateo.io/v1alpha1", "kind": "CompositionDefinition",
      "metadata": {"name": "seqone", "namespace": NS}, "spec": {"chart": {"url": f"{BASE}/seqone-0.1.0.tgz"}}}))
    wait("controller", lambda: k("-n", NS, "get", "deploy", "seqones-v0-1-0-controller", "-o", "jsonpath={.status.readyReplicas}", check=False).stdout.strip() == "1", 600)
    k("apply", "-f", "-", stdin=json.dumps({"apiVersion": f"{GROUP}/v0-1-0", "kind": "Seqone", "metadata": {"name": "c", "namespace": NS}, "spec": {"marker": "one"}}))

    def rel():
        r = k("-n", NS, "get", f"seqones.{GROUP}", "c", "-o", "json", check=False)
        if r.returncode:
            return None
        return json.loads(r.stdout)["metadata"].get("labels", {}).get("krateo.io/release-name")

    name = wait("release", lambda: (lambda n: n if n and replicas(n) == 1 else None)(rel()), 400)
    out = {"chart_replicas": 1}
    for arm, key in (("no pause (control)", None), ("krateo.io/paused", "krateo.io/paused"), ("krateo.io/gracefully-paused", "krateo.io/gracefully-paused")):
        k("-n", NS, "scale", "sts", f"{name}-db", "--replicas=1")
        wait("back at 1", lambda: replicas(name) == 1, 120)
        if key:
            k("-n", NS, "annotate", "--overwrite", f"seqones.{GROUP}", "c", f"{key}=true")
            time.sleep(10)
        k("-n", NS, "scale", "sts", f"{name}-db", "--replicas=0")
        seen = []
        for _ in range(RESYNC_WAIT // 15):
            time.sleep(15)
            seen.append(replicas(name))
        out[arm] = {"replicas_over_time": seen, "held": all(r == 0 for r in seen)}
        if key:
            k("-n", NS, "annotate", f"seqones.{GROUP}", "c", f"{key}-")
        print(arm, out[arm], flush=True)
    p = os.path.expanduser("~/krateo/cpu-sat-66-results/migration-scale.json")
    json.dump(out, open(p, "w"), indent=1)
    print("written", p, flush=True)


main()
