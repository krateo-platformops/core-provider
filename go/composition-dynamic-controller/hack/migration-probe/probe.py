#!/usr/bin/env python3
"""What does a version migration do to a field the new version removes?

No etcd is needed. The old schema declares the field, so reading through the OLD endpoint AFTER the
migration decides it: endpoint pruning cannot explain its absence at an endpoint that declares it.

    before bump, via old   establishes that the field exists
    after,       via old   THE ANSWER
    after,       via new   expected to be pruned on the way out

The witness: the after-state is not read until `krateo.io/composition-version` has moved on every
instance, because that label moves in the same write that would prune the field. A read taken before
it moves measures some other write.

Prune would remove the old endpoint right after the migration, taking the answer with it. So a helm
release holds it open by naming the old version in its stored manifest (#305). Uninstalling that
release is then the moment the last reference goes.

KUBECONFIG must point at the disposable cluster. The script refuses any other context.
"""
import json
import os
import subprocess
import sys
import tempfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
NS = "mig"
GROUP = "composition.krateo.io"
OLD, NEW = "v0-1-0", "v0-2-0"
CRD = f"migprobes.{GROUP}"
LABEL = "krateo.io/composition-version"
FIELD_VALUE = "SHOULD-IT-SURVIVE-MIGRATION"
INSTANCES = ["one", "two"]
ALL = INSTANCES + ["hold"]
BASE = "http://chartrepo.chartrepo.svc.cluster.local"
CONTEXT = "gke_integration-test-431120_us-central1-a_cpu-sat-66"


def sh(*a, check=True, stdin=None):
    r = subprocess.run(a, capture_output=True, text=True, input=stdin)
    if check and r.returncode != 0:
        sys.exit(f"FAILED: {' '.join(a)[:150]}\n{(r.stdout + r.stderr)[-500:]}")
    return r


def k(*a, check=True, stdin=None):
    return sh("kubectl", *a, check=check, stdin=stdin)


def read(version, name):
    """The object as this endpoint serves it, or None if it does not answer."""
    r = k("get", "--raw", f"/apis/{GROUP}/{version}/namespaces/{NS}/migprobes/{name}", check=False)
    return json.loads(r.stdout) if r.returncode == 0 else None


def view(o):
    if o is None:
        return None
    m = o["metadata"]
    return {"apiVersion": o["apiVersion"], "spec": o.get("spec"), "label": m.get("labels", {}).get(LABEL),
            "uid": m["uid"], "generation": m.get("generation"), "resourceVersion": m["resourceVersion"],
            "annotations": {x: y for x, y in m.get("annotations", {}).items() if "krateo" in x}}


def crd_versions():
    d = json.loads(k("get", "crd", CRD, "-o", "json").stdout)
    return {v["name"]: {"served": v["served"], "storage": v["storage"]} for v in d["spec"]["versions"]}, d


def definition(url):
    return json.dumps({"apiVersion": "core.krateo.io/v1alpha1", "kind": "CompositionDefinition",
                       "metadata": {"name": "migprobe", "namespace": NS}, "spec": {"chart": {"url": url}}})


def wait(what, ok, seconds):
    deadline = time.time() + seconds
    while time.time() < deadline:
        v = ok()
        if v:
            return v
        time.sleep(5)
    sys.exit(f"TIMED OUT: {what}")


def snapshot(stage, out):
    out[stage] = {
        "at": time.strftime("%H:%M:%SZ", time.gmtime()),
        "versions": crd_versions()[0],
        "versionInfo": json.loads(k("-n", NS, "get", "compositiondefinition", "migprobe", "-o", "json").stdout)
                       .get("status", {}).get("managed", {}).get("versionInfo"),
        "instances": {n: {OLD: view(read(OLD, n)), NEW: view(read(NEW, n))} for n in ALL},
        "controllers": k("-n", NS, "get", "deploy", "--no-headers", "-o", "custom-columns=N:.metadata.name,R:.status.readyReplicas").stdout.split("\n")[:-1],
        "pruned_events": [e.get("message", "")[:240] for e in json.loads(
            k("-n", NS, "get", "events", "--field-selector", "reason=CompositionFieldsPruned", "-o", "json").stdout)["items"]],
    }


def main():
    if k("config", "current-context").stdout.strip() != CONTEXT:
        sys.exit("refusing: wrong cluster")
    out = {}

    k("create", "namespace", NS, check=False)
    k("apply", "-f", "-", stdin=definition(f"{BASE}/migprobe-0.1.0.tgz"))
    wait("the v0-1-0 CRD and controller", lambda: read(OLD, "none") is None and k(
        "-n", NS, "get", "deploy", "migprobes-v0-1-0-controller", "-o", "jsonpath={.status.readyReplicas}",
        check=False).stdout.strip() == "1", 600)

    for n in INSTANCES:
        k("apply", "-f", "-", stdin=json.dumps({
            "apiVersion": f"{GROUP}/{OLD}", "kind": "Migprobe", "metadata": {"name": n, "namespace": NS},
            "spec": {"keptField": "kept", "removedField": FIELD_VALUE}}))
    sh("helm", "install", "hold", f"{HERE}/holdref", "-n", NS)
    wait("owner labels on all three", lambda: all(
        "krateo.io/composition-definition-name" in (read(OLD, n) or {}).get("metadata", {}).get("labels", {})
        for n in ALL), 600)

    snapshot("before", out)
    field = (out["before"]["instances"]["one"][OLD] or {}).get("spec", {}).get("removedField")
    if field != FIELD_VALUE:
        sys.exit(f"the control failed: the field is not readable before the bump: {field!r}")
    print("control: the field reads through the old endpoint before the bump", flush=True)

    k("apply", "-f", "-", stdin=definition(f"{BASE}/migprobe-0.2.0.tgz"))
    t0 = time.time()

    def moved():
        labels = {n: (view(read(NEW, n)) or {}).get("label") for n in ALL}
        print(f"  {int(time.time() - t0)}s labels {labels}", flush=True)
        return all(v == NEW for v in labels.values())

    wait("the label to move on every instance", moved, 1500)
    out["witness"] = {"label_moved_after_s": int(time.time() - t0), "instances": ALL}
    snapshot("after", out)

    # Vacuum's shape decides whether storage itself could prune.
    d = crd_versions()[1]
    v = [x for x in d["spec"]["versions"] if x["name"] == "vacuum"]
    out["vacuum"] = {"present": bool(v), "served": v[0]["served"] if v else None, "storage": v[0]["storage"] if v else None,
                     "spec_schema": v[0]["schema"]["openAPIV3Schema"]["properties"].get("spec") if v else None,
                     "conversion": d["spec"].get("conversion", {}).get("strategy")}

    # The last reference goes: does the old version leave, and how soon?
    out["held_open"] = OLD in crd_versions()[0]
    sh("helm", "uninstall", "hold", "-n", NS, "--wait", "--timeout", "5m", check=False)
    t1 = time.time()
    gone = None
    while time.time() - t1 < 900:
        if OLD not in crd_versions()[0]:
            gone = int(time.time() - t1)
            break
        time.sleep(10)
    out["old_version_pruned_after_s"] = gone
    snapshot("end", out)

    path = os.path.expanduser("~/krateo/cpu-sat-66-results/migration-probe.json")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    json.dump(out, open(path, "w"), indent=1)
    print("written", path, flush=True)


main()
