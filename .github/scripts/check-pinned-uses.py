#!/usr/bin/env python3
"""Every `uses:` in this repository's workflows is pinned by commit, so no push elsewhere changes what our CI runs.

    python3 -I .github/scripts/check-pinned-uses.py [repo-root]   # self-test, then the scan

Why: core-provider 2.13.125 was never published. Its release-tag run failed at STARTUP (no job, no log) because
krateo-platformops/.github's release-oci.yaml, called `@main`, gained a required input (77bcf7f, release-oci 2.0.0)
with no commit in this repository. `@main` on a reusable workflow is not a weak pin, it is no pin. The same holds for a
third-party action's floating major tag (`@v6`). Diego decided (2026-11-23) to pin every `uses:`, as installer did in
#778. The krateo-platformops/.github rule this mirrors is that repository's .github/scripts/internal_pins.py.

Every `uses:` in .github/workflows/*.y*ml (and .github/actions/**/action.y*ml, if any) must be:
  - a 40-hex commit after `@`: not a branch, a tag or an abbreviated SHA;
  - commented with the version the commit is:
      krateo-platformops/.github  `# X.Y.Z` or `# main YYYY-MM-DD` (internal_pins.py's two forms; no timestamp, no `v`);
      any other owner             `# vX.Y.Z` or `# X.Y.Z` (an action's own tag) or `# main YYYY-MM-DD`.
Local `./...` paths move with this repository's commit and pass. `docker://` images are out of scope here.

NOT checked (needs the other repository's history): that the commit is on that repository's history and that a
`# main YYYY-MM-DD` date is the commit's own. The bump PR that moves a pin is where that is reviewed.

Fails when it scanned no file (a check that read nothing has checked nothing) and when its self-test misjudges a
planted line.
"""
import glob
import os
import re
import sys

INTERNAL = "krateo-platformops/.github/"
USES = re.compile(r"""^\s*(?:-\s+)?uses:\s*(['"]?)([^'"\s#]+)\1\s*(?:#\s*(.*?))?\s*$""")
SHA = re.compile(r"^[0-9a-f]{40}$")
DATED = re.compile(r"^main \d{4}-\d{2}-\d{2}$")
VERSION = re.compile(r"^\d+\.\d+\.\d+$")
V_VERSION = re.compile(r"^v\d+\.\d+\.\d+$")
MIN_FILES = 5


def problems(lines):
    """(line number, message) for every `uses:` that is not pinned as the header says."""
    out = []
    for n, line in enumerate(lines, 1):
        if line.lstrip().startswith("#"):
            continue
        m = USES.match(line)
        if not m:
            continue
        target, comment = m.group(2), (m.group(3) or "").strip()
        if target.startswith("./") or target.startswith("docker://"):
            continue
        if "@" not in target:
            out.append((n, "%s has no @ref: pin it by commit" % target))
            continue
        ref = target.rpartition("@")[2]
        if not SHA.match(ref):
            out.append((n, "%s is at %r, not a 40-hex commit: pin it by commit" % (target, ref)))
            continue
        internal = target.lower().startswith(INTERNAL)
        ok = bool(DATED.match(comment) or VERSION.match(comment) or (not internal and V_VERSION.match(comment)))
        if not ok:
            want = "`# X.Y.Z` or `# main YYYY-MM-DD`" if internal else "`# vX.Y.Z`, `# X.Y.Z` or `# main YYYY-MM-DD`"
            out.append((n, "%s has comment %r; it needs %s naming the pinned version" % (target, comment, want)))
    return out


def self_test():
    sha = "d23441a48e516b6c34aea4fa41551a30e30af803"
    cases = [
        # (line, should be refused)
        ("      - uses: actions/checkout@v4", True),
        ("      - uses: actions/checkout@v6.1.0 # v6.1.0", True),
        ("      - uses: actions/checkout@d23441a # v6.1.0", True),
        ("      - uses: actions/checkout@%s" % sha, True),
        ("      - uses: actions/checkout@%s # latest" % sha, True),
        ("      - uses: actions/checkout@%s # main 2026-11-21T19:43:02Z" % sha, True),
        ("    uses: krateo-platformops/.github/.github/workflows/release-oci.yaml@main", True),
        ("    uses: krateo-platformops/.github/.github/workflows/release-oci.yaml@%s # v2.0.0" % sha, True),
        ("    uses: actions/checkout", True),
        ("      - uses: actions/checkout@%s # v6.1.0" % sha, False),
        ("      - uses: actions/checkout@%s # 6.1.0" % sha, False),
        ("    uses: krateo-platformops/.github/.github/workflows/release-oci.yaml@%s # 2.0.0" % sha, False),
        ("    uses: krateo-platformops/.github/.github/workflows/release-oci.yaml@%s # main 2026-11-21" % sha, False),
        ("      - uses: './.github/actions/local'", False),
        ("      - uses: docker://alpine:3.20", False),
        ("      # uses: actions/checkout@v4", False),
    ]
    bad = []
    for line, refuse in cases:
        if bool(problems([line])) != refuse:
            bad.append("self-test misjudged %r (expected %s)" % (line, "refused" if refuse else "accepted"))
    return bad


def main(root):
    bad = self_test()
    if bad:
        for b in bad:
            print("::error::" + b)
        return 1
    files = sorted(glob.glob(os.path.join(root, ".github/workflows/*.y*ml"))
                   + glob.glob(os.path.join(root, ".github/actions/**/action.y*ml"), recursive=True))
    if len(files) < MIN_FILES:
        print("::error::scanned %d workflow files (expected at least %d): a check that read nothing has checked nothing"
              % (len(files), MIN_FILES))
        return 1
    total, seen = 0, 0
    for f in files:
        with open(f, encoding="utf-8") as fh:
            lines = fh.read().splitlines()
        seen += sum(1 for l in lines if USES.match(l) and not l.lstrip().startswith("#"))
        for n, msg in problems(lines):
            total += 1
            print("::error file=%s,line=%d::%s" % (os.path.relpath(f, root), n, msg))
    print("scanned %d files, %d uses: lines, %d unpinned" % (len(files), seen, total))
    if seen == 0:
        print("::error::found 0 uses: lines: the pattern matched nothing")
        return 1
    return 1 if total else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1] if len(sys.argv) > 1 else "."))
