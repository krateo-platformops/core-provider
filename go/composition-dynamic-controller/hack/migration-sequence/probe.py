#!/usr/bin/env python3
"""Can a StatefulSet be moved onto volumeClaimTemplates by a script running outside the chart upgrade?

Measures, on a composition whose chart holds a StatefulSet with no volumeClaimTemplates:
  1. how fast the cdc recreates a deleted StatefulSet with no pause;
  2. under each pause annotation, whether a manual scale to 0 holds across a resync, and whether a
     deleted StatefulSet stays deleted;
  3. whether a parent chart that renders the composition undoes the pause, or moves it anyway;
  4. moving to a chart with volumeClaimTemplates with a PVC of the expected name already present: is
     that same PVC adopted, and does the release end `deployed`.

The PVC carries a uid and a label, so "adopted" means the same object rather than a new one by name.
"""
import json, os, subprocess, sys, time

NS, GROUP = "seq", "composition.krateo.io"
BASE = "http://chartrepo.chartrepo.svc.cluster.local"
CTX = "gke_integration-test-431120_us-central1-a_cpu-sat-66"
RESYNC_WAIT = 150  # more than two 60s resyncs
OUT = {}


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


def cd(name, url):
    return json.dumps({"apiVersion": "core.krateo.io/v1alpha1", "kind": "CompositionDefinition",
                       "metadata": {"name": name, "namespace": NS}, "spec": {"chart": {"url": url}}})


def ready_controller(plural, version):
    wait(f"{plural} {version} controller", lambda: k("-n", NS, "get", "deploy", f"{plural}-{version}-controller", "-o",
         "jsonpath={.status.readyReplicas}", check=False).stdout.strip() == "1", 600)


def sts(rel):
    r = k("-n", NS, "get", "sts", f"{rel}-db", "-o", "json", check=False)
    return json.loads(r.stdout) if r.returncode == 0 else None


def cr():
    r = k("-n", NS, "get", f"seqprobes.{GROUP}", "c", "-o", "json", check=False)
    return json.loads(r.stdout) if r.returncode == 0 else None


def rel_of(o):
    return o["metadata"].get("labels", {}).get("krateo.io/release-name", "c")


def conds(o):
    return {c["type"]: (c["status"], c.get("reason")) for c in o.get("status", {}).get("conditions", []) if c["type"] in ("Ready", "Synced")}


def annotate(key, value):
    if value is None:
        k("-n", NS, "annotate", f"seqprobes.{GROUP}", "c", f"{key}-")
    else:
        k("-n", NS, "annotate", "--overwrite", f"seqprobes.{GROUP}", "c", f"{key}={value}")


def recreate_time(rel):
    k("-n", NS, "delete", "sts", f"{rel}-db", "--wait=true")
    t0 = time.time()
    while time.time() - t0 < 300:
        if sts(rel):
            return round(time.time() - t0, 1)
        time.sleep(2)
    return None


def pause_case(key, rel):
    res = {}
    wait("sts present", lambda: sts(rel), 300)
    k("-n", NS, "scale", "sts", f"{rel}-db", "--replicas=1")
    annotate(key, "true")
    time.sleep(10)
    k("-n", NS, "scale", "sts", f"{rel}-db", "--replicas=0")
    time.sleep(RESYNC_WAIT)
    s = sts(rel)
    res["replicas_after_scale_and_two_resyncs"] = s["spec"]["replicas"] if s else None
    res["conds_paused"] = conds(cr())
    k("-n", NS, "delete", "sts", f"{rel}-db", "--wait=true")
    time.sleep(RESYNC_WAIT)
    res["recreated_while_paused"] = sts(rel) is not None
    annotate(key, None)
    t0 = time.time()
    while time.time() - t0 < 300 and not sts(rel):
        time.sleep(2)
    res["recreated_after_unpause_s"] = round(time.time() - t0, 1) if sts(rel) else None
    return res


