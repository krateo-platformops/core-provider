#!/usr/bin/env python3
"""Exercise core-provider#629 (interrupted fresh install -> automatic reinstall) on a real cluster.

Installs the RELEASED core-provider charts, then kills the real composition controller pod while a
chart's lifecycle hook Job is running. Needs a disposable cluster on kind: the SIGKILL arms reach the
node through `docker exec`. Standard library only.

  KUBECONFIG=... run.py --version 2.13.128 --node <kind node container>

Arms that decide the result (any failure exits 1):
  sigkill       fresh install, controller SIGKILLed mid post-install hook: reinstalled, hooks run again.
  kill-switch   same, composition annotated manual: kept, HooksMayNotHaveRun, nothing uninstalled.
  once-deployed CONTROL: a deployed release, upgrade interrupted the same way: never uninstalled.
  witness       CONTROL: deleting a composition DOES run the pre-delete hook, so its absence means something.

Arms that are measured: force-delete, lease-stolen, graceful-delete. Which state each leaves is the
question, so only a kill that leaves pending-install exercises #629 and is held to sigkill's checks.
Any other state is reported NOT REPRODUCED, never as a pass.
"""
import argparse, datetime, json, subprocess, sys, threading, time

NS, CP_NS = "exercise", "krateo-system"
SWITCH = "krateo.io/interrupted-install-recovery"
COUNTER = "krateo.io/interrupted-install-reinstalls"
UNVERIFIED = "krateo.io/hooks-unverified"
HOOK_SECONDS = 45
results = []  # (arm, verdict, detail)


def sh(*a, check=True, inp=None):
    p = subprocess.run(a, input=inp, capture_output=True, text=True)
    if check and p.returncode:
        raise RuntimeError(f"{' '.join(a)}: exit {p.returncode}: {p.stderr.strip()[:400]}")
    return p.stdout


def kget(kind, name=None, ns=NS, **kw):
    """The object (None only on NotFound), or the list's items. Any other error raises."""
    a = ["kubectl", "get", kind] + ([name] if name else []) + ["-n", ns, "-o", "json"]
    if kw.get("selector"):
        a += ["-l", kw["selector"]]
    p = subprocess.run(a, capture_output=True, text=True)
    if p.returncode:
        if name and "(NotFound)" in p.stderr:
            return None
        raise RuntimeError(f"get {kind} {name}: {p.stderr.strip()[:300]}")
    o = json.loads(p.stdout)
    return o if name else o["items"]


def uid(o):
    return o and o["metadata"]["uid"]


def wait(what, fn, timeout=240, every=1.0):
    end, last = time.time() + timeout, None
    while time.time() < end:
        last = fn()
        if last:
            return last
        time.sleep(every)
    raise TimeoutError(f"timed out after {timeout}s waiting for {what}")


def log(m):
    print(f"[{datetime.datetime.now(datetime.timezone.utc):%H:%M:%S}] {m}", flush=True)


