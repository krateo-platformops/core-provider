#!/usr/bin/env python3
"""Exercise core-provider#629/#636 (a first install that is interrupted or fails is retried as an install)
on a real cluster.

Installs the published core-provider charts, optionally with a commit's own images, and kills the real
composition controller pod while a chart's lifecycle hook Job is running. Needs a disposable cluster on
kind: the SIGKILL arms reach the node through `docker exec`. Standard library only.

  KUBECONFIG=... run.py --version 2.13.128 --node <kind node container> --chart <dir> [--image-tag ci-abc]

Every arm decides the result; any failure exits 1.
  sigkill, force-delete, lease-stolen   first install, controller killed mid post-install hook, which must
                                        leave pending-install: settled as failed and installed again, the
                                        objects kept, every install hook run, nothing uninstalled.
  graceful-delete                       the same kill with a drain, which must leave failed: installed again.
  failing-hook                          nothing killed, hook 1 exits 1: never Ready=True, InstallFailing from
                                        the 3rd failure, and it recovers once the hook passes.
  kill-switch                           composition annotated manual: kept, HooksMayNotHaveRun.
  once-deployed                         CONTROL: a deployed release whose upgrade is interrupted keeps its
                                        objects and is never installed again.
  witness                               CONTROL: deleting a composition DOES run the pre-delete hook, so its
                                        absence elsewhere means something.

A version before #636 fails the first two rows: that is the control for the exercise itself. Run it with
--control, which exits 0 only when exactly those arms FAIL and the others PASS. So a control that behaves
is green and one that does not is red, like any other run; without it, a red control run on a commit reads
as that commit's CI failing (installer's pin gate did, on 2.13.129).
"""
import argparse, datetime, json, subprocess, sys, threading, time

NS, CP_NS = "exercise", "krateo-system"
SWITCH = "krateo.io/interrupted-install-recovery"
COUNTER = "krateo.io/interrupted-install-reinstalls"
UNVERIFIED = "krateo.io/hooks-unverified"
NEVER = "krateo.io/release-never-deployed"
RETRIED = {"Interrupted": "InterruptedInstallRetried", "Failed": "FailedInstallRetried"}
HOOK_SECONDS = 45
results = []  # (arm, verdict, detail, deciding)
REQUIRED = ["sigkill", "kill-switch", "once-deployed", "witness", "force-delete", "lease-stolen", "graceful-delete", "failing-hook"]
FAIL_BEFORE_636 = {"sigkill", "force-delete", "lease-stolen", "graceful-delete", "failing-hook"}


def problems(results, control):
    """What makes this run red. An arm that never ran, or that ended in ERROR, is a problem in both modes."""
    out = [f"{a}: did not run" for a in REQUIRED if a not in {r[0] for r in results}]
    for arm, verdict, detail, deciding in results:
        want = "FAIL" if control and arm in FAIL_BEFORE_636 else "PASS"
        if deciding and verdict != want:
            out.append(f"{arm}: {verdict}, want {want}")
    return out


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

    def __init__(self, name, gvr_version, annotations=None, hook_seconds=HOOK_SECONDS, install_exit=0):
        self.name, self.rel, self.timeline, self.stop = name, f"rel-{name}", [], threading.Event()
        self.errors = 0
        self.manifest = {
            "apiVersion": f"composition.krateo.io/{gvr_version}", "kind": "Hookprobe",
            "metadata": {"name": name, "namespace": NS, "annotations": annotations or {},
                         "labels": {"krateo.io/release-name": self.rel,
                                    "krateo.io/composition-version": gvr_version}},
            "spec": {"rev": 1, "hookSeconds": hook_seconds, "installHookExit": install_exit}}

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

    def status(self, k):
        return ((self.comp() or {}).get("status") or {}).get(k)

    def message(self):
        return next((c["message"] for c in self.status("conditions") or [] if c["type"] == "Ready"), "")

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


def watch_ready(a, done, timeout):
    """Ready's values in order, and whether it was ever True before both install hooks were complete. The
    hooks are read AFTER Ready, so a True with a hook incomplete is a real violation, never a race."""
    seen, early, end = [], False, time.time() + timeout
    while time.time() < end:
        r = a.ready()
        both = a.complete("install") and a.complete("install-2")
        early |= r[0] == "True" and not both
        if not seen or seen[-1] != r:
            seen.append(r)
        if done(r, both):
            break
        time.sleep(0.7)
    return seen, early


