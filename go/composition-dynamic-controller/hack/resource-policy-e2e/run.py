#!/usr/bin/env python3
"""Measure helm.sh/resource-policy: keep on the composition controller's own upgrade and delete paths.

Installs the RELEASED core-provider charts and drives compositions of two probe charts: one templates a
CRD (with an object of that kind created outside any release), the other a cluster-scoped object of a
kind it does not own, which is the GatewayClass case. "Survives" means the SAME UID, read again after
the controller has had time to resync, for the object as well as for its CRD.
"""
import argparse, importlib.util, json, os, sys, time

here = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("ex", os.path.join(here, "..", "interrupted-install-e2e", "run.py"))
ex = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ex)
sh, kget, wait, log, check, uid, NS = ex.sh, ex.kget, ex.wait, ex.log, ex.check, ex.uid, ex.NS
RESYNC = 150  # the chart's resync is 60s: two resyncs and a margin, so a later prune would be seen
ANN = "helm.sh/resource-policy"
results = []


class Comp:
    def __init__(self, arm, plural, kind, version, enabled=True, keep=False):
        self.name, self.plural = arm, plural
        sh("kubectl", "apply", "-f", "-", inp=json.dumps({
            "apiVersion": f"composition.krateo.io/{version}", "kind": kind,
            "metadata": {"name": arm, "namespace": NS, "labels": {
                "krateo.io/release-name": f"rel-{arm}", "krateo.io/composition-version": version}},
            "spec": {"enabled": enabled, "keep": keep}}))
        self.rel = wait(f"{arm}: release", self._release, 180)
        wait(f"{arm}: anchor", lambda: self.anchor(enabled, keep), 180)

    def _release(self):
        for s in kget("secrets", selector="owner=helm"):
            n = s["metadata"]["labels"]["name"]
            if n.startswith(f"rel-{self.name}"):
                return n

    def revision(self):
        out = sh("kubectl", "get", "secrets", "-n", NS, "-l", f"owner=helm,name={self.rel}", "-o",
                 "jsonpath={range .items[*]}{.metadata.labels.version},{.metadata.labels.status}{'\\n'}{end}")
        return sorted((int(l.split(",")[0]), l.split(",")[1]) for l in out.split())[-1]

    def anchor(self, enabled, keep):
        """The anchor ConfigMap carries the values, so it says which render is live."""
        c = kget("configmaps", f"{self.rel}-anchor")
        want = {"enabled": str(enabled).lower(), "keep": str(keep).lower()}
        return c is not None and c["data"] == want

    def roll(self, enabled, keep):
        before = self.revision()
        sh("kubectl", "patch", f"{self.plural}.composition.krateo.io", self.name, "-n", NS, "--type", "merge",
           "-p", json.dumps({"spec": {"enabled": enabled, "keep": keep}}))
        try:
            wait(f"{self.name}: the roll", lambda: self.revision()[0] > before[0] and self.revision()[1] == "deployed"
                 and self.anchor(enabled, keep), 240)
            landed = True
        except TimeoutError as e:
            print(f"      {e}")
            landed = False
        print(f"      rolled to enabled={enabled} keep={keep}: {before} -> {self.revision()} landed={landed}")
        return landed

    def ready(self):
        o = kget(f"{self.plural}.composition.krateo.io", self.name)
        return next((f"{c['status']}/{c['reason']}" for c in ((o or {}).get("status") or {}).get("conditions") or []
                     if c["type"] == "Ready"), None)

    def delete(self):
        sh("kubectl", "delete", f"{self.plural}.composition.krateo.io", self.name, "-n", NS, "--wait=false")
        try:
            wait(f"{self.name}: deletion", lambda: kget(f"{self.plural}.composition.krateo.io", self.name) is None, 240)
        except TimeoutError as e:
            print(f"      {e}")


def obj(kind, name):
    return kget(kind, name, ns="default") if kind else None