class Arm:
    """One composition, and the timeline of its helm release Secrets (labels only, never their data)."""

    def __init__(self, name, gvr_version, annotations=None, hook_seconds=HOOK_SECONDS):
        self.name, self.rel, self.timeline, self.stop = name, f"rel-{name}", [], threading.Event()
        self.errors = 0
        self.manifest = {
            "apiVersion": f"composition.krateo.io/{gvr_version}", "kind": "Hookprobe",
            "metadata": {"name": name, "namespace": NS, "annotations": annotations or {},
                         "labels": {"krateo.io/release-name": self.rel,
                                    "krateo.io/composition-version": gvr_version}},
            "spec": {"rev": 1, "hookSeconds": hook_seconds}}

    def create(self):
        sh("kubectl", "apply", "-f", "-", inp=json.dumps(self.manifest))
        # The release name carries a random suffix unless one is recorded: read it, never assume it.
        self.rel = wait(f"{self.name}: release name", lambda: self._release(), 180)
        threading.Thread(target=self._watch, daemon=True).start()
        return self

    def _release(self):
        for s in kget("secrets", selector="owner=helm"):
            n = s["metadata"]["labels"]["name"]
            if n.startswith(f"rel-{self.name}") or n.startswith(self.name):
                return n
        return None

    def secrets(self):
        out = sh("kubectl", "get", "secrets", "-n", NS, "-l", f"owner=helm,name={self.rel}", "-o",
                 "jsonpath={range .items[*]}{.metadata.labels.version},{.metadata.labels.status},{.metadata.uid}{'\\n'}{end}")
        return tuple(sorted(tuple(l.split(",")) for l in out.splitlines() if l))

    def _watch(self):
        prev = None
        while not self.stop.is_set():
            try:
                cur = self.secrets()
                if cur != prev:
                    self.timeline.append((time.time(), cur))
                    prev = cur
            except Exception:  # a failed read is counted, never recorded as "no releases"
                self.errors += 1
            time.sleep(0.3)

    def show_timeline(self):
        t0 = self.timeline[0][0] if self.timeline else 0
        for t, s in self.timeline:
            print(f"      +{t - t0:6.1f}s  " + ("; ".join(f"v{v} {st} {u[:8]}" for v, st, u in s) or "(no release)"))
        print(f"      reads that failed: {self.errors}")

    def comp(self):
        return kget("hookprobes.composition.krateo.io", self.name)

    def ready(self):
        for c in ((self.comp() or {}).get("status") or {}).get("conditions") or []:
            if c["type"] == "Ready":
                return c["status"], c["reason"]
        return None, None

    def ann(self, k):
        return (self.comp()["metadata"].get("annotations") or {}).get(k)

    def job(self, which):
        return kget("jobs", f"{self.rel}-{which}")

    def complete(self, which):
        j = self.job(which)
        return bool(j) and any(c["type"] == "Complete" and c["status"] == "True" for c in j["status"].get("conditions", []))

    def app(self):
        return kget("configmaps", f"{self.rel}-app")

    def predelete(self):
        return kget("configmaps", f"{self.rel}-predelete")

    def events(self, reason):
        n = 0
        for e in kget("events.events.k8s.io"):
            if e.get("reason") == reason and (e.get("regarding") or {}).get("name") == self.name:
                n += 1
        return n

    def wait_hook_running(self, which):
        def running():
            for p in kget("pods", selector=f"job-name={self.rel}-{which}"):
                if p["status"].get("phase") == "Running":
                    return True
        wait(f"{self.name}: {which} hook pod Running", running, 180, 0.5)


def controller_pod():
    pods = [p for p in kget("pods", ns=CP_NS) if p["metadata"]["name"].startswith("hookprobes-")
            and not p["metadata"].get("deletionTimestamp")]
    ok = [p for p in pods if (p["status"].get("containerStatuses") or [{}])[0].get("ready")]
    return ok[0] if len(pods) == 1 and ok else None


def lease():
    return [l for l in kget("leases.coordination.k8s.io", ns=CP_NS)
            if l["metadata"]["name"].startswith("hookprobes-") or "hookprobe" in l["metadata"]["name"]]


def fresh_controller():
    """A new controller pod holding its Lease, so no arm inherits the previous one's restart backoff."""
    old = controller_pod()
    if old:
        sh("kubectl", "delete", "pod", "-n", CP_NS, old["metadata"]["name"], "--wait=true")
    pod = wait("a fresh controller pod", lambda: (lambda p: p if p and (not old or uid(p) != uid(old)) else None)(controller_pod()), 180)
    wait("the controller to hold its lease", lambda: any(l["spec"].get("holderIdentity") == pod["metadata"]["name"] for l in lease()), 120)
    return pod


def sigkill(node, pod):
    cid = pod["status"]["containerStatuses"][0]["containerID"].split("//")[1]
    pid = sh("docker", "exec", node, "crictl", "inspect", "-o", "go-template", "--template", "{{.info.pid}}", cid).strip()
    if not pid.isdigit() or int(pid) < 2:
        raise RuntimeError(f"no controller pid: {pid!r}")
    sh("docker", "exec", node, "kill", "-9", pid)