def main():
    if k("config", "current-context").stdout.strip() != CTX:
        sys.exit("refusing: wrong cluster")
    k("create", "namespace", NS, check=False)

    k("apply", "-f", "-", stdin=cd("seqprobe", f"{BASE}/seqprobe-0.1.0.tgz"))
    ready_controller("seqprobes", "v0-1-0")
    k("apply", "-f", "-", stdin=cd("seqparent", f"{BASE}/seqparent-0.1.0.tgz"))
    ready_controller("seqparents", "v0-1-0")

    # The parent renders composition `c`, as an umbrella does.
    k("apply", "-f", "-", stdin=json.dumps({"apiVersion": f"{GROUP}/v0-1-0", "kind": "Seqparent",
      "metadata": {"name": "p", "namespace": NS}, "spec": {"marker": "one"}}))
    o = wait("child and its StatefulSet", lambda: (lambda x: x if x and sts(rel_of(x)) else None)(cr()), 400)
    rel = rel_of(o)
    OUT["release"] = rel

    OUT["no_pause_recreate_s"] = recreate_time(rel)
    print("no pause", OUT["no_pause_recreate_s"], flush=True)
    for key in ("krateo.io/paused", "krateo.io/gracefully-paused"):
        OUT[key] = pause_case(key, rel)
        print(key, OUT[key], flush=True)

    # Does the parent undo the pause, or move the paused child?
    annotate("krateo.io/paused", "true")
    k("-n", NS, "patch", f"seqparents.{GROUP}", "p", "--type=merge", "-p", '{"spec":{"marker":"two"}}')
    time.sleep(RESYNC_WAIT)
    x = cr()
    OUT["parent"] = {"pause_annotation_kept": x["metadata"].get("annotations", {}).get("krateo.io/paused"),
                     "child_spec_marker": x["spec"].get("marker"),
                     "child_cm_marker": k("-n", NS, "get", "cm", f"{rel}-marker", "-o", "jsonpath={.data.marker}", check=False).stdout,
                     "conds": conds(x)}
    print("parent", OUT["parent"], flush=True)

    # The migration itself, paused: remove the workload, leave the claim, move to B, resume.
    k("-n", NS, "delete", "sts", f"{rel}-db", "--wait=true", check=False)
    pvc = f"data-{rel}-db-0"
    k("apply", "-f", "-", stdin=json.dumps({"apiVersion": "v1", "kind": "PersistentVolumeClaim",
      "metadata": {"name": pvc, "namespace": NS, "labels": {"migrated-by": "script"}},
      "spec": {"accessModes": ["ReadWriteOnce"], "resources": {"requests": {"storage": "1Gi"}}}}))
    uid = json.loads(k("-n", NS, "get", "pvc", pvc, "-o", "json").stdout)["metadata"]["uid"]
    k("apply", "-f", "-", stdin=cd("seqprobe", f"{BASE}/seqprobe-0.2.0.tgz"))
    time.sleep(RESYNC_WAIT)
    OUT["while_paused_at_B"] = {"sts": sts(rel) is not None, "label": (cr() or {}).get("metadata", {}).get("labels", {}).get("krateo.io/composition-version")}
    print("paused at B", OUT["while_paused_at_B"], flush=True)
    annotate("krateo.io/paused", None)

    wait("the StatefulSet with a claim", lambda: (lambda s: s if s and s["spec"].get("volumeClaimTemplates") else None)(sts(rel)), 500)
    k("-n", NS, "scale", "sts", f"{rel}-db", "--replicas=1")
    wait("the pod ready", lambda: k("-n", NS, "get", "pod", f"{rel}-db-0", "-o", "jsonpath={.status.containerStatuses[0].ready}", check=False).stdout.strip() == "true", 300)
    p = json.loads(k("-n", NS, "get", "pvc", pvc, "-o", "json").stdout)
    rels = json.loads(k("-n", NS, "get", "secrets", "-l", f"owner=helm,name={rel}", "-o", "json").stdout)["items"]
    x = cr()
    OUT["adoption"] = {"same_pvc": p["metadata"]["uid"] == uid, "phase": p["status"].get("phase"), "label": p["metadata"].get("labels", {}).get("migrated-by"),
                       "pod_claim": json.loads(k("-n", NS, "get", "pod", f"{rel}-db-0", "-o", "json").stdout)["spec"]["volumes"][0].get("persistentVolumeClaim"),
                       "revisions": sorted((int(s["metadata"]["labels"]["version"]), s["metadata"]["labels"]["status"]) for s in rels),
                       "conds": conds(x), "gen": x["metadata"].get("generation"), "obs": x["status"].get("observedGeneration"),
                       "version_label": x["metadata"].get("labels", {}).get("krateo.io/composition-version")}
    print("adoption", OUT["adoption"], flush=True)
    p = os.path.expanduser("~/krateo/cpu-sat-66-results/migration-sequence.json")
    json.dump(OUT, open(p, "w"), indent=1)
    print("written", p, flush=True)


main()
