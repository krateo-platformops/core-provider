#!/usr/bin/env python3
"""#382 / #383: during a core-provider upgrade, how many core-provider pods reconcile at once, and how
many times is the cdc rolled?

A differential on one cluster. Each arm is an upgrade that changes the cdc's env (so old and new
core-provider disagree, which is what makes them fight), measured from the moment helm starts:

  control   the published chart (leader election off by default)
  subject   the #383 chart from a local path (leader election on by default), same images

Counted from the API server, not from logs: new cdc ReplicaSets created after the upgrade began, and
the distinct core-provider pods alive during it. "Reconciling" is read from core-provider's logs per
pod, with the total log lines per pod beside it, so a zero from an unreadable log cannot pass as one.
"""
import argparse, json, os, subprocess, sys, threading, time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import run  # noqa: E402  (the harness: kubectl, sh, guard_context, constants)


def rs_created_after(t0):
    items = json.loads(run.kubectl("-n", run.NS, "get", "rs", "-l", f"app.kubernetes.io/name={run.CDC}", "-o", "json"))["items"]
    return sorted(i["metadata"]["creationTimestamp"] for i in items if i["metadata"]["creationTimestamp"] >= t0)


def cp_pods():
    return json.loads(run.kubectl("-n", "krateo-system", "get", "pods", "-l",
                                  "app.kubernetes.io/name=core-provider,app.kubernetes.io/instance=core-provider", "-o", "json"))["items"]


def upgrade(chart, version, saturation):
    args = ["helm", "upgrade", "core-provider", chart, "-n", "krateo-system", "-f", f"{run.HERE}/values-base.yaml",
            "--set", "cdc.resources.limits.cpu=250m",
            "--set-string", f"cdc.env.COMPOSITION_CONTROLLER_SATURATION_ENABLED={saturation}",
            "--set-string", f"cdc.env.COMPOSITION_CONTROLLER_CPU_PRESSURE_THRESHOLD={'50' if saturation == 'true' else '0'}",
            "--wait", "--timeout", "5m"]
    if version:
        args += ["--version", version]
    run.sh(*args)


def arm(name, chart, version, saturation, watch_s=120):
    run.settle("one core-provider pod", run.one_core_provider)
    run.settle("the cdc Deployment, one pod, no rollout", run.cdc_stable)
    t0 = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    # The OLD pod must be in the sample. helm --wait returns after the new pod is Ready, and the old one
    # can be gone by then: the first version of this probe sampled only afterwards and saw one pod in
    # every arm, which measured nothing about overlap. So: snapshot before, and sample during.
    seen = {pod["metadata"]["name"]: True for pod in cp_pods()}
    before = set(seen)
    done = threading.Event()

    def sample():
        while not done.is_set():
            try:
                for pod in cp_pods():
                    seen[pod["metadata"]["name"]] = pod["metadata"].get("deletionTimestamp") is None
            except SystemExit:
                pass
            time.sleep(1)

    th = threading.Thread(target=sample, daemon=True)
    th.start()
    upgrade(chart, version, saturation)
    end = time.time() + watch_s
    while time.time() < end:
        time.sleep(2)
    done.set()
    th.join()
    # Cloud Logging, not kubectl logs: the OLD pod is gone by now, and a log that cannot be read must
    # not count as a pod that did nothing. Every pod's total line count is the control.
    time.sleep(60)  # ingestion delay
    t1 = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(end))
    logs = {}
    for name_ in seen:
        q = (f'resource.type="k8s_container" AND resource.labels.cluster_name="cpu-sat-66" '
             f'AND resource.labels.pod_name="{name_}" AND timestamp>="{t0}" AND timestamp<="{t1}"')
        r = subprocess.run(["gcloud", "logging", "read", q, "--project", "integration-test-431120", "--limit", "5000",
                            "--format=value(jsonPayload.msg,jsonPayload.name)"], capture_output=True, text=True)
        lines = r.stdout.splitlines()
        logs[name_] = {"readable": r.returncode == 0, "lines": len(lines),
                       "updates": sum(1 for l in lines if l.startswith("Updating CompositionDefinition") and "loadchart" in l)}
    lease = subprocess.run(["kubectl", "-n", "krateo-system", "get", "lease", "leader-election-core-provider",
                            "-o", "jsonpath={.spec.holderIdentity}"], capture_output=True, text=True)
    out = {"arm": name, "t0": t0, "new_cdc_replicasets": len(rs_created_after(t0)), "core_provider_pods": seen,
           "old_pods": sorted(before),
           "updates_by_pod": logs, "lease_holder": lease.stdout.strip() if lease.returncode == 0 else None}
    print(json.dumps(out), flush=True)
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--subject-chart", required=True, help="local path to the #383 chart with versions filled in")
    ap.add_argument("--published", default="2.13.73")
    ap.add_argument("--runs", type=int, default=5)
    a = ap.parse_args()
    run.guard_context()
    results = []
    # Bring the published chart in first, so the control starts from it.
    upgrade(run.CHART, a.published, "false")
    # Blocks, not alternation: a subject upgrade must start from a pod that already elects, so an
    # alternating design would make every subject arm a transition.
    sat = "true"
    for i in range(1, a.runs + 1):
        results.append(arm(f"control #{i}: published chart, saturation -> {sat}", run.CHART, a.published, sat))
        sat = "false" if sat == "true" else "true"
    # The transition itself: the OLD pod still runs without election, so it may fight once more.
    results.append(arm("transition: to the #383 chart", a.subject_chart, None, sat))
    sat = "false" if sat == "true" else "true"
    for i in range(1, a.runs + 1):
        results.append(arm(f"subject #{i}: #383 chart, saturation -> {sat}", a.subject_chart, None, sat))
        sat = "false" if sat == "true" else "true"
    p = os.path.expanduser("~/krateo/cpu-sat-66-results/leader-probe.json")
    json.dump(results, open(p, "w"), indent=1)
    print("written", p, flush=True)


main()
