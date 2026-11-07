#!/usr/bin/env python3
"""When does a pre-upgrade hook run under the cdc, and what does an immutable refusal do?

The cdc does not run `helm upgrade`: it patches the live objects, then runs a real Upgrade only to write
the revision and run hooks. So a pre-upgrade hook should see the NEW value already on the live object.
The hook job prints what it read, which is the witness: it comes from the cluster, not from a log line.

KUBECONFIG must point at the disposable cluster.
"""
import json, os, subprocess, sys, time

HERE = os.path.dirname(os.path.abspath(__file__))
NS, CD, BASE = "hooks", "hookprobe", "http://chartrepo.chartrepo.svc.cluster.local"
CTX = "gke_integration-test-431120_us-central1-a_cpu-sat-66"
GROUP, OLD = "composition.krateo.io", "v0-1-0"
RES = {}


def k(*a, check=True, stdin=None):
    r = subprocess.run(["kubectl", *a], capture_output=True, text=True, input=stdin)
    if check and r.returncode != 0:
        sys.exit(f"FAILED kubectl {' '.join(a)[:120]}\n{(r.stdout + r.stderr)[-400:]}")
    return r


def wait(what, ok, secs):
    end = time.time() + secs
    while time.time() < end:
        v = ok()
        if v:
            return v
        time.sleep(5)
    sys.exit(f"TIMED OUT: {what}")


def cd(url):
    return json.dumps({"apiVersion": "core.krateo.io/v1alpha1", "kind": "CompositionDefinition",
                       "metadata": {"name": CD, "namespace": NS}, "spec": {"chart": {"url": url}}})


def comp(version, marker):
    return json.dumps({"apiVersion": f"{GROUP}/{version}", "kind": "Hookprobe",
                       "metadata": {"name": "p", "namespace": NS}, "spec": {"marker": marker}})


def set_marker(version, marker):
    """A write in the moment after a prune can fail conversion, so the bump's own settling is retried."""
    end = time.time() + 120
    while True:
        r = k("apply", "--server-side", "--force-conflicts", "--field-manager=probe", "-f", "-", stdin=comp(version, marker), check=False)
        if r.returncode == 0:
            return
        if time.time() > end:
            sys.exit(r.stderr[-300:])
        time.sleep(5)


def get():
    r = k("-n", NS, "get", f"hookprobes.{GROUP}", "p", "-o", "json", check=False)
    return json.loads(r.stdout) if r.returncode == 0 else None


def release(o):
    return o["metadata"].get("labels", {}).get("krateo.io/release-name", "p")


def secrets():
    r = json.loads(k("-n", NS, "get", "secrets", "-l", "owner=helm", "-o", "json").stdout)["items"]
    return sorted((int(s["metadata"]["labels"]["version"]), s["metadata"]["labels"]["status"]) for s in r)


def cm(rel, suffix):
    r = k("-n", NS, "get", "cm", f"{rel}-{suffix}", "-o", "jsonpath={.data.marker}", check=False)
    return r.stdout if r.returncode == 0 else None


def conds(o):
    return {c["type"]: (c["status"], c.get("reason"), (c.get("message") or "")[:220]) for c in o.get("status", {}).get("conditions", [])}


def main():
    if k("config", "current-context").stdout.strip() != CTX:
        sys.exit("refusing: wrong cluster")
    k("create", "namespace", NS, check=False)
    k("-n", NS, "create", "sa", "hookprobe", check=False)
    k("-n", NS, "create", "rolebinding", "hookprobe", "--clusterrole=view", f"--serviceaccount={NS}:hookprobe", check=False)
    k("apply", "-f", "-", stdin=cd(f"{BASE}/hookprobe-0.1.0.tgz"))
    wait("controller", lambda: k("-n", NS, "get", "deploy", "hookprobes-v0-1-0-controller", "-o", "jsonpath={.status.readyReplicas}", check=False).stdout.strip() == "1", 600)
    k("apply", "-f", "-", stdin=comp(OLD, "one"))
    o = wait("first release", lambda: (lambda x: x if x and secrets() and cm(release(x), "first") == "one" else None)(get()), 300)
    rel = release(o)
    RES["base"] = {"revisions": secrets(), "first": cm(rel, "first")}
    print("base", RES["base"], flush=True)

    # A: a real change with a pre-upgrade hook, on a chart bump.
    k("apply", "-f", "-", stdin=cd(f"{BASE}/hookprobe-0.2.0.tgz"))
    wait("v0-2-0", lambda: (get() or {}).get("metadata", {}).get("labels", {}).get("krateo.io/composition-version") == "v0-2-0", 600)
    set_marker("v0-2-0", "two")
    job = f"{rel}-pre-two"

    def done():
        r = k("-n", NS, "get", "job", job, "-o", "jsonpath={.status.succeeded}", check=False)
        return r.stdout.strip() == "1"

    wait("the hook job", done, 400)
    logs = k("-n", NS, "logs", f"job/{job}").stdout.strip()
    RES["hook"] = {"job_log": logs, "live_first_now": cm(rel, "first"), "revisions": secrets()}
    print("hook", RES["hook"], flush=True)

    # B: an immutable refusal. zlast sorts after the StatefulSet: is a partial apply left behind?
    before = secrets()
    k("apply", "-f", "-", stdin=cd(f"{BASE}/hookprobe-0.3.0.tgz"))
    wait("v0-3-0", lambda: (get() or {}).get("metadata", {}).get("labels", {}).get("krateo.io/composition-version") == "v0-3-0", 600)
    set_marker("v0-3-0", "three")
    t0 = time.time()
    samples = []
    while time.time() - t0 < 300:
        x = get()
        samples.append({"t": int(time.time() - t0), "conds": {a: b[:2] for a, b in conds(x).items() if a in ("Ready", "Synced")},
                        "gen": x["metadata"]["generation"], "obs": x["status"].get("observedGeneration")})
        time.sleep(30)
    x = get()
    RES["refusal"] = {"revisions_before": before, "revisions_after": secrets(), "first": cm(rel, "first"), "zlast": cm(rel, "zlast"),
                      "conds": conds(x), "samples": samples[-3:], "pods": k("-n", NS, "get", "pods", "--no-headers").stdout.strip()[:200]}
    print("refusal", json.dumps(RES["refusal"])[:900], flush=True)
    p = os.path.expanduser("~/krateo/cpu-sat-66-results/hook-probe.json")
    json.dump(RES, open(p, "w"), indent=1)
    print("written", p, flush=True)


main()
