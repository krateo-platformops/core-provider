#!/usr/bin/env python3
"""Exercise core-provider#651 on a real cluster: while a roll of core-provider is stuck, the old process
keeps rendering its OWN controller templates, so it rolls no composition controller.

Needs a disposable kind cluster made with kind.yaml (one arm kills a container through `docker exec`).
Standard library only; helm and kubectl on the PATH.

  run.py --node <kind node> --probe <hookprobe chart dir> --from 2.13.129 \\
         (--target-chart DIR --crds-chart DIR --image-tag ci-abc --chart-inspector-tag 2.13.129 |
          --target-version 2.13.130)
  run.py ... --suite from-old --from 2.13.57
  run.py ... --target-version 2.13.129 --control

The target chart is always a copy with one key added to the controller's env ConfigMap template
(EXERCISE_REV), so "the new templates were rendered" is readable on the cluster. Every arm decides.

suite main, FROM a released chart:
  quota-full         the upgrade's new pod is refused by a full pod quota: nothing is rolled, and the new
                     template ConfigMaps exist while the old ones are gone.
  container-killed   the old container is killed in that state: it restarts, on its own files.
  quota-lifted       everything converges on the new templates.
  image-pull         the next upgrade's new pod cannot pull its image: nothing is rolled.
  old-pod-deleted    the old pod is deleted in that state: its replacement cannot mount (FailedMount on
                     the deleted ConfigMap), and nothing is rolled.
  image-fixed        everything converges.
  rollback-back      helm rollback to the released chart, quota full: nothing is rolled; then converges.
  rollback-forward   helm rollback to the newer revision, quota full: nothing is rolled; then converges.
  values-only        a values change on the same chart reaches the controller promptly.
  unknown-key        a template key the binary does not know is refused: nothing is applied, the
                     definition says which key, and it recovers when the key goes.
suite from-old, FROM a much older released chart (the acmp incident's, 2.13.57):
  from-old           quota-full, from that version.
  from-old-lifted    everything converges.

A target before the fix fails the arms in FAIL_BEFORE_651, each for the reason given there: that is the
control for the exercise itself. With --control the run is green only when exactly those arms FAIL, for
those reasons, and the others PASS. An arm that did not run, or ended in ERROR, is red in both modes.
"""
import argparse, datetime, json, os, re, shutil, subprocess, sys, tempfile, time

NS, CP_NS, REL = "exercise", "krateo-system", "core-provider"
STEM = "hookprobes-v0-1-0"  # the probe definition's controller objects: <plural>-<version>
REPO = "oci://ghcr.io/krateo-platformops/charts"
POLL = 30  # core-provider's drift poll here, seconds (CORE_PROVIDER_POLL_INTERVAL)
HOLD, SHORT_HOLD = 120, 75  # both > kubelet sync (10s, jittered to 15s) + POLL; the control shows it is enough
VOLUMES = ("cdc-deployment", "cdc-configmap", "cdc-rbac", "jsonschema-configmap", "cdc-service")
results = []  # (arm, verdict, detail)
REQUIRED = {
    "main": ["quota-full", "container-killed", "quota-lifted", "image-pull", "old-pod-deleted", "image-fixed",
             "rollback-back", "rollback-forward", "values-only", "unknown-key"],
    "from-old": ["from-old", "from-old-lifted"],
}
FAIL_BEFORE_651 = {  # arm -> what its failure must say
    "quota-full": "ROLLED", "from-old": "ROLLED", "image-pull": "ROLLED",
    "rollback-back": "ROLLED", "rollback-forward": "ROLLED",
    "container-killed": "FILES CHANGED", "old-pod-deleted": "OLD POD STARTED", "unknown-key": "APPLIED",
}


def problems(suite, control):
    """What makes this run red."""
    ran = [r[0] for r in results]
    out = [f"{a}: did not run" for a in REQUIRED[suite] if a not in ran]
    out += [f"{a}: ran {ran.count(a)} times" for a in sorted(set(ran)) if ran.count(a) != 1]
    for arm, verdict, detail in results:
        want = "FAIL" if control and arm in FAIL_BEFORE_651 else "PASS"
        if verdict != want:
            out.append(f"{arm}: {verdict}, want {want}")
        elif want == "FAIL" and FAIL_BEFORE_651[arm] not in detail:
            out.append(f"{arm}: FAIL, but not for the defect ({FAIL_BEFORE_651[arm]}): {detail}")
    return out