def retried_checks(fails, a, cause, sec1, job1, app1):
    """What an automatic retry must leave, whatever ended the first attempt. cause: Interrupted or Failed."""
    other = "Failed" if cause == "Interrupted" else "Interrupted"
    fin, statuses = a.secrets(), {s[1] for _, snap in a.timeline for s in snap}
    rec = a.status("installRecovery") or {}
    print(f"      now {fin}; Ready={a.ready()}; installRecovery={rec}; statuses seen {sorted(statuses)}")
    check(fails, not statuses & {"pending-upgrade", "uninstalling", "pending-rollback"}, "never upgraded, rolled back or uninstalled")
    check(fails, [(s[0], s[1]) for s in fin] == [("1", "failed"), ("2", "deployed")] and fin[0][2] == sec1[0][2],
          "revision 1 is the same record, failed, and revision 2 is deployed")
    check(fails, uid(a.app()) == app1, "the app object is the SAME object: nothing was uninstalled")
    check(fails, uid(a.job("install")) not in (None, job1), "the install hook Job is a new object")
    check(fails, a.complete("install") and a.complete("install-2"), "both install hooks ran to completion")
    check(fails, a.job("upgrade") is None, "no upgrade hook ran")
    check(fails, a.predelete() is None, "no pre-delete hook ran")
    check(fails, a.ready()[0] == "True", "Ready=True")
    n = a.events(RETRIED[cause])
    check(fails, n == 1, f"exactly one {RETRIED[cause]} event (got {n})")
    check(fails, a.events(RETRIED[other]) == 0, f"no {RETRIED[other]} event")
    check(fails, rec.get("kind") == "Retried" and rec.get("count") == 1 and rec.get("reason") == cause and rec.get("message"),
          f"status.installRecovery says Retried, 1, {cause} (got {rec})")
    check(fails, "revision" not in rec and not a.status("installFailures"), "the per-attempt records are gone")
    check(fails, all(a.ann(k) is None for k in (UNVERIFIED, COUNTER, NEVER)), "no mark is left on the composition")


def fresh_install_arm(how, node, version, cause):
    """Kill the controller mid post-install hook. cause: what the kill must have left, or the arm is void."""
    log(f"=== {how}: first install, controller killed mid post-install hook")
    pod = fresh_controller()
    a = Arm(how, version).create()
    a.wait_hook_running("install")
    job1, app1, sec1 = uid(a.job("install")), uid(a.app()), a.secrets()
    if a.job("install-2") is not None:
        raise RuntimeError(f"{how}: the second hook already exists at the kill, so nothing is left un-run")
    print(f"      at the kill: {sec1}; install Job {job1[:8]}, not complete: {not a.complete('install')}")
    if len(sec1) != 1 or sec1[0][1] != "pending-install" or a.complete("install"):
        raise RuntimeError(f"{how}: not mid-install at the kill: {sec1}")
    kill(how, node, pod)
    print(f"      killed: {proof_of_death(how, pod)}")
    seen, early = watch_ready(a, lambda r, both: r[0] == "True" and both, 420)
    a.show_timeline()
    print(f"      Ready over time: {seen}")
    fails = []
    check(fails, not early, "never Ready=True before both install hooks completed")
    retried_checks(fails, a, cause, sec1, job1, app1)
    a.stop.set()
    results.append((how, "FAIL" if fails else "PASS", "; ".join(fails), True))


