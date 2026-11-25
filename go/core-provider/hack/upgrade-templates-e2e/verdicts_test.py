#!/usr/bin/env python3
"""What run.py concludes, without a cluster: which runs are red, and what the replacement of a deleted old
pod may do. The workflow runs this before it creates one (python3 -I verdicts_test.py)."""
import importlib.util, os, sys, unittest

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("run", os.path.join(os.path.dirname(os.path.abspath(__file__)), "run.py"))
run = importlib.util.module_from_spec(spec)
spec.loader.exec_module(run)


class Problems(unittest.TestCase):
    def red(self, results, control, suite):
        run.results[:] = results
        return bool(run.problems(suite, control))

    def test_every_run(self):
        for suite, arms in run.REQUIRED.items():
            fixed = [(a, "PASS", "") for a in arms]
            before = [(a, "FAIL", f"x: {run.FAIL_BEFORE_651[a]} y") if a in run.FAIL_BEFORE_651 else (a, "PASS", "") for a in arms]
            self.assertFalse(self.red(fixed, False, suite))
            self.assertFalse(self.red(before, True, suite))
            self.assertTrue(self.red(fixed, True, suite), "a control that fails nothing")
            self.assertTrue(self.red(before, False, suite), "a version without the fix")
            self.assertTrue(self.red([], False, suite))
            self.assertTrue(self.red([], True, suite))
            for control, base in ((False, fixed), (True, before)):
                for i, (arm, verdict, detail) in enumerate(base):
                    with_ = lambda *r: base[:i] + [r] + base[i + 1:]
                    self.assertTrue(self.red(base[:i] + base[i + 1:], control, suite), f"{arm} missing")
                    self.assertTrue(self.red(base + [base[i]], control, suite), f"{arm} twice")
                    self.assertTrue(self.red(with_(arm, "ERROR", detail), control, suite), f"{arm} ERROR")
                    flipped = "FAIL" if verdict == "PASS" else "PASS"
                    self.assertTrue(self.red(with_(arm, flipped, detail), control, suite), f"{arm} flipped")
                    if verdict == "FAIL":
                        self.assertTrue(self.red(with_(arm, "FAIL", "another reason"), control, suite), f"{arm} for another reason")

    def test_the_control_fails_only_arms_that_exist(self):
        self.assertLessEqual(set(run.FAIL_BEFORE_651), {a for arms in run.REQUIRED.values() for a in arms})


class Replacement(unittest.TestCase):
    OWN = {"core-provider-cdc-configmap-aaaaaaaaaa", "core-provider-rbac-configmap-bbbbbbbbbb"}
    MISSING = {"core-provider-cdc-configmap-aaaaaaaaaa"}

    def v(self, rev=None, errors=(), cache=True, own_rev="A", missing=None):
        return run.replacement_verdict(own_rev, self.OWN, self.MISSING if missing is None else missing, list(errors), rev, cache)

    @staticmethod
    def err(name):
        return f'MountVolume.SetUp failed for volume "cdc-configmap" : configmap "{name}" not found'

    def test_runs_on_its_own(self):
        ok, what = self.v("A")
        self.assertTrue(ok)
        self.assertIn("rev A", what)
        self.assertTrue(self.v("", own_rev="")[0], "a released chart's templates carry no rev")

    def test_runs_on_another_rev(self):
        for rev in ("B", ""):
            for cache in (True, False):
                # Also when it said it could not mount at first.
                for errors in ([], [self.err("core-provider-cdc-configmap-aaaaaaaaaa")]):
                    ok, what = self.v(rev, errors, cache)
                    self.assertFalse(ok)
                    self.assertIn("OLD POD STARTED", what)  # what the control must say
        self.assertFalse(self.v("A", own_rev="")[0])

    def test_runs_with_nothing_to_mount_from(self):
        ok, what = self.v("A", cache=False)
        self.assertFalse(ok)
        self.assertIn("OLD POD STARTED", what)

    def test_cannot_mount_its_own(self):
        for cache in (True, False):
            ok, what = self.v(errors=[self.err("core-provider-cdc-configmap-aaaaaaaaaa")], cache=cache)
            self.assertTrue(ok)
            self.assertIn("core-provider-cdc-configmap-aaaaaaaaaa", what)
            self.assertTrue(self.v(errors=[self.err("other"), self.err("core-provider-cdc-configmap-aaaaaaaaaa")], cache=cache)[0])

    def test_failed_mount_of_something_else(self):
        for name in ("core-provider-cdc-configmap-cccccccccc",  # the new chart's
                     "core-provider-cdc-configmap", "kube-root-ca.crt",
                     "core-provider-cdc-configmap-aaaaaaaaaa2",  # its own is a prefix
                     "core-provider-rbac-configmap-bbbbbbbbbb"):  # its own, but still there
            for cache in (True, False):
                self.assertFalse(self.v(errors=[self.err(name)], cache=cache)[0], name)
        self.assertFalse(self.v(errors=[self.err("core-provider-cdc-configmap-aaaaaaaaaa")], missing=set())[0])

    def test_nothing_happened(self):
        for errors in ([], ["MountVolume.SetUp failed: failed to sync configmap cache: timed out waiting for the condition"]):
            self.assertFalse(self.v(errors=errors)[0])