def log(m):
    print(f"[{datetime.datetime.now(datetime.timezone.utc):%H:%M:%S}] {m}", flush=True)


def sh(*a, check=True, inp=None):
    p = subprocess.run(a, input=inp, capture_output=True, text=True)
    if check and p.returncode:
        raise RuntimeError(f"{' '.join(a)}: exit {p.returncode}: {p.stderr.strip()[:600]}")
    return p.stdout


def kget(kind, name=None, ns=CP_NS, selector=None):
    """The object (None only on NotFound), or the list's items. Any other error raises. ns None: all."""
    a = ["kubectl", "get", kind] + ([name] if name else []) + (["-n", ns] if ns else ["-A"]) + ["-o", "json"]
    if selector:
        a += ["-l", selector]
    p = subprocess.run(a, capture_output=True, text=True)
    if p.returncode:
        if name and "(NotFound)" in p.stderr:
            return None
        raise RuntimeError(f"get {kind} {name}: {p.stderr.strip()[:300]}")
    o = json.loads(p.stdout)
    return o if name else o["items"]


def wait(what, fn, timeout=240, every=2.0):
    end = time.time() + timeout
    while time.time() < end:
        last = fn()
        if last:
            return last
        time.sleep(every)
    raise TimeoutError(f"timed out after {timeout}s waiting for {what}")


def uid(o):
    return o["metadata"]["uid"]


# ---------------------------------------------------------------- charts

workdir = tempfile.mkdtemp(prefix="upgrade-templates-")


def pulled(chart, version):
    """A released chart, unpacked."""
    d = os.path.join(workdir, f"{chart}-{version}")
    if not os.path.isdir(d):
        os.makedirs(d)
        sh("helm", "pull", f"{REPO}/{chart}", "--version", version, "--untar", "--untardir", d)
    return os.path.join(d, chart)


def edited(src, rev, unknown=False):
    """A copy of a core-provider chart whose controller env ConfigMap template carries one more key."""
    d = os.path.join(tempfile.mkdtemp(dir=workdir), "core-provider")
    shutil.copytree(src, d)
    substitute(d)
    p = os.path.join(d, "assets/cdc/configmap.yaml")
    s = open(p).read()
    if s.count("\ndata:\n") != 1:
        raise RuntimeError(f"{p}: no single data: line to add a key after")
    add = f'  EXERCISE_REV: "{rev}"\n'
    if unknown:
        add += '  EXERCISE_UNKNOWN: {{ "{{" }} .exerciseUnknown {{ "}}" }}\n'
    open(p, "w").write(s.replace("\ndata:\n", "\ndata:\n" + add))
    return d


def substitute(d):
    """The release workflow's placeholders, in a chart taken from the repository."""
    p = os.path.join(d, "Chart.yaml")
    s = open(p).read()
    open(p, "w").write(s.replace("CHART_VERSION", "0.0.0-exercise").replace("APP_VERSION", "0.0.0-exercise"))


COMMON = [  # a CI runner has 4 CPUs; the chart asks 1 CPU per chart-inspector replica
    "--set", "chartInspector.replicaCount=1", "--set", "chartInspector.resources.requests.cpu=100m",
    "--set", "resources.requests.cpu=100m", "--set-string", f"env.CORE_PROVIDER_POLL_INTERVAL={POLL}s",
    "--set-string", "env.CORE_PROVIDER_MAX_ERROR_RETRY_INTERVAL=10s"]