def kill(how, node, pod):
    name = pod["metadata"]["name"]
    if how == "sigkill":
        sigkill(node, pod)
    elif how == "force-delete":
        sh("kubectl", "delete", "pod", "-n", CP_NS, name, "--grace-period=0", "--force", "--wait=false")
    elif how == "graceful-delete":
        sh("kubectl", "delete", "pod", "-n", CP_NS, name, "--wait=false")
    elif how == "lease-stolen":
        ls = [l for l in lease() if l["spec"].get("holderIdentity") == name]
        if len(ls) != 1:
            raise RuntimeError(f"expected one lease held by {name}, found {len(ls)}")
        now = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")
        sh("kubectl", "patch", "lease", "-n", CP_NS, ls[0]["metadata"]["name"], "--type", "merge", "-p", json.dumps(
            {"spec": {"holderIdentity": "exercise-intruder", "renewTime": now, "acquireTime": now, "leaseDurationSeconds": 15}}))


def proof_of_death(how, before):
    """The kill must be seen to have happened: a kill that did nothing would make every arm vacuous."""
    if how in ("force-delete", "graceful-delete"):
        wait("a replacement controller pod", lambda: (lambda p: p and uid(p) != uid(before))(controller_pod()), 180)
        return "the pod was replaced"

    def restarted():
        p = kget("pods", before["metadata"]["name"], ns=CP_NS)
        cs = p["status"]["containerStatuses"][0]
        return cs if cs["restartCount"] > before["status"]["containerStatuses"][0]["restartCount"] else None
    cs = wait("the container to restart", restarted, 180)
    t = cs["lastState"].get("terminated") or {}
    if how == "sigkill" and t.get("exitCode") != 137:
        raise RuntimeError(f"expected exit 137 (SIGKILL), got {t}")
    return f"same pod, container restarted, previous exit code {t.get('exitCode')}"


def check(fails, cond, msg):
    print(f"      {'ok  ' if cond else 'FAIL'} {msg}", flush=True)
    if not cond:
        fails.append(msg)


def fresh_install_arm(how, node, version, strict):
    """Kill the controller mid post-install hook. strict: the arm must reproduce pending-install."""
    log(f"=== {how}: fresh install, controller killed mid post-install hook")
    pod = fresh_controller()
    a = Arm(how, version).create()
    a.wait_hook_running("install")
    job1, app1, sec1 = uid(a.job("install")), uid(a.app()), a.secrets()
    if a.job("install-2") is not None:
        raise RuntimeError(f"{how}: the second hook already exists at the kill, so nothing is left un-run")
    print(f"      at the kill: {sec1}; install Job {job1[:8]}, not complete: {not a.complete('install')}")
    if len(sec1) != 1 or sec1[0][1] != "pending-install" or a.complete("install"):
        raise RuntimeError(f"{how}: not mid-install at the kill: {sec1}")
    t_kill = time.time()
    kill(how, node, pod)
    print(f"      killed: {proof_of_death(how, pod)}")
    try:
        wait(f"{how}: to settle", lambda: a.ready()[1] == "HooksMayNotHaveRun" or (
            a.ready()[0] == "True" and (a.complete("install-2") or time.time() - t_kill > 200)), 420)
    except TimeoutError as e:
        print(f"      {e}; Ready={a.ready()}")
    first = [s for _, snap in a.timeline for s in snap if s[2] == sec1[0][2]]
    states = [s[1] for s in first]
    reproduced = "failed" not in states and "deployed" not in states
    a.show_timeline()
    ev, fin, job2 = a.events("InterruptedInstallReinstalled"), a.secrets(), uid(a.job("install"))
    print(f"      first attempt's states: {states}; reinstall events {ev}; Ready={a.ready()}; now {fin}")
    fails = []
    # Safety, whatever state the kill left: no teardown hook, and never Ready with hooks that did not run.
    check(fails, a.predelete() is None, "no pre-delete hook ran")
    check(fails, not (a.ready()[0] == "True" and not (a.complete("install") and a.complete("install-2"))),
          f"not Ready=True with an install hook that never completed (hook 1 complete: {a.complete('install')}, "
          f"hook 2 exists: {a.job('install-2') is not None}, complete: {a.complete('install-2')})")
    if reproduced:
        check(fails, ev == 1, f"exactly one InterruptedInstallReinstalled event (got {ev})")
        check(fails, job2 is not None and job2 != job1, "the install hook Job is a new object")
        check(fails, a.complete("install") and a.complete("install-2"), "both install hooks ran to completion")
        check(fails, uid(a.app()) not in (None, app1), "the app object was reinstalled")
        check(fails, len(fin) == 1 and fin[0][0] == "1" and fin[0][1] == "deployed" and fin[0][2] != sec1[0][2], "one revision, deployed, a new release")
        check(fails, a.ready()[0] == "True", "Ready=True")
        check(fails, a.ann(UNVERIFIED) is None, "no hooks-unverified mark")
        check(fails, a.ann(COUNTER) is None, "the attempt counter is cleared")
    a.stop.set()
    verdict = "FAIL" if fails else ("PASS" if reproduced else "NOT REPRODUCED")
    if strict and not reproduced:
        verdict = "FAIL"
        fails.append("the kill did not leave pending-install, so #629 was not exercised")
    results.append((how, verdict, "; ".join(fails) or f"first attempt's states {states}", strict))
    return a