def failing_hook_arm(version):
    log("=== failing-hook: first install whose first post-install hook exits 1; nothing is killed")
    a = Arm("failing-hook", version, hook_seconds=2, install_exit=1).create()
    wait("the app object", a.app, 120)
    app1 = uid(a.app())
    seen, early = watch_ready(a, lambda r, both: r[1] == "InstallFailing", 420)
    msg, failures = a.message(), a.status("installFailures") or {}
    # Stay on it: the reason must not flap while retries run.
    more, early2 = watch_ready(a, lambda r, both: False, 75)
    a.show_timeline()
    print(f"      Ready over time: {seen} then {more}\n      at the first InstallFailing: {failures}; {msg[:300]}")
    fails = []
    allowed = {("False", "HelmOperationInProgress"), ("False", "Creating"), ("False", "InstallFailing"), (None, None)}
    check(fails, seen[-1] == ("False", "InstallFailing"), "Ready reached False/InstallFailing")
    check(fails, set(seen) | set(more) <= allowed, "Ready never said anything else")
    check(fails, set(more) == {("False", "InstallFailing")}, "no flap away from InstallFailing while it retries")
    check(fails, not early and not early2, "never Ready=True")
    check(fails, failures.get("count") == 3, f"InstallFailing came with the 3rd failure (got {failures.get('count')})")
    check(fails, f"the first install has failed {failures.get('count')} times (" in msg and "automatic retries so far); last error:" in msg,
          "the message names the failures, the retries and the error")
    check(fails, a.job("install-2") is None, "hook 2 was never reached")
    check(fails, a.job("upgrade") is None, "no upgrade hook ran")
    check(fails, max(len(s) for _, s in a.timeline) <= 3, "at most 3 revisions are kept")
    check(fails, not {s[1] for _, snap in a.timeline for s in snap} & {"pending-upgrade", "uninstalling"}, "never upgraded or uninstalled")
    n = a.status("installRecovery") or {}
    check(fails, n.get("kind") == "Retried" and n.get("count", 0) >= 2 and n.get("reason") == "Failed", f"installRecovery {n}")
    log("    the hook is fixed")
    sh("kubectl", "patch", "hookprobes.composition.krateo.io", "failing-hook", "-n", NS, "--type", "merge",
       "-p", json.dumps({"spec": {"installHookExit": 0}}))
    seen, early = watch_ready(a, lambda r, both: r[0] == "True" and both, 300)
    print(f"      Ready over time: {seen}; now {a.secrets()}")
    check(fails, not early, "never Ready=True before both install hooks completed")
    check(fails, seen[-1][0] == "True" and a.complete("install") and a.complete("install-2"), "both hooks ran and Ready=True")
    check(fails, uid(a.app()) == app1, "the app object is the SAME object throughout")
    check(fails, a.job("upgrade") is None, "still no upgrade hook")
    check(fails, not a.status("installFailures") and (a.status("installRecovery") or {}).get("count", 0) >= 3, "the failures are forgotten, the retries are not")
    check(fails, all(a.ann(k) is None for k in (UNVERIFIED, NEVER)), "no mark is left on the composition")
    check(fails, a.predelete() is None, "no pre-delete hook ran")
    a.stop.set()
    results.append(("failing-hook", "FAIL" if fails else "PASS", "; ".join(fails), True))


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
    check(fails, not any(a.events(r) for r in RETRIED.values()), "no retry event")
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
        wait("recovery", lambda: uid(a.job("upgrade")) not in (None, job1) and a.complete("upgrade") and a.ready()[0] == "True"
             and a.secrets()[-1][1] == "deployed", 360)
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
    check(fails, not any(a.events(r) for r in RETRIED.values()), "no retry event")
    check(fails, (a.app()["data"] or {}).get("rev") == "2", "the upgrade was applied")
    check(fails, uid(a.job("upgrade")) != job1 and a.complete("upgrade"), "the upgrade hook ran again, to completion")
    check(fails, a.ready()[0] == "True", f"Ready=True (got {a.ready()})")
    check(fails, a.secrets()[-1][1] == "deployed", f"the latest revision is deployed (got {a.secrets()[-1]})")
    check(fails, a.status("installRecovery") is None, "no installRecovery: it is for first installs")
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


def install_core_provider(version, image_tag=None):
    """image_tag: core-provider and the composition controller run this tag (loaded on the node) instead."""
    for c, rel, extra in (("core-provider-crds", "core-provider-crds", []),
                          ("core-provider", "core-provider", [
                              # A CI runner has 4 CPUs; the chart asks 1 CPU per chart-inspector replica.
                              "--set", "chartInspector.replicaCount=1", "--set", "chartInspector.resources.requests.cpu=100m",
                              "--set", "resources.requests.cpu=100m",
                              "--set-string", "cdc.env.COMPOSITION_CONTROLLER_DEBUG=true"]
                           + (["--set", f"image.tag={image_tag},cdc.image.tag={image_tag}"] if image_tag else []))):
        log(f"installing {c} {version}")
        sh("helm", "upgrade", "--install", rel, f"oci://ghcr.io/krateo-platformops/charts/{c}", "--version", version,
           "-n", CP_NS, "--create-namespace", "--wait", "--timeout", "8m", *extra)