class Target:
    """What is upgraded TO: a chart from the repository with a commit's images, or a released version."""

    def __init__(self, o):
        self.src = o.target_chart or pulled("core-provider", o.target_version)
        self.crds = o.crds_chart or pulled("core-provider-crds", o.target_version)
        self.image = o.image_tag or o.target_version
        self.flags = []
        if o.image_tag:  # chart-inspector stays a released image, as in the interrupted-install exercise
            self.flags = ["--set", f"image.tag={o.image_tag},cdc.image.tag={o.image_tag},chartInspector.image.tag={o.chart_inspector_tag}"]

    def upgrade_crds(self):
        d = os.path.join(tempfile.mkdtemp(dir=workdir), "core-provider-crds")
        shutil.copytree(self.crds, d)
        substitute(d)
        sh("helm", "upgrade", "--install", "core-provider-crds", d, "-n", CP_NS, "--wait", "--timeout", "3m")
        time.sleep(2 * POLL)  # the running core-provider has seen it before anything is watched

    def upgrade(self, rev, *extra, unknown=False):
        """Never --wait: a stuck roll must be left stuck."""
        sh("helm", "upgrade", REL, edited(self.src, rev, unknown), "-n", CP_NS, *COMMON, *self.flags, *extra)


def revision():
    return json.loads(sh("helm", "history", REL, "-n", CP_NS, "-o", "json"))[-1]["revision"]


# ---------------------------------------------------------------- the cluster

def core_provider():
    return kget("deployments.apps", REL)


def pods(old_uids=()):
    """core-provider's pods (chart-inspector's carry another name label), without those in old_uids."""
    return [p for p in kget("pods", selector="app.kubernetes.io/name=core-provider,app.kubernetes.io/instance=" + REL)
            if uid(p) not in old_uids]


def status0(p):
    return (p["status"].get("containerStatuses") or [{}])[0]


def is_ready(p):
    return status0(p).get("ready") and not p["metadata"].get("deletionTimestamp")


def the_pod():
    ps = pods()
    if len(ps) != 1 or not is_ready(ps[0]):
        raise RuntimeError(f"want one ready core-provider pod, have {[(p['metadata']['name'], bool(is_ready(p))) for p in ps]}")
    return ps[0]


def template_names(spec):
    return {v["name"]: v["configMap"]["name"] for v in spec["volumes"] if v["name"] in VOLUMES and "configMap" in v}


def mounted_rev(pod):
    """EXERCISE_REV in the env ConfigMap template that is mounted in this pod now, or ''."""
    body = sh("kubectl", "exec", "-n", CP_NS, pod["metadata"]["name"], "--", "cat", "/tmp/assets/cdc-configmap/configmap.yaml")
    if "COMPOSITION_CONTROLLER_SA_NAME" not in body:
        raise RuntimeError("the mounted template is not the controller's env ConfigMap")
    m = re.search(r'EXERCISE_REV: "(\w+)"', body)
    return m.group(1) if m else ""


def snapshot():
    """Every controller object of the probe definition: what a render of it would move."""
    out = {}
    for kind in ("deployments.apps", "configmaps", "services", "serviceaccounts", "roles.rbac.authorization.k8s.io",
                 "rolebindings.rbac.authorization.k8s.io", "clusterroles.rbac.authorization.k8s.io",
                 "clusterrolebindings.rbac.authorization.k8s.io"):
        for o in kget(kind, ns=None):
            m = o["metadata"]
            if m["name"].startswith(STEM):
                # A Deployment's resourceVersion moves with its status; its generation with its spec only.
                v = m["generation"] if kind == "deployments.apps" else m["resourceVersion"]
                out[f"{kind} {m.get('namespace', '')}/{m['name']}"] = f"{m['uid'][:8]}@{v}"
    if not any(k.startswith("deployments") for k in out) or not any(k.startswith("configmaps") for k in out):
        raise RuntimeError(f"no controller objects to watch: {sorted(out)}")
    return out


def moved(before, after):
    return sorted(k for k in set(before) | set(after) if before.get(k) != after.get(k))


def cd_condition(t):
    cd = kget("compositiondefinitions.core.krateo.io", "hookprobe", NS)
    return next((c for c in (cd.get("status") or {}).get("conditions") or [] if c["type"] == t), {})