class Crd:
    """The composition's CRD, and an object of that kind that belongs to no release."""

    def __init__(self, c):
        self.group = f"{c.rel}.probe.example.com"
        self.name = f"things.{self.group}"
        wait("CRD Established", lambda: self.get() and any(
            x["type"] == "Established" and x["status"] == "True" for x in self.get()["status"].get("conditions", [])), 120)
        sh("kubectl", "apply", "-f", "-", inp=json.dumps(
            {"apiVersion": f"{self.group}/v1", "kind": "Thing", "metadata": {"name": "mine"}, "spec": {"a": 1}}))
        self.crd, self.thing = uid(self.get()), uid(self.user())
        if not self.crd or not self.thing:
            raise RuntimeError("control: the CRD and the user object must exist first")

    def get(self):
        return obj("crds", self.name)

    def user(self):
        # A missing CRD makes the kind unknown, which is an error, not NotFound: gone either way.
        return kget(self.name, "mine", ns="default") if self.get() else None

    def annotated(self):
        return (self.get()["metadata"].get("annotations") or {}).get(ANN)

    def state(self):
        return {"crd": "same" if uid(self.get()) == self.crd else ("gone" if self.get() is None else "REPLACED"),
                "object": "same" if uid(self.user()) == self.thing else ("gone" if self.user() is None else "REPLACED")}


def settle(fn):
    """The state now and after the resyncs, so an answer that changes later is not missed."""
    time.sleep(20)
    first = fn()
    time.sleep(RESYNC)
    last = fn()
    print(f"      state {first}, after {RESYNC}s more {last}")
    return first, last


def arm(name, deciding, fn):
    log(f"=== {name}")
    fails, note = [], ""
    try:
        note = fn(fails) or ""
    except Exception as e:
        fails.append(repr(e))
    results.append((name, "FAIL" if fails else "PASS", deciding, "; ".join(fails) or note))


def crd_arms(v):
    mk = lambda a, **k: Comp(a, "crdprobes", "Crdprobe", v, **k)
    both = lambda s, w: s[0] == {"crd": w, "object": w} and s[1] == s[0]

    def no_keep(f):
        c = mk("crd-no-keep"); k = Crd(c)
        check(f, c.roll(False, False), "the roll landed")
        check(f, both(settle(k.state), "gone"), "CONTROL: without keep, the CRD and its object are deleted")
        return f"Ready {c.ready()}"

    def two_rolls(f):
        c = mk("crd-two-rolls"); k = Crd(c)
        check(f, c.roll(True, True), "roll 1 (introduce keep) landed")
        check(f, k.annotated() == "keep", f"the LIVE CRD carries keep between the rolls (got {k.annotated()!r})")
        check(f, c.roll(False, True), "roll 2 (condition off) landed")
        check(f, both(settle(k.state), "same"), "the CRD and its object SURVIVE, same UIDs")
        return f"Ready {c.ready()}"

    def one_roll(f):
        c = mk("crd-one-roll"); k = Crd(c)
        check(f, c.roll(False, True), "the single roll (keep + condition off) landed")
        s = settle(k.state)
        check(f, both(s, "gone"), "one roll that does both DELETES them (keep is read from the live object)")
        return f"{s[1]}"

    def hand_upgrade(f):
        c = mk("crd-hand-upgrade"); k = Crd(c)
        sh("kubectl", "annotate", "crd", k.name, f"{ANN}=keep")
        check(f, c.roll(False, False), "the roll landed")
        s = settle(k.state)
        check(f, both(s, "same"), "a hand-added live keep protects an upgrade")
        return f"{s[1]}"

    def hand_uninstall(f):
        c = mk("crd-hand-delete"); k = Crd(c)
        sh("kubectl", "annotate", "crd", k.name, f"{ANN}=keep")
        c.delete()
        s = settle(k.state)
        return f"hand-added live keep, then the composition is DELETED: {s[1]}"

    def chart_uninstall(f):
        c = mk("crd-chart-delete", keep=True); k = Crd(c)
        c.delete()
        s = settle(k.state)
        check(f, both(s, "same"), "keep from the chart protects a delete")
        return f"{s[1]}"

    arm("crd-no-keep (control)", True, no_keep)
    arm("crd-two-rolls", True, two_rolls)
    arm("crd-one-roll", False, one_roll)
    arm("crd-hand-upgrade", False, hand_upgrade)
    arm("crd-hand-delete", False, hand_uninstall)
    arm("crd-chart-delete", False, chart_uninstall)