def serve_charts(*chart_dirs):
    """Package the charts (all version 0.1.0) and serve them in the cluster. Returns name -> URL."""
    import base64, os, tempfile
    d, data = tempfile.mkdtemp(), {}
    for c in chart_dirs:
        sh("helm", "package", c, "-d", d)
    for t in sorted(os.listdir(d)):
        data[t] = base64.b64encode(open(os.path.join(d, t), "rb").read()).decode()
    labels = {"app": "charts"}
    objs = [
        {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": NS}},
        {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "charts", "namespace": NS}, "binaryData": data},
        {"apiVersion": "v1", "kind": "Service", "metadata": {"name": "charts", "namespace": NS}, "spec": {"selector": labels, "ports": [{"port": 8080}]}},
        {"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "charts", "namespace": NS},
         "spec": {"selector": {"matchLabels": labels}, "template": {"metadata": {"labels": labels}, "spec": {
             "containers": [{"name": "httpd", "image": "registry.k8s.io/e2e-test-images/busybox:1.36.1-1",
                             "command": ["httpd", "-f", "-p", "8080", "-h", "/srv"],
                             "volumeMounts": [{"name": "c", "mountPath": "/srv"}]}],
             "volumes": [{"name": "c", "configMap": {"name": "charts"}}]}}}}]
    sh("kubectl", "apply", "-f", "-", inp=json.dumps({"apiVersion": "v1", "kind": "List", "items": objs}))
    sh("kubectl", "rollout", "status", "-n", NS, "deployment/charts", "--timeout=180s")
    return {t[:-len("-0.1.0.tgz")]: f"http://charts.{NS}.svc.cluster.local:8080/{t}" for t in data}


def define(name, url):
    """Create a CompositionDefinition, wait for it, and return the served CRD version."""
    sh("kubectl", "apply", "-f", "-", inp=json.dumps({
        "apiVersion": "core.krateo.io/v1alpha1", "kind": "CompositionDefinition", "metadata": {"name": name, "namespace": NS},
        "spec": {"chart": {"url": url, "version": "0.1.0"}}}))

    def cd_ready():
        cd = kget("compositiondefinitions.core.krateo.io", name)
        st = cd.get("status") or {}
        return st if any(c["type"] == "Ready" and c["status"] == "True" for c in st.get("conditions") or []) else None
    st = wait("the CompositionDefinition to be Ready", cd_ready, 300)
    log(f"CompositionDefinition Ready: {st['apiVersion']} {st['kind']}")
    return st["apiVersion"].split("/")[1]


def check_images(version, image_tag=None):
    imgs = {c["image"] for p in kget("pods", ns=CP_NS) for c in p["spec"]["containers"]}
    log("images under test: " + ", ".join(sorted(imgs)))
    want = {f"ghcr.io/krateo-platformops/{n}:{t}" for n, t in (("core-provider", image_tag or version),
            ("composition-dynamic-controller", image_tag or version), ("chart-inspector", version))}
    if imgs != want:
        raise RuntimeError(f"running {imgs}, want {want}")


def setup(version, chart_dir, image_tag):
    install_core_provider(version, image_tag)
    v = define("hookprobe", serve_charts(chart_dir)["hookprobe"])
    check_images(version, image_tag)
    return v


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
    ap.add_argument("--image-tag", default=None)
    ap.add_argument("--control", action="store_true", help="a version before #636: exactly its known arms must FAIL")
    o = ap.parse_args()

    def arm(name, fn):
        try:
            return fn()
        except Exception as e:
            results.append((name, "ERROR", repr(e), True))
    try:
        v = setup(o.version, o.chart, o.image_tag)
    except Exception as e:
        results.append(("setup", "ERROR", repr(e), True))
    else:
        arm("sigkill", lambda: fresh_install_arm("sigkill", o.node, v, "Interrupted"))
        arm("kill-switch", lambda: kill_switch_arm(o.node, v))
        once = arm("once-deployed", lambda: once_deployed_arm(o.node, v))
        if once:
            arm("witness", lambda: witness_arm(once))
        arm("force-delete", lambda: fresh_install_arm("force-delete", o.node, v, "Interrupted"))
        arm("lease-stolen", lambda: fresh_install_arm("lease-stolen", o.node, v, "Interrupted"))
        arm("graceful-delete", lambda: fresh_install_arm("graceful-delete", o.node, v, "Failed"))
        arm("failing-hook", lambda: failing_hook_arm(v))
    log(f"=== results, core-provider {o.image_tag or o.version}")
    for arm, verdict, detail, dec in results:
        print(f"  {verdict:15} {arm:16} {'deciding' if dec else ''}  {detail}")
    bad = problems(results, o.control)
    for p in bad:
        print("  " + p)
    if bad:
        diagnostics()
    print("first-install exercise" + (" CONTROL" if o.control else ""), "FAILED" if bad else "passed",
          "for core-provider", o.image_tag or o.version)
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