def hold(seconds, before, fails):
    """Nothing of the controller moves for this long, and the definition stays Ready."""
    start, polls, not_ready = time.time(), 0, None
    while time.time() - start < seconds:
        polls += 1
        diff = moved(before, snapshot())
        if diff:
            print(f"      ROLLED after {time.time() - start:.0f}s: {diff}")
            fails.append(f"ROLLED: {', '.join(diff)}")
            return
        r = cd_condition("Ready")
        if r.get("status") != "True" and not not_ready:
            not_ready = f"{r.get('status')}/{r.get('reason')} {r.get('message', '')[:120]}"
        time.sleep(5)
    print(f"      held {seconds}s: {len(before)} objects unchanged over {polls} polls")
    check(fails, not not_ready, f"the definition stayed Ready=True ({not_ready or 'it did'})")


def check(fails, ok, what):
    print(f"      {'ok  ' if ok else 'BAD '} {what}")
    if not ok:
        fails.append(what)


def record(arm, fails):
    results.append((arm, "FAIL" if fails else "PASS", "; ".join(fails)))
    log(f"{arm}: {'FAIL' if fails else 'PASS'}")


def settled():
    """Nothing in the namespace is still rolling or terminating: a pod that went away after the quota was
    sized would free the very slot that must be missing."""
    if any(p["metadata"].get("deletionTimestamp") for p in kget("pods")):
        return False
    for d in kget("deployments.apps"):
        st, n = d["status"], d["spec"]["replicas"]
        if st.get("observedGeneration") != d["metadata"]["generation"] or {st.get(k, 0) for k in ("replicas", "updatedReplicas", "availableReplicas")} != {n}:
            return False
    return True


def fill_quota():
    wait("the namespace to settle", settled, 240)
    n = len([p for p in kget("pods") if p["status"].get("phase") not in ("Succeeded", "Failed")])
    sh("kubectl", "apply", "-f", "-", inp=json.dumps({"apiVersion": "v1", "kind": "ResourceQuota",
       "metadata": {"name": "full", "namespace": CP_NS}, "spec": {"hard": {"pods": str(n)}}}))
    wait("the quota to be full", lambda: ((kget("resourcequotas", "full") or {}).get("status") or {}).get("used", {}).get("pods") == str(n), 60)
    log(f"pod quota full: {n}/{n}")


def lift_quota():
    sh("kubectl", "delete", "resourcequota", "full", "-n", CP_NS)
    # A ReplicaSet retries a refused pod with a growing backoff, up to minutes: an event retries it now.
    sh("kubectl", "annotate", "replicasets.apps", "--all", "-n", CP_NS, f"exercise/nudge={time.time():.0f}", "--overwrite")


def refused_by_quota():
    """core-provider's new ReplicaSet cannot create its pod."""
    for c in core_provider()["status"].get("conditions") or []:
        if c["type"] == "ReplicaFailure" and c["status"] == "True" and "exceeded quota" in c.get("message", ""):
            return c["message"]


def witness_replaced(fails, old):
    """The new template ConfigMaps exist and the old pod's are gone. Not vacuous: one must differ."""
    new = template_names(core_provider()["spec"]["template"]["spec"])
    was = template_names(old["spec"])
    have = {c["metadata"]["name"] for c in kget("configmaps")}
    print(f"      old pod's templates: {was}\n      new templates:       {new}")
    check(fails, new["cdc-configmap"] != was["cdc-configmap"], "the new env ConfigMap template is another ConfigMap")
    check(fails, set(new.values()) <= have, "every new template ConfigMap exists")
    gone = set(was.values()) - set(new.values())
    check(fails, gone and not gone & have, f"the replaced template ConfigMaps are gone ({sorted(gone & have) or 'all of them'})")