def kill_switch_arm(node, version):
    log("=== kill-switch: composition annotated manual, SIGKILL mid post-install hook")
    pod = fresh_controller()
    a = Arm("kill-switch", version, {SWITCH: "manual"}).create()
    a.wait_hook_running("install")
    job1, app1 = uid(a.job("install")), uid(a.app())
    kill("sigkill", node, pod)
    print(f"      killed: {proof_of_death('sigkill', pod)}")
    try:
        wait("the human gate", lambda: a.ready()[1] == "HooksMayNotHaveRun", 300)
    except TimeoutError as e:
        print(f"      {e}")
    a.show_timeline()
    fails = []
    check(fails, a.ready() == ("False", "HooksMayNotHaveRun"), f"Ready=False/HooksMayNotHaveRun (got {a.ready()})")
    check(fails, a.ann(UNVERIFIED) is not None, "hooks-unverified is set")
    check(fails, a.events("InterruptedInstallReinstalled") == 0, "no reinstall event")
    check(fails, uid(a.app()) == app1, "the app object is the same object: not uninstalled")
    check(fails, uid(a.job("install")) == job1, "the hook Job is the same object")
    check(fails, a.predelete() is None, "no pre-delete hook ran")
    a.stop.set()
    results.append(("kill-switch", "FAIL" if fails else "PASS", "; ".join(fails), True))


