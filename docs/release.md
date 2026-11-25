---
type: Runbook
title: core-provider — release
description: How a release ships — one plain-semver tag builds the three module images and publishes the OCI chart; what lands where and what to verify.
resource: oci://ghcr.io/krateo-platformops/charts/core-provider
tags: [release, ci, oci, monorepo]
timestamp: 2026-08-07T00:00:00Z
---

# Release

One tag ships everything. This monorepo has a single version line: the three images
(`core-provider`, `composition-dynamic-controller`, `chart-inspector`) and the
`core-provider` chart all release at the tag's version — and the chart wires the CDC
and chart-inspector image tags to its `appVersion`, so the artifacts pair up by
construction (a CDC fix cannot ship without deploying, closing the #57/#67 gap —
see the history note in `helm/core-provider/values.yaml`).

## The runbook

1. **Merge to `main`** with PR CI green
   ([`release-pullrequest.yaml`](../.github/workflows/release-pullrequest.yaml)):
   validate-only multi-arch builds of all three images, per-module Go checks
   (`component-go-checks` reusable; race + coverage), the CRD-drift guard
   (`make generate` + `git diff crds/`, core-provider module only — the sole module
   with committed CRDs), chart lint + render smoke
   ([`lint.yaml`](../.github/workflows/lint.yaml)), security scanning, and the
   docs-standard lint.