def converged(target_image, rev, timeout=420):
    """One ready core-provider pod, of the current ReplicaSet and image; the controller in the management
    namespace rendered from its templates and rolled out; the definition Ready."""
    def done():
        d = core_provider()
        ps = pods()
        if len(ps) != 1 or not is_ready(ps[0]) or d["status"].get("observedGeneration") != d["metadata"]["generation"] \
                or d["status"].get("updatedReplicas") != 1:
            return None
        if not ps[0]["spec"]["containers"][0]["image"].endswith(":" + target_image):
            return None
        cm, dep = kget("configmaps", STEM + "-configmap"), kget("deployments.apps", STEM + "-controller")
        if not cm or not dep or cm["data"].get("EXERCISE_REV", "") != rev:
            return None
        st, c = dep["status"], dep["spec"]["template"]["spec"]["containers"][0]
        if not c["image"].endswith(":" + target_image):
            return None
        if st.get("observedGeneration") != dep["metadata"]["generation"] or st.get("availableReplicas") != 1 or st.get("updatedReplicas") != 1:
            return None
        return cd_condition("Ready").get("status") == "True" and cd_condition("Synced").get("status") == "True"
    start = time.time()
    wait(f"core-provider {target_image} and templates rev {rev or '(none)'} everywhere", done, timeout, 3)
    print(f"      converged in {time.time() - start:.0f}s")


def converge_arm(arm, image, rev, timeout=420):
    log(f"=== {arm}")
    fails = []
    try:
        converged(image, rev, timeout)
        check(fails, mounted_rev(the_pod()) == rev, f"the new pod's templates are rev {rev or '(none)'}")
    except TimeoutError as e:
        fails.append(str(e))
    record(arm, fails)


# ---------------------------------------------------------------- arms

def stuck_on_quota(arm, start):
    """start() an upgrade or rollback whose new pod a full quota refuses, and hold. Returns the old pod."""
    log(f"=== {arm}")
    fails = []
    fill_quota()
    old = the_pod()
    was = mounted_rev(old)
    before = snapshot()
    start()
    why = wait("the new pod to be refused by the quota", refused_by_quota, 120)
    print(f"      refused: {why[:160]}")
    witness_replaced(fails, old)
    hold(HOLD, before, fails)
    now = [p for p in pods() if uid(p) == uid(old)]
    check(fails, len(now) == 1 and is_ready(now[0]) and status0(now[0])["restartCount"] == status0(old)["restartCount"],
          "the old pod kept running, without a restart")
    check(fails, len(pods()) == 1, "no other core-provider pod exists: the roll was stuck throughout")
    if now:
        r = mounted_rev(now[0])
        check(fails, r == was, f"its templates are its own (rev {was or '(none)'}){'' if r == was else f': FILES CHANGED to rev {r}'}")
    record(arm, fails)
    return old


def container_killed_arm(node, old, own_rev):
    log("=== container-killed")
    fails, before = [], snapshot()
    p = next(p for p in pods() if uid(p) == uid(old))
    n = status0(p)["restartCount"]
    cid = status0(p)["containerID"].split("//")[1]
    pid = sh("docker", "exec", node, "crictl", "inspect", "-o", "go-template", "--template", "{{.info.pid}}", cid).strip()
    if not pid.isdigit() or int(pid) < 2:
        raise RuntimeError(f"no core-provider pid: {pid!r}")
    sh("docker", "exec", node, "kill", "-9", pid)

    def restarted():
        q = [q for q in pods() if uid(q) == uid(old)]
        return q and status0(q[0]).get("restartCount", 0) > n and is_ready(q[0]) and q[0]
    try:
        q = wait("the old container to restart and be Ready", restarted, 240)
        print(f"      restarted: {n} -> {status0(q)['restartCount']}")
        r = mounted_rev(q)
        check(fails, r == own_rev, f"it restarted on its own templates (rev {own_rev or '(none)'}){'' if r == own_rev else f': FILES CHANGED to rev {r}'}")
        hold(SHORT_HOLD, before, fails)
        check(fails, bool(refused_by_quota()), "the roll was still stuck")
    except TimeoutError as e:
        fails.append(str(e))
    record("container-killed", fails)


def image_pull_arm(target):
    log("=== image-pull")
    fails, old, before = [], the_pod(), snapshot()
    was = mounted_rev(old)
    # The last image.tag wins over the target's own.
    target.upgrade("B", "--set", "image.tag=exercise-no-such-tag")

    def cannot_pull():
        for p in pods([uid(old)]):
            r = ((status0(p).get("state") or {}).get("waiting") or {}).get("reason")
            if r in ("ErrImagePull", "ImagePullBackOff"):
                return r
    print("      new pod:", wait("the new pod to fail to pull its image", cannot_pull, 120))
    witness_replaced(fails, old)
    hold(HOLD, before, fails)
    now = [p for p in pods() if uid(p) == uid(old)]
    check(fails, len(now) == 1 and is_ready(now[0]), "the old pod kept running")
    if now:
        r = mounted_rev(now[0])
        check(fails, r == was, f"its templates are its own (rev {was}){'' if r == was else f': FILES CHANGED to rev {r}'}")
    check(fails, bool(cannot_pull()), "the roll was stuck throughout")
    record("image-pull", fails)
    return old