def once_deployed_arm(node, version):
    log("=== once-deployed CONTROL: deployed release, SIGKILL mid post-upgrade hook")
    a = Arm("once-deployed", version, hook_seconds=1).create()
    wait("the first install", lambda: a.ready()[0] == "True" and a.complete("install-2"), 240)
    app1, sec1 = uid(a.app()), a.secrets()
    pod = fresh_controller()
    sh("kubectl", "patch", "hookprobes.composition.krateo.io", "once-deployed", "-n", NS, "--type", "merge",
       "-p", json.dumps({"spec": {"rev": 2, "hookSeconds": HOOK_SECONDS}}))
    a.wait_hook_running("upgrade")
    job1, at = uid(a.job("upgrade")), a.secrets()
    print(f"      before {sec1}; at the kill {at}")
    if not any(s[1] == "pending-upgrade" for s in at) or not any(s[1] == "deployed" for s in at):
        raise RuntimeError(f"once-deployed: not mid-upgrade at the kill: {at}")
    kill("sigkill", node, pod)
    print(f"      killed: {proof_of_death('sigkill', pod)}")
    try:
        wait("recovery", lambda: uid(a.job("upgrade")) not in (None, job1) and a.complete("upgrade") and a.ready()[0] == "True", 360)
    except TimeoutError as e:
        print(f"      {e}; Ready={a.ready()}")
    a.show_timeline()
    fails = []
    check(fails, uid(a.app()) == app1, "the app object is the SAME object: the release was never uninstalled")
    # Not "revision 1 is always there": helm prunes it at max history (HELM_MAX_HISTORY, default 3) once
    # the recovery's rollback and upgrade exist. What an uninstall would do is empty the history.
    seen = [{s[1] for s in snap} for _, snap in a.timeline]
    live = seen[next(i for i, st in enumerate(seen) if "deployed" in st):]
    check(fails, all(st & {"deployed", "superseded"} for st in live) and not any("uninstalling" in st for st in seen),
          f"a once-deployed revision was present in all {len(live)} readings since the first deploy, none uninstalling")
    check(fails, a.predelete() is None, "no pre-delete hook ran")
    check(fails, a.events("InterruptedInstallReinstalled") == 0, "no reinstall event")
    check(fails, (a.app()["data"] or {}).get("rev") == "2", "the upgrade was applied")
    check(fails, uid(a.job("upgrade")) != job1 and a.complete("upgrade"), "the upgrade hook ran again, to completion")
    check(fails, a.ready()[0] == "True", f"Ready=True (got {a.ready()})")
    a.stop.set()
    results.append(("once-deployed", "FAIL" if fails else "PASS", "; ".join(fails), True))
    return a


def witness_arm(a):
    log("=== witness CONTROL: deleting a composition runs its pre-delete hook")
    sh("kubectl", "delete", "hookprobes.composition.krateo.io", a.name, "-n", NS, "--wait=false")
    try:
        wait("the composition to go", lambda: a.comp() is None, 240)
    except TimeoutError as e:
        print(f"      {e}")
    fails = []
    check(fails, a.predelete() is not None, "the pre-delete witness appeared")
    results.append(("witness", "FAIL" if fails else "PASS", "; ".join(fails), True))


def setup(version, chart_dir):
    import base64, os, tempfile
    for c, rel, extra in (("core-provider-crds", "core-provider-crds", []),
                          ("core-provider", "core-provider", [
                              # A CI runner has 4 CPUs; the chart asks 1 CPU per chart-inspector replica.
                              "--set", "chartInspector.replicaCount=1", "--set", "chartInspector.resources.requests.cpu=100m",
                              "--set", "resources.requests.cpu=100m",
                              "--set-string", "cdc.env.COMPOSITION_CONTROLLER_DEBUG=true"])):
        log(f"installing {c} {version}")
        sh("helm", "upgrade", "--install", rel, f"oci://ghcr.io/krateo-platformops/charts/{c}", "--version", version,
           "-n", CP_NS, "--create-namespace", "--wait", "--timeout", "8m", *extra)
    d = tempfile.mkdtemp()
    sh("helm", "package", chart_dir, "-d", d)
    tgz = base64.b64encode(open(os.path.join(d, "hookprobe-0.1.0.tgz"), "rb").read()).decode()
    labels = {"app": "charts"}
    objs = [
        {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": NS}},
        {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "charts", "namespace": NS}, "binaryData": {"hookprobe-0.1.0.tgz": tgz}},
        {"apiVersion": "v1", "kind": "Service", "metadata": {"name": "charts", "namespace": NS}, "spec": {"selector": labels, "ports": [{"port": 8080}]}},
        {"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "charts", "namespace": NS},
         "spec": {"selector": {"matchLabels": labels}, "template": {"metadata": {"labels": labels}, "spec": {
             "containers": [{"name": "httpd", "image": "registry.k8s.io/e2e-test-images/busybox:1.36.1-1",
                             "command": ["httpd", "-f", "-p", "8080", "-h", "/srv"],
                             "volumeMounts": [{"name": "c", "mountPath": "/srv"}]}],
             "volumes": [{"name": "c", "configMap": {"name": "charts"}}]}}}}]
    sh("kubectl", "apply", "-f", "-", inp=json.dumps({"apiVersion": "v1", "kind": "List", "items": objs}))
    sh("kubectl", "rollout", "status", "-n", NS, "deployment/charts", "--timeout=180s")
    sh("kubectl", "apply", "-f", "-", inp=json.dumps({
        "apiVersion": "core.krateo.io/v1alpha1", "kind": "CompositionDefinition", "metadata": {"name": "hookprobe", "namespace": NS},
        "spec": {"chart": {"url": f"http://charts.{NS}.svc.cluster.local:8080/hookprobe-0.1.0.tgz", "version": "0.1.0"}}}))

    def cd_ready():
        cd = kget("compositiondefinitions.core.krateo.io", "hookprobe")
        st = cd.get("status") or {}
        return st if any(c["type"] == "Ready" and c["status"] == "True" for c in st.get("conditions") or []) else None
    st = wait("the CompositionDefinition to be Ready", cd_ready, 300)
    log(f"CompositionDefinition Ready: {st['apiVersion']} {st['kind']}")
    imgs = {c["image"] for p in kget("pods", ns=CP_NS) for c in p["spec"]["containers"]}
    log("images under test: " + ", ".join(sorted(imgs)))
    if not all(i.endswith(":" + version) for i in imgs):
        raise RuntimeError(f"not only {version} is running: {imgs}")
    return st["apiVersion"].split("/")[1]