2. **Make the reason tables release-ready for `X.Y.Z`** (#667), on a branch from the
   commit you will tag, and merge it before tagging. `docs/ready-reasons.json` and
   `docs/cd-ready-reasons.json` publish each reason's `sinceVersion`; a reason added since
   the last release says `"unreleased"`, and the release that ships it replaces that with
   its own tag and records it in the registry's `testdata/released-reasons.txt`:

   ```sh
   cd go/composition-dynamic-controller
   RELEASE_TAG=X.Y.Z UPDATE_READY_REASONS=1 go test -tags releasegate ./internal/conditionreason/ -run '^TestReasonTablesAreReleaseReady$'
   UPDATE_READY_REASONS=1 go test ./internal/composition/ -run TestReadyReasonTableIsCurrent
   cd ../core-provider
   RELEASE_TAG=X.Y.Z UPDATE_READY_REASONS=1 go test -tags releasegate ./internal/cdreason/ -run '^TestReasonTablesAreReleaseReady$'
   UPDATE_READY_REASONS=1 go test ./internal/cdreason/ -run TestCDReadyReasonTableIsCurrent
   ```

   The first command of each pair rewrites every `Since: Unreleased` (and `RemovedIn:
   Unreleased`) in the registry to `X.Y.Z` and appends the newly released reasons to
   `released-reasons.txt`; the second regenerates the JSON. When nothing is unreleased
   both are no-ops and there is nothing to commit. `release-tag.yaml`'s
   `release-ready-reasons` job runs the same test with `RELEASE_TAG` set to the pushed tag
   and without `UPDATE_READY_REASONS`, and `build` needs it: a tag whose tables still say
   `"unreleased"`, or hold a reason `released-reasons.txt` does not record, publishes
   nothing. If that happens, leave the tag (see below on re-tagging) and release the next
   version this way.

3. **Tag with plain semver — `X.Y.Z`, no `v` prefix — and make it ANNOTATED.** The
   release workflows trigger on `[0-9]+.[0-9]+.[0-9]+` only; a `v`-prefixed tag ships
   **nothing**, silently.

   ```sh
   git tag -a 2.12.5 -m "core-provider 2.12.5" && git push origin 2.12.5
   ```

   The `-a` is not decoration. An annotated tag is a real object carrying who tagged
   it, when, and why, and a consumer verifies a release by peeling it and comparing
   the result to `main`. A lightweight tag is a bare pointer: `git/ref/tags/X.Y.Z`
   resolves straight to a commit, `git/tags/<sha>` returns 404, and a reader cannot
   distinguish "tagged deliberately at this commit" from "tag moved here afterwards".
   Every platform repo ships annotated release tags and downstream verification steps
   assume it — this page previously showed the lightweight form, and 2.13.26 shipped
   lightweight as a result, which the installer session caught during its pre-roll
   verification.

   **If a tag is already pushed lightweight, leave it.** Re-tagging means deleting and
   re-pushing the ref, which re-triggers `release-tag` and `release-oci`: the images
   and charts are rebuilt and republished under the same version, the digests need not
   match the ones already verified and rolled, and the fleet's deployed artifact stops
   corresponding to what the registry serves. Fix it on the next tag.

4. **CI builds and publishes**, no manual steps:
   - [`release-tag.yaml`](../.github/workflows/release-tag.yaml) → the shared
     `component-image-build` workflow (`krateo-platformops/.github`) builds the three
     multi-arch (amd64+arm64) images →
     `ghcr.io/krateo-platformops/{core-provider,composition-dynamic-controller,chart-inspector}:X.Y.Z`.
     (chart-inspector builds from the shared `go/` context — its Dockerfile copies
     the whole tree.)
   - [`release-oci.yaml`](../.github/workflows/release-oci.yaml) (the canonical
     byte-identical org workflow) discovers **every first-class chart under
     `helm/`**, substitutes the `Chart.yaml` placeholders
     (`CHART_VERSION`/`APP_VERSION` → the tag), packages and pushes to
     `oci://ghcr.io/krateo-platformops/charts/`: `core-provider:X.Y.Z`,
     `core-provider-target:X.Y.Z` (the remote-target prerequisites chart,
     `helm/target-chart/`), and `core-provider-crds:X.Y.Z` (`helm/core-provider-crds/`,
     versioned from the tag like the others since #157).

5. **Verify** the artifacts pair up:

   ```sh
   helm show chart oci://ghcr.io/krateo-platformops/charts/core-provider --version X.Y.Z
   # appVersion in the output must equal X.Y.Z
   ```

6. **Roll it out** by bumping the Krateo installer's core-provider chart pin, or
   `helm upgrade` on a standalone install. Existing CDCs re-roll to the release's CDC
   image on their definitions' next reconcile (digest change — expected, fleet-wide).

   **If you capture the live values first, pass `-o yaml`.** `helm get values` defaults
   to `table`, which prefixes the YAML with a `USER-SUPPLIED VALUES:` heading. The
   heading has no indented body, so YAML parses it as a top-level key with a null
   value, sitting *beside* the real keys rather than above them — the values themselves
   are preserved and nothing is shadowed. The problem is the extra key: `core-provider`
   and `core-provider-crds` both set root `additionalProperties: false` in their
   `values.schema.json`, so `helm upgrade -f` on the captured file is rejected outright:

   ```
   Error: values don't meet the specifications of the schema(s) in the following chart(s):
   core-provider:
   - at '': additional properties 'USER-SUPPLIED VALUES' not allowed
   ```

   The failure is loud and self-diagnosing — it names the offending key. `-o yaml` emits
   the values alone:

   ```sh
   helm --kube-context=$CTX get values <release> -n <ns> -o yaml > live-values.yaml
   #                                                       ^^^^^^^^ not optional
   ```

   (Verified on helm v3.19.0 against the published 2.13.27 charts. `target-chart` ships
   no `values.schema.json`, and there the stray null key is simply ignored and the render
   succeeds with every value intact — so `-o yaml` is hygiene plus a guard for the strict
   charts, not a defence against value loss.)

## CRD changes

The two owned CRDs are generated from the Go types (`make generate` in
`go/core-provider/`), committed under `go/core-provider/crds/`, and drift-gated in PR
CI. The `core-provider` chart does **not** package them — the Krateo installer's
bootstrap applies them (standalone installs: `kubectl apply -f go/core-provider/crds/`
or the `core-provider-crds` chart at the same version, whose templates must be
refreshed when the CRDs change; `TestShippedCRDChartIsTheGeneratedCRDs` fails until they
are, and `TestServedKindsRequiredIsEveryGeneratedCRD` until the engine's startup check
(#589) lists every served Kind), and the engine
embeds the `CompositionDefinition` CRD for remote-target seeding
(`internal/tools/deploy/remoteseed.go`). The old cross-repo `crds → chart-repo`
publish job was dropped with the monorepo fold (#65). The generated *composition*
CRDs are runtime artifacts — never released, always derived from the managed charts.