def old_pod_deleted_arm(old):
    log("=== old-pod-deleted")
    fails, before = [], snapshot()
    names = template_names(old["spec"])
    sh("kubectl", "delete", "pod", "-n", CP_NS, old["metadata"]["name"], "--wait=true", "--timeout=120s")

    def replacement():
        return next((p for p in pods([uid(old)]) if p["spec"]["containers"][0]["image"] == old["spec"]["containers"][0]["image"]), None)
    new = wait("the old ReplicaSet's replacement pod", replacement, 60)
    if template_names(new["spec"]) != names:
        raise RuntimeError("the replacement is not the old spec")

    def failed_mount():
        return [e["message"] for e in kget("events") if e.get("reason") == "FailedMount"
                and e["involvedObject"].get("uid") == uid(new) and "not found" in e.get("message", "")]
    started = lambda: next((p for p in pods() if uid(p) == uid(new) and (status0(p).get("state") or {}).get("running")), None)
    wait("the replacement to start, or to say why it cannot", lambda: failed_mount() or started(), 120)
    msgs = failed_mount()
    if msgs:
        print(f"      {msgs[0][:200]}")
    check(fails, any(n in m for m in msgs for n in names.values()), "FailedMount names the old pod's deleted template ConfigMap")
    hold(SHORT_HOLD, before, fails)
    check(fails, not started(), "the old spec's replacement never ran" + (": OLD POD STARTED on the new templates" if started() else ""))
    record("old-pod-deleted", fails)


def values_only_arm(target):
    log("=== values-only")
    fails = []
    t0 = time.time()
    target.upgrade("B", "--set-string", "cdc.resyncInterval=2m")

    def rolled():
        d = kget("deployments.apps", STEM + "-controller")
        return d and any(a.startswith("-resync-interval=") and "2m" in a for a in d["spec"]["template"]["spec"]["containers"][0]["args"])
    try:
        wait("the values change on the controller", rolled, 180)
        print(f"      the controller carries it {time.time() - t0:.0f}s after the upgrade started")
        converged(target.image, "B")
    except TimeoutError as e:
        fails.append(str(e))
    record("values-only", fails)


def unknown_key_arm(target):
    log("=== unknown-key")
    fails, before, old = [], snapshot(), the_pod()
    keep = ("--set-string", "cdc.resyncInterval=2m")
    target.upgrade("B", *keep, unknown=True)
    wait("the new core-provider pod to replace the old", lambda: len(pods()) == 1 and pods([uid(old)]) and is_ready(pods()[0]), 240)

    def said():
        c = cd_condition("Synced")
        return c.get("status") == "False" and "exerciseUnknown" in c.get("message", "") and c["message"]

    def applied():
        cm = kget("configmaps", STEM + "-configmap")
        return "EXERCISE_UNKNOWN" in cm["data"] and cm["data"]["EXERCISE_UNKNOWN"]
    try:
        wait("the refusal, or the render", lambda: said() or applied(), 240)
    except TimeoutError as e:
        fails.append(str(e))
    v = applied()
    check(fails, not v, f"nothing rendered from it was applied{f': APPLIED as {v!r}' if v else ''}")
    check(fails, bool(said()), f"the definition's Synced names the key ({(said() or '-')[:160]})")
    check(fails, cd_condition("Ready").get("reason") == "NotObserved", f"Ready is NotObserved ({cd_condition('Ready').get('reason')})")
    if not v:
        diff = moved(before, snapshot())
        check(fails, not diff, f"no controller object moved ({diff})")
    target.upgrade("B", *keep)
    try:
        converged(target.image, "B")
    except TimeoutError as e:
        fails.append(f"recovery: {e}")
    record("unknown-key", fails)


