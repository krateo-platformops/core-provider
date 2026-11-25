#!/usr/bin/env python3
"""Every job in release-tag.yaml that PUBLISHES waits for every GATE job.

    python3 -I .github/scripts/check-release-gate.py [repo-root]   # self-test, then the check

Why (#680): the images waited for the test gate (#92), but release-oci had no `needs:` and published the three charts
at the tag push, concurrently with the tests, whatever they concluded. A declared test job is not a gate; `needs:` is.

Gate jobs: every job whose id starts with `test-`, and `release-ready-reasons` (#667's release-time reason check).
Publishing jobs: every job that calls a reusable workflow that publishes (component-image-build.yaml, release-oci.yaml).
Each publishing job's `needs:` must contain every gate job DIRECTLY (not only through another job), so removing one
`needs:` entry is caught where it is made.

Fails when the workflow has no gate job or no publishing job (a check that found nothing has checked nothing), and when
its self-test misjudges a planted workflow.
"""
import os
import sys

import yaml

PUBLISHERS = ("component-image-build.yaml", "release-oci.yaml")


def gate_jobs(jobs):
    return {j for j in jobs if j.startswith("test-") or j == "release-ready-reasons"}


def publishing_jobs(jobs):
    return {j for j, spec in jobs.items() if any(p in str((spec or {}).get("uses", "")) for p in PUBLISHERS)}


def problems(workflow):
    jobs = (workflow or {}).get("jobs") or {}
    gates, pubs = gate_jobs(jobs), publishing_jobs(jobs)
    out = []
    if not gates:
        out.append("no gate job found (test-* or release-ready-reasons)")
    if not pubs:
        out.append("no publishing job found (" + ", ".join(PUBLISHERS) + ")")
    for j in sorted(pubs):
        needs = jobs[j].get("needs") or []
        needs = {needs} if isinstance(needs, str) else set(needs)
        missing = sorted(gates - needs)
        if missing:
            out.append(f"{j} publishes without waiting for: {', '.join(missing)}")
    return out


def self_test():
    gate = {"test-a": {"uses": "x/component-go-checks.yaml@sha"}, "release-ready-reasons": {"runs-on": "u"}}
    good = {"jobs": {**gate,
                     "build": {"needs": ["test-a", "release-ready-reasons"], "uses": "x/component-image-build.yaml@sha"},
                     "release-oci": {"needs": ["test-a", "release-ready-reasons"], "uses": "x/release-oci.yaml@sha"}}}
    cases = [
        ("good", good, 0),
        ("release-oci without needs (#680)", {"jobs": {**good["jobs"], "release-oci": {"uses": "x/release-oci.yaml@sha"}}}, 1),
        ("build missing one gate", {"jobs": {**good["jobs"], "build": {"needs": "test-a", "uses": "x/component-image-build.yaml@sha"}}}, 1),
        ("no gate job", {"jobs": {"build": {"uses": "x/component-image-build.yaml@sha"}}}, 1),
        ("no publishing job", {"jobs": gate}, 1),
    ]
    bad = [name for name, wf, want in cases if len(problems(wf)) != want]
    if bad:
        print("self-test FAILED: misjudged " + "; ".join(bad))
        sys.exit(2)


def main():
    self_test()
    root = sys.argv[1] if len(sys.argv) > 1 else "."
    path = os.path.join(root, ".github", "workflows", "release-tag.yaml")
    with open(path, encoding="utf-8") as f:
        wf = yaml.safe_load(f)
    found = problems(wf)
    jobs = (wf or {}).get("jobs") or {}
    print(f"{path}: gate jobs {sorted(gate_jobs(jobs))}, publishing jobs {sorted(publishing_jobs(jobs))}")
    for p in found:
        print("FAIL: " + p)
    sys.exit(1 if found else 0)


if __name__ == "__main__":
    main()