def cr_arms(v):
    sh("kubectl", "apply", "-f", "-", inp=json.dumps({
        "apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": {"name": "gadgets.probe.example.com"},
        "spec": {"group": "probe.example.com", "scope": "Cluster", "names": {"kind": "Gadget", "plural": "gadgets", "singular": "gadget"},
                 "versions": [{"name": "v1", "served": True, "storage": True, "schema": {"openAPIV3Schema": {
                     "type": "object", "x-kubernetes-preserve-unknown-fields": True}}}]}}))
    sh("kubectl", "wait", "--for=condition=Established", "crd/gadgets.probe.example.com", "--timeout=60s")

    def run(name, keep, want, what):
        def f(fails):
            c = Comp(name, "crprobes", "Crprobe", v, keep=keep)
            g = lambda: obj("gadgets.probe.example.com", c.rel)
            u = uid(wait(f"{name}: the object", g, 120))
            live = (g()["metadata"].get("annotations") or {}).get(ANN)
            check(fails, live == ("keep" if keep else None), f"the live annotation before the roll is {live!r}")
            check(fails, c.roll(False, keep), "the roll (create: false) landed")
            s = settle(lambda: "same" if uid(g()) == u else ("gone" if g() is None else "REPLACED"))
            check(fails, s == (want, want), what)
            return f"{s[1]}; Ready {c.ready()}"
        return f

    arm("cr-no-keep (control)", True, run("cr-no-keep", False, "gone", "CONTROL: without keep, the object is deleted"))
    arm("cr-keep (the GatewayClass case)", True, run("cr-keep", True, "same", "with keep from the start, the object SURVIVES, same UID"))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--version", required=True)
    o = ap.parse_args()
    try:
        ex.install_core_provider(o.version)
        urls = ex.serve_charts(os.path.join(here, "crdprobe"), os.path.join(here, "crprobe"))
        vcrd = ex.define("crdprobe", urls["crdprobe"])
        vcr = ex.define("crprobe", urls["crprobe"])
        ex.check_images(o.version)
        cr_arms(vcr)
        crd_arms(vcrd)
    except Exception as e:
        results.append(("run", "ERROR", True, repr(e)))
    log(f"=== results, core-provider {o.version}")
    for n, v, d, note in results:
        print(f"  {v:6} {n:34} {'deciding' if d else 'measured'}  {note}")
    want = {"cr-no-keep (control)", "cr-keep (the GatewayClass case)", "crd-no-keep (control)", "crd-two-rolls"}
    missing = want - {r[0] for r in results}
    bad = missing or [r for r in results if r[2] and r[1] != "PASS"]
    if missing:
        print("  deciding arms that did not run:", sorted(missing))
    for r in results:
        if not r[2] and r[1] != "PASS":
            print(f"::warning::measured arm {r[0]}: {r[3]}")
    if bad or any(r[1] != "PASS" for r in results):
        print("::group::diagnostics")
        for a in (["kubectl", "get", "crds", "-o", "name"], ["kubectl", "get", "events", "-n", NS, "--sort-by=.lastTimestamp"],
                  ["kubectl", "get", "crdprobes.composition.krateo.io,crprobes.composition.krateo.io", "-n", NS, "-o", "yaml"]):
            print("$", " ".join(a)); print(sh(*a, check=False)[-15000:])
        for p in kget("pods", ns=ex.CP_NS):
            if "probes-" in p["metadata"]["name"]:
                print("--- controller", p["metadata"]["name"]); print(sh("kubectl", "logs", "-n", ex.CP_NS, p["metadata"]["name"], "--tail=500", check=False)[-40000:])
        print("::endgroup::")
    print("resource-policy exercise", "FAILED" if bad else "passed", "for core-provider", o.version)
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