# ---------------------------------------------------------------- setup

def serve_probe(chart):
    import base64
    d = tempfile.mkdtemp(dir=workdir)
    sh("helm", "package", chart, "-d", d)
    t = os.listdir(d)[0]
    labels = {"app": "charts"}
    objs = [
        {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": NS}},
        {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "charts", "namespace": NS},
         "binaryData": {t: base64.b64encode(open(os.path.join(d, t), "rb").read()).decode()}},
        {"apiVersion": "v1", "kind": "Service", "metadata": {"name": "charts", "namespace": NS}, "spec": {"selector": labels, "ports": [{"port": 8080}]}},
        {"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "charts", "namespace": NS},
         "spec": {"selector": {"matchLabels": labels}, "template": {"metadata": {"labels": labels}, "spec": {
             "containers": [{"name": "httpd", "image": "registry.k8s.io/e2e-test-images/busybox:1.36.1-1",
                             "command": ["httpd", "-f", "-p", "8080", "-h", "/srv"],
                             "volumeMounts": [{"name": "c", "mountPath": "/srv"}]}],
             "volumes": [{"name": "c", "configMap": {"name": "charts"}}]}}}}]
    sh("kubectl", "apply", "-f", "-", inp=json.dumps({"apiVersion": "v1", "kind": "List", "items": objs}))
    sh("kubectl", "rollout", "status", "-n", NS, "deployment/charts", "--timeout=180s")
    return f"http://charts.{NS}.svc.cluster.local:8080/{t}"


def setup(o):
    """The released chart FROM which everything starts, and one definition with a running controller."""
    sync = json.loads(sh("kubectl", "get", "--raw", f"/api/v1/nodes/{o.node}/proxy/configz"))["kubeletconfig"]["syncFrequency"]
    if sync != "10s":
        raise RuntimeError(f"the kubelet syncs every {sync}, not 10s: the holds would not outlast its refresh (kind.yaml)")
    for c, extra in (("core-provider-crds", []), ("core-provider", COMMON)):
        log(f"installing {c} {o.base}")
        sh("helm", "upgrade", "--install", c, pulled(c, o.base), "-n", CP_NS, "--create-namespace", "--wait", "--timeout", "8m", *extra)
    sh("kubectl", "apply", "-f", "-", inp=json.dumps({
        "apiVersion": "core.krateo.io/v1alpha1", "kind": "CompositionDefinition", "metadata": {"name": "hookprobe", "namespace": NS},
        "spec": {"chart": {"url": serve_probe(o.probe), "version": "0.1.0"}}}))
    wait("the CompositionDefinition to be Ready", lambda: cd_condition("Ready").get("status") == "True", 300)
    img = the_pod()["spec"]["containers"][0]["image"]
    if not img.endswith(":" + o.base):
        raise RuntimeError(f"core-provider runs {img}, want {o.base}")
    log(f"FROM {o.base}: ready; watching {len(snapshot())} controller objects")


def main_suite(o, target):
    def arm(name, fn):
        try:
            return fn()
        except Exception as e:
            results.append((name, "ERROR", repr(e)))
            log(f"{name}: ERROR {e!r}")
    target.upgrade_crds()
    # The FROM pod's own templates carry no EXERCISE_REV.
    old = arm("quota-full", lambda: stuck_on_quota("quota-full", lambda: target.upgrade("A")))
    if old:
        arm("container-killed", lambda: container_killed_arm(o.node, old, ""))
    lift_quota()
    arm("quota-lifted", lambda: converge_arm("quota-lifted", target.image, "A"))
    old = arm("image-pull", lambda: image_pull_arm(target))
    if old:
        arm("old-pod-deleted", lambda: old_pod_deleted_arm(old))
    target.upgrade("B")
    arm("image-fixed", lambda: converge_arm("image-fixed", target.image, "B"))
    newer = revision()

    def rollback(name, to, image, rev):
        def run():
            stuck_on_quota(name, lambda: sh("helm", "rollback", REL, str(to), "-n", CP_NS))
            # Part of the same arm: the rollback itself must still happen once there is room.
            lift_quota()
            r = results.pop()
            fails = [r[2]] if r[2] else []
            try:
                converged(image, rev)
            except TimeoutError as e:
                fails.append(str(e))
            record(name, fails)
        try:
            run()
        except Exception as e:
            results[:] = [r for r in results if r[0] != name]
            results.append((name, "ERROR", repr(e)))
            log(f"{name}: ERROR {e!r}")
            if kget("resourcequotas", "full"):
                lift_quota()
    rollback("rollback-back", 1, o.base, "")
    rollback("rollback-forward", newer, target.image, "B")
    arm("values-only", lambda: values_only_arm(target))
    arm("unknown-key", lambda: unknown_key_arm(target))