class ReplacedOldPodArm(unittest.TestCase):
    """The arm itself over a fake cluster: what it reads, when, and what it records."""
    OWN = "core-provider-cdc-configmap-aaaaaaaaaa"

    @staticmethod
    def pod(name, image, running=True):
        return {"metadata": {"name": name, "uid": name}, "spec": {
            "containers": [{"image": image}],
            "volumes": [{"name": "cdc-configmap", "configMap": {"name": ReplacedOldPodArm.OWN}}, {"name": "token", "projected": {}}]},
            "status": {"containerStatuses": [{"ready": running, "state": {"running": {"startedAt": "now"}} if running else {"waiting": {"reason": "ContainerCreating"}}}]}}

    def arm(self, rev=None, error=None, cache=True, then=None, rolls=False, exists=()):
        """rev: what the replacement's container reads, None: it does not run. then: the same, after the hold."""
        old, bad = self.pod("old", "cp:1"), self.pod("bad", "cp:no-such-tag", False)
        state = {"rev": rev, "error": error, "deleted": False, "snap": 0}

        def kget(kind, name=None, ns=None, selector=None):
            if kind == "configmaps":
                return [{"metadata": {"name": n}} for n in ("core-provider-cdc-configmap-new0000000", *exists)]
            if kind == "events":
                if not state["error"]:
                    return []
                return [{"reason": "FailedMount", "involvedObject": {"uid": "new"}, "message": f'configmap "{state["error"]}" not found'},
                        {"reason": "FailedMount", "involvedObject": {"uid": "another"}, "message": f'configmap "{self.OWN}" not found'}]
            raise AssertionError(kind)

        def pods(old_uids=()):
            ps = [old, bad] + ([self.pod("new", "cp:1", state["rev"] is not None)] if state["deleted"] else [])
            return [p for p in ps if run.uid(p) not in old_uids]

        def sh(*a, **kw):
            assert a[:3] == ("kubectl", "delete", "pod") and "old" in a, a
            state["deleted"] = True
            return ""

        def hold(seconds, before, fails, stays_ready=True):
            self.assertEqual(stays_ready, cache)
            if rolls:
                fails.append("ROLLED: x")
            if then:
                state.update(then)

        fakes = dict(kget=kget, pods=pods, sh=sh, hold=hold, snapshot=lambda: {}, log=lambda m: None, print=lambda *a, **k: None,
                     mounted_rev=lambda p: state["rev"])
        saved = {k: getattr(run, k) for k in fakes if hasattr(run, k)}
        run.results[:] = []
        try:
            for k, v in fakes.items():
                setattr(run, k, v)
            new = run.replaced_old_pod_arm("a", old, "A", cache)
        finally:
            for k in fakes:
                if k in saved:
                    setattr(run, k, saved[k])
                else:
                    delattr(run, k)
        self.assertEqual(run.uid(new), "new")
        self.assertEqual(len(run.results), 1)
        return run.results[0][1:]

    def test_fine(self):
        verdict, note = self.arm(rev="A")
        self.assertEqual(verdict, "PASS")
        self.assertIn("runs on its own templates", note)
        for cache in (True, False):
            verdict, note = self.arm(error=self.OWN, cache=cache)
            self.assertEqual(verdict, "PASS")
            self.assertIn("cannot mount", note)
        # It could not mount, and did after all, on its own.
        self.assertEqual(self.arm(error=self.OWN, then={"rev": "A"})[0], "PASS")

    def test_not_fine(self):
        for kw in (dict(rev="B"), dict(rev="B", cache=False), dict(rev="A", cache=False), dict(error="another"),
                   dict(error=self.OWN, exists=(self.OWN,)), dict(rev="A", rolls=True), dict(error=self.OWN, rolls=True),
                   # what only the look after the hold sees
                   dict(rev="A", then={"rev": "B"}), dict(error=self.OWN, then={"rev": "B"}), dict(error=self.OWN, cache=False, then={"rev": "A"})):
            self.assertEqual(self.arm(**kw)[0], "FAIL", kw)
        # The control: the fixed names exist, and the replacement runs the new chart's files.
        for cache in (True, False):
            verdict, detail = self.arm(rev="B", cache=cache, exists=(self.OWN,))
            self.assertEqual(verdict, "FAIL")
            self.assertIn(run.FAIL_BEFORE_651["old-pod-deleted"], detail)
            self.assertIn(run.FAIL_BEFORE_651["old-pod-no-cache"], detail)



if __name__ == "__main__":
    unittest.main()
