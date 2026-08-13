---
type: Decision
title: "Design: Path A — self-seeding remote projection"
description: Make the projected cdc self-sufficient on the spoke — core-provider projects the CompositionDefinition CRD, chart-inspector and a shadow definition too.
resource: compositiondefinitions.core.krateo.io
tags: [design, multicluster, seeding]
status: implemented
timestamp: 2026-08-07T00:00:00Z
---

> **Status (re-verified 2026-08-07): implemented** — Path A Inc 1–3 landed (#32–#34):
> `deploy.SeedRemoteTarget` (`go/core-provider/internal/tools/deploy/remoteseed.go`)
> projects the embedded CRD + chart-inspector + shadow definition, with reachability
> and ref-counted teardown (e2e-covered in `remoteseed_e2e_test.go`).

# Design — Path A: self-seeding remote projection (make the projected cdc self-sufficient)

**Status:** proposed (2026-07-02) · **Owner:** core-provider · **Basis:** Phase-0 PoC findings
(`~/Downloads/phase0-remote-installer-poc-results.md`) + [[remote-installer-proposal.md]].

## Goal

A remote-targeted `CompositionDefinition` (`spec.deploy.targetRef` → a `KubernetesTarget`) should
reconcile a composition **end-to-end on the spoke with zero manual seeding**. Phase 0 proved
projection works (cdc + generated CRD + RBAC land on the spoke, target `Healthy`) but the projected
cdc is **not self-sufficient** — a composition instance only reconciles after four things are
supplied on the spoke by hand. Path A makes core-provider **project those too**.

## Current projection (grounded)

- `connector.Connect` (`internal/controllers/compositiondefinitions/compositiondefinitions.go:301-330`):
  when `clusterkube.IsRemote(cr.Spec.Deploy)`, swaps the provisioning clients
  (`ext.kube/dynamic/client`) to the spoke via `clusterkube.Remote` (`internal/tools/clusterkube/clusterkube.go:47-107`);
  `ext.mgmt` stays on the **hub** (holds the CR + status).
- `deploy.Deploy` applies the generated CRD + cdc Deployment + RBAC + SA through the **spoke** client
  (`opts.KubeClient`/`opts.DynClient`). The chart is fetched via `mgmt` (**hub**);
  `status.packageUrl` = `pkg.PackageURL()` (`chartfs.go:80`, passed through unchanged),
  set by `RefreshCompositionDefinitionStatus` (`compositiondefinitions.go:811`).
- Target health: `status.target.connectionStatus` Healthy/Down (`compositiondefinitions.go:362-371`).

## The four seed gaps and the fix for each

All fixes run in a new **remote-seed step** invoked only when `IsRemote`, applying through the
already-swapped spoke client — so they compose with the existing `Deploy` path and require no new
client plumbing.

### B — target namespace (easy)
The cdc is projected into the CR's namespace on the spoke, which may not exist there
(`observe failed: namespaces "<ns>" not found`).
**Fix:** ensure the namespace on the spoke before projecting the cdc. RBAC add: `namespaces: create`
on the target identity (`docs/how-to/remote-target-rbac.yaml`).

### C — CompositionDefinition co-location (core of Path A)
The spoke cdc resolves a package by **listing `compositiondefinitions.core.krateo.io` in its own
cluster** and reading `status.packageUrl` (cdc `internal/tools/archive/getter.go:187-203`,
`searchCompositionDefinition` @ 298). Projection places neither the CRD nor a CR on the spoke.
**Fix (two objects, via the spoke client):**
1. Apply the `compositiondefinitions.core.krateo.io` **CRD** onto the spoke (ship it as an embedded
   asset, like the existing cdc/rbac templates; apply idempotently).
2. Project a **shadow `CompositionDefinition`** on the spoke carrying the resolved status the cdc
   reads: `packageUrl`, `apiVersion`, `kind`, `resource` (mirror what the hub computed —
   `RefreshCompositionDefinitionStatus`). It is status-only bait for the getter; no controller runs
   it on the spoke (core-provider isn't there), so the status persists.

### C2 — packageUrl reachability (contract + validation)
`packageURL` is passed through unchanged, so it is spoke-reachable **iff** the chart URL is
(public OCI/HTTP = fine; a hub in-cluster `*.svc.cluster.local` URL is not).
**Fix:** no rewrite. Add a validation: when `IsRemote` and the resolved `packageUrl` looks
cluster-local (`.svc`, `.cluster.local`, a localhost/RFC-1918 host), set a
`RemoteChartUnreachable` condition on the CR instead of silently projecting a spoke-unresolvable URL.
Document the contract in `remote-target-credentials.md`.

### D — chart-inspector co-location (the heavy decision)
The projected cdc calls chart-inspector at `URL_CHART_INSPECTOR` (its projected configmap, a **hub**
Service DNS today) to compute per-composition RBAC → NXDOMAIN on the spoke.
- **D1 (recommended — true autonomy):** project a chart-inspector Deployment + SA + RBAC + ConfigMap
  onto the spoke (reuse the hub chart-inspector manifests core-provider already owns), and keep
  `URL_CHART_INSPECTOR` pointing at the now **spoke-local** chart-inspector Service. Matches the
  projection model (the spoke runs everything on its own SA); heaviest to implement.
- **D2 (lighter — cross-cluster call):** point the projected cdc's `URL_CHART_INSPECTOR` at a
  **spoke-reachable hub** chart-inspector (requires exposing hub chart-inspector to the spoke —
  ingress/LB + auth). Keeps one chart-inspector but adds a hub↔spoke request-path dependency.
**Recommendation: D1** — it preserves the "hub credential only at bootstrap; steady state is
spoke-local" property the whole design rests on.

## Idempotency, digest, teardown

- Seeded objects (namespace, CD CRD, shadow CD, chart-inspector set) are applied idempotently and
  kept **out of the composition deploy digest** (they are target infrastructure, not per-composition
  state — same rationale as the apiRefRBAC self-mapping fix).
- **Teardown:** extend the existing remote undeploy so deleting the hub CR cleans the shadow CD and
  (ref-counted) the chart-inspector set. Leave the CD CRD in place if other remote CDs on the same
  spoke still reference it (ref-count by label), else remove.

## Implementation increments

1. **Inc 1 — B + C** (namespace ensure + CD CRD asset + shadow-CD projection). Contained, unit-
   testable, and e2e-verifiable on kind: a remote `CompositionDefinition` gets past package
   resolution with no manual `compositiondefinitions` CRD / CR on the spoke.
2. **Inc 2 — D1** (project chart-inspector onto the spoke). Unblocks the RBAC-generation call →
   full composition-instance reconcile on the spoke, hands-off.
3. **Inc 3 — C2 validation + teardown + RBAC doc** and the full e2e (kind hub+spoke, then a real
   GKE spoke). Assert: apply one remote `CompositionDefinition`, a composition instance installs on
   the spoke with **zero** manual seeding.

## RBAC delta (target identity)

Add to `remote-target-rbac.yaml` (on top of the admissionregistration rule already shipped in
core-provider#29): `namespaces: create`. The CD-CRD apply and chart-inspector Deployment/SA/RBAC/CM
are already covered by the existing `customresourcedefinitions` / `deployments` /
`serviceaccounts,configmaps,services` / `roles,clusterroles,...` rules.

## Test plan

Reuse the Phase-0 kind harness (hub + spoke on the shared docker net, k8s ≥ 1.36; spoke kubeconfig
via the spoke control-plane container IP — a valid cert SAN). Success = one remote-targeted
`CompositionDefinition` for a clean-schema chart reconciles a composition instance on the spoke with
no hand-applied CRD / CompositionDefinition / chart-inspector.