def diagnostics():
    print("::group::diagnostics")
    for a in (["kubectl", "get", "pods,jobs,configmaps,leases", "-A"],
              ["kubectl", "get", "hookprobes.composition.krateo.io,compositiondefinitions.core.krateo.io", "-n", NS, "-o", "yaml"],
              ["kubectl", "get", "events", "-n", NS, "--sort-by=.lastTimestamp"],
              ["kubectl", "logs", "-n", CP_NS, "deployment/core-provider", "--tail=150"]):
        print("$", " ".join(a)); print(sh(*a, check=False)[-20000:])
    for p in kget("pods", ns=CP_NS):
        if p["metadata"]["name"].startswith("hookprobes-"):
            for prev in ([], ["--previous"]):
                print("--- controller", p["metadata"]["name"], prev)
                print(sh("kubectl", "logs", "-n", CP_NS, p["metadata"]["name"], "--tail=400", *prev, check=False)[-30000:])
    print("::endgroup::", flush=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--version", required=True)
    ap.add_argument("--node", required=True, help="the kind node's container name")
    ap.add_argument("--chart", required=True)
    o = ap.parse_args()
    required = ["sigkill", "kill-switch", "once-deployed", "witness"]
    try:
        v = setup(o.version, o.chart)
        fresh_install_arm("sigkill", o.node, v, strict=True)
        kill_switch_arm(o.node, v)
        witness_arm(once_deployed_arm(o.node, v))
        for how in ("force-delete", "lease-stolen", "graceful-delete"):
            try:
                fresh_install_arm(how, o.node, v, strict=False)
            except Exception as e:
                results.append((how, "ERROR", repr(e), False))
    except Exception as e:
        results.append(("run", "ERROR", repr(e), True))
    log(f"=== results, core-provider {o.version}")
    for arm, verdict, detail, dec in results:
        print(f"  {verdict:15} {arm:16} {'deciding' if dec else 'measured'}  {detail}")
    ran = {r[0] for r in results}
    missing = [r for r in required if r not in ran]  # the denominator: an arm that never ran is a failure
    if missing:
        print(f"  deciding arms that did not run: {missing}")
    bad = missing or [r for r in results if r[3] and r[1] != "PASS"]
    odd = [r for r in results if not r[3] and r[1] in ("FAIL", "ERROR")]
    if bad or odd:
        diagnostics()
    for r in odd:
        print(f"::warning::measured arm {r[0]}: {r[1]}: {r[2]}")
    print("#629 exercise", "FAILED" if bad else "passed", "for core-provider", o.version)
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