def from_old_suite(o, target):
    target.upgrade_crds()
    for name, fn in (
            ("from-old", lambda: stuck_on_quota("from-old", lambda: target.upgrade("A"))),
            ("from-old-lifted", lambda: (lift_quota(), converge_arm("from-old-lifted", target.image, "A", 600)))):
        try:
            fn()
        except Exception as e:
            results.append((name, "ERROR", repr(e)))
            log(f"{name}: ERROR {e!r}")


def diagnostics():
    print("::group::diagnostics")
    for a in (["kubectl", "get", "pods,replicasets,deployments,configmaps,resourcequotas,leases", "-n", CP_NS, "-o", "wide"],
              ["kubectl", "get", "pods", "-n", CP_NS, "-l", "app.kubernetes.io/name=core-provider", "-o", "yaml"],
              ["kubectl", "get", "compositiondefinitions.core.krateo.io", "-n", NS, "-o", "yaml"],
              ["kubectl", "get", "events", "-n", CP_NS, "--sort-by=.lastTimestamp"],
              ["helm", "history", REL, "-n", CP_NS],
              ["kubectl", "logs", "-n", CP_NS, "-l", "app.kubernetes.io/name=core-provider", "--tail=200", "--prefix"],
              ["kubectl", "logs", "-n", CP_NS, "-l", "app.kubernetes.io/name=hookprobes-v0-1-0-controller", "--tail=100", "--prefix"]):
        print("$", " ".join(a))
        print(sh(*a, check=False)[-20000:])
    print("::endgroup::", flush=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--suite", choices=sorted(REQUIRED), default="main")
    ap.add_argument("--node", required=True, help="the kind node's container name")
    ap.add_argument("--probe", required=True, help="the probe definition's chart directory")
    ap.add_argument("--from", dest="base", required=True, help="the released version everything starts from")
    ap.add_argument("--target-version", help="a released version to upgrade to")
    ap.add_argument("--target-chart", help="or a chart directory, with --crds-chart and --image-tag")
    ap.add_argument("--crds-chart")
    ap.add_argument("--image-tag", help="core-provider and the composition controller run this tag, loaded on the node")
    ap.add_argument("--chart-inspector-tag", help="with --image-tag: the released chart-inspector to run")
    ap.add_argument("--control", action="store_true", help="a target before #651: exactly its known arms must FAIL")
    o = ap.parse_args()
    if bool(o.target_version) == bool(o.target_chart) or bool(o.target_chart) != bool(o.crds_chart and o.image_tag and o.chart_inspector_tag):
        ap.error("give --target-version, or --target-chart with --crds-chart, --image-tag and --chart-inspector-tag")
    start = time.time()
    try:
        setup(o)
        {"main": main_suite, "from-old": from_old_suite}[o.suite](o, Target(o))
    except Exception as e:
        results.append(("setup", "ERROR", repr(e)))
    log(f"=== results: {o.suite}, {o.base} -> {o.target_version or o.image_tag}, {time.time() - start:.0f}s")
    for arm, verdict, detail in results:
        print(f"  {verdict:6} {arm:18} {detail}")
    bad = problems(o.suite, o.control)
    print(f"  {len([r for r in results if r[0] in REQUIRED[o.suite]])} of {len(REQUIRED[o.suite])} arms ran")
    for p in bad:
        print("  " + p)
    if bad:
        diagnostics()
    print("upgrade-templates exercise" + (" CONTROL" if o.control else ""), "FAILED" if bad else "passed")
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
