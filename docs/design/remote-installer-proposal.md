---
type: Decision
title: "Proposal: deploy a remote Krateo installer from the hub"
description: Provision a full Krateo onto a spoke by deploying the installer umbrella as a remote-targeted composition.
resource: compositiondefinitions.core.krateo.io
tags: [design, multicluster, installer, archive]
status: superseded-by:remote-composition-mirror.md
timestamp: 2026-08-07T00:00:00Z
---

> **Status (re-verified 2026-08-07): superseded.** The projection mechanics this
> proposal builds on shipped (Path A, [self-seeding](./remote-projection-self-seeding.md)),
> but its `RemoteInstall` modeling was replaced by the
> [remote composition mirror](./remote-composition-mirror.md) and `RemoteInstall`
> was removed (#56). Point-in-time document pinned to pre-monorepo SHAs — read as
> history, not current truth.

# Proposal — Deploy a remote Krateo installer from the hub Krateo

**Goal:** let an existing ("hub") Krateo provision a **full Krateo platform onto a remote ("spoke") cluster** — declaratively, through Krateo's own machinery — by deploying the **installer umbrella** (bootstrap mode) onto the spoke, which then self-bootstraps.

**Source of truth:** latest `main` of
- `krateo-platformops/core-provider` @ `ad8129cb` (OTel telemetry commit)
- `krateo-platformops/composition-dynamic-controller` (now `go/composition-dynamic-controller/` in the core-provider monorepo) @ `44d84ca3` (OTel traceparent commit; helm via `krateo-platformops/plumbing v1.7.7`)

---

## TL;DR — the hard part is already shipped

Multi-cluster remote-targeting is **already implemented, wired, and e2e-validated on core-provider `main`** (per `docs/design/multicluster-compositions.md`). This is therefore **~90% a usage/composition-modeling + validation task, not a feature build.** The chosen architecture is **controller projection**: the hub's core-provider uses a spoke kubeconfig to deploy the per-composition cdc (+ generated CRD + RBAC) **onto the spoke**, where it then installs charts using the **spoke's own ServiceAccount**. The hub credential is needed only at bootstrap; steady state is spoke-local.

What's missing for the *installer* specifically is narrow: model the umbrella as a remote-targeted composition, size the bootstrap RBAC, handle the self-bootstrap-on-spoke recursion, and **actually validate a full umbrella-on-spoke run** (only the CRD/connectivity path has been e2e-covered).

---

## 1. The shipped mechanism (core-provider, projection model)

| Piece | Where | What it does |
|---|---|---|
| `KubernetesTarget` (cluster-scoped) | `apis/compositiondefinitions/v1alpha1/types.go:189-217` | `spec.kubeconfigRef` → a Secret key holding the **spoke's kubeconfig** (ESO-rotatable). Passive ref, no own controller. |
| `CompositionDefinition.spec.deploy.targetRef` | `types.go:94-118` | points a CD at a `KubernetesTarget`. **Omitted ⇒ local** (backward-compatible). |
| `clusterkube.Remote()` | `internal/tools/clusterkube/clusterkube.go:47-107` | reads the target + its kubeconfig Secret → `clientcmd.RESTConfigFromKubeConfig` → full spoke client set; records Secret resourceVersion for rotation. |
| `Connect()` client swap | `internal/controllers/compositiondefinitions/compositiondefinitions.go:281-317` | when `IsRemote`, swaps the provisioning clients (`kube`/`dynamic`/`client`) to the **spoke**; keeps `mgmt` = hub (CR + status). |
| cdc projection | `internal/tools/deploy/deploy.go:450-465` | applies the cdc **Deployment**, ConfigMap, Service, generated CRD, SA + RBAC **into the spoke** via the swapped client. |
| Target health | `compositiondefinitions.go:340-358`, `types.go:251-270` | per-CD `status.target.{connectionStatus,version,kubeconfigSecretResourceVersion}` + Secret-watch re-enqueue. |
| Docs | `docs/design/multicluster-compositions.md`, `docs/how-to/remote-target-rbac.yaml`, `remote-target-credentials.md` | the design + the minimum spoke RBAC + ESO credential recipes. |

**Validated:** the CRD/connectivity path against a real GKE spoke. **Not yet covered (flagged in the design doc):** a full controller reconcile with the real cdc image across two clusters.

## 2. The cdc reality — and why it's fine under projection

The cdc is **single-cluster by construction**: one `*rest.Config` in `main.go:177-187`, threaded everywhere (`h.kubeconfig`); helm + dynamic clients are rebuilt from it per op (`composition.go:203/287/474/615/729`). There is **no** target-cluster field in the composition spec.

That is **correct for the projection model** — the cdc is *deployed onto the spoke* and runs locally there, so it never needs to "target a remote cluster." The hub's core-provider does the remoting; the cdc just runs where it's placed.

> The cdc analysis also scoped a *second* architecture — **central remote-apply** (one hub cdc targeting the spoke per-composition via a `kubeconfigSecretRef`). The plumbing wrapper even has a latent seam for it (`InstallConfig.RestConfig`, `RESTClientGetter` accepting raw kubeconfig). **But the design explicitly chose projection over remote-apply**, so this is the *alternative*, not the recommended path.

## 3. Proposed architecture (recommended — projection)

```
HUB Krateo                                              SPOKE cluster
──────────                                              ─────────────
KubernetesTarget(spoke)  ── kubeconfigRef ─► Secret
        ▲
        │ targetRef
CompositionDefinition(installer-umbrella, bootstrap values)
        │
   core-provider  ── clusterkube.Remote() + Connect() swap ──►  projects:
                                                          • installer CRD
                                                          • installer cdc Deployment (runs on SPOKE SA)
                                                          • RBAC/SA
        │
Composition(installer)  ──────────────────────────────►  spoke cdc helm-installs
   (CR + status stay on HUB)                              the umbrella (bootstrap mode)
                                                                   │
                                                          umbrella SELF-BOOTSTRAPS:
                                                          spoke gets its OWN core-provider/cdc/CRDs
                                                          → spoke reconciles its own platform (autonomous)
```

**Ownership split (by design):** the **hub** owns the `Composition` CR + drives reconcile; the **spoke** owns the release + all workloads + (after bootstrap) its own engine. Deleting the hub CR → uninstall on the spoke.

## 4. Two options

| | **A — Remote-targeted CompositionDefinition (recommended)** | **B — Central remote-apply from the hub cdc (alternative)** |
|---|---|---|
| Model | umbrella = a CD with `spec.deploy.targetRef` → spoke | one hub cdc, per-composition `kubeconfigSecretRef` → spoke |
| Code change | **~none** in core-provider (uses shipped path) | cdc change: add `kubeconfigSecretRef`, resolve `spokeCfg`, pass to `helm.NewClient` + RBAC-apply client (keep CR/status + chart-inspector hub-local) |
| Design fit | the **shipped, intended** model | the model the design **rejected** |
| cdc location | runs **on the spoke** | runs **on the hub**, targets spoke |
| Best when | standing up a managed Krateo fleet | one-shot installs without placing a cdc on the spoke |

**Recommendation: Option A.** It uses what's already shipped and validated, and matches the design.

## 5. Installer-specific gaps & caveats (the real work)

1. **Model the umbrella as a composition.** The installer is a Helm chart, so it fits a `CompositionDefinition.spec.chart` directly; supply **bootstrap-mode values** (`bootstrap.coreProvider.enabled=true`, exposure, etc.).
2. **Self-bootstrap-on-spoke recursion.** The umbrella is itself a self-bootstrapping Krateo. Projecting it means the **spoke** ends up with its *own* core-provider/cdc — the hub's core-provider only projects the umbrella's top-level cdc, it does **not** reconcile the spoke's inner compositions. Validate the umbrella's `crds/`-chart + the cdc-restart hook fire correctly on the spoke (stale CRD-discovery cache is a known footgun).
3. **Bootstrap RBAC is broad.** A whole-umbrella install needs the hub kubeconfig to have **near-cluster-admin on the spoke during bootstrap** (CRDs, RBAC incl. `bind`/`escalate`, operators) — far broader than the single-cdc `remote-target-rbac.yaml`. Mitigate with ESO short-lived/scoped creds; lean on the fact that *steady-state* installs use the spoke's own SA. **Phase-0 finding (2026-07-01):** even the single-cdc `remote-target-rbac.yaml` now additionally needs `admissionregistration.k8s.io` (mutating/validating admission policies + bindings), because core-provider >= 2.5.0 projects a composition-version MutatingAdmissionPolicy onto the target — the bundle it's fixed in this PR.
4. **Credential security.** Already structurally sound (native Secret, per-reconcile re-read, Secret-watch, no plaintext in CR/logs). Harden the ESO rotation boundary (surface `Down` only after a threshold).
5. **No first-class "spoke / remote-install" object.** `KubernetesTarget` is a passive ref with no status/lifecycle. Optional follow-on: give it a `KubernetesTargetStatus` + a lightweight reachability reconciler, and/or a thin `RemoteInstall` CR (= target + umbrella chart + bootstrap values) so "install Krateo on this spoke" is **one declarative object**.
6. **UNPROVEN end-to-end.** A real umbrella-on-spoke run has not been exercised — this is the #1 validation item.

## 6. Phased plan

- **Phase 0 — de-risk the shipped path (days):** PoC a remote-targeted CD for a *simple* chart (not the umbrella) → confirm core-provider projects the cdc + installs on a real spoke, target health goes Healthy. Closes the "full reconcile across two clusters unproven" gap cheaply.
- **Phase 1 — umbrella PoC (1–2 wks):** model the installer umbrella as a remote-targeted CD with bootstrap values; assemble the broad bootstrap RBAC bundle for the spoke; run it; verify the spoke **self-bootstraps a full Krateo** and the hub CR reflects readiness. Iterate on the recursion/CRD-cache footguns.
- **Phase 2 — first-class fleet (follow-on):** promote `KubernetesTarget` to a registered spoke with status + reachability reconciler; add the `RemoteInstall` intent object; extend creds to TokenRequest/cloud-identity forms (design §3.3/§3.6, deferred). Optionally surface a hub UI/agent flow ("install Krateo on spoke X").

## Appendix — grounding
- core-provider: `apis/compositiondefinitions/v1alpha1/types.go:94-217,251-270`; `internal/tools/clusterkube/clusterkube.go:40-107`; `internal/controllers/compositiondefinitions/compositiondefinitions.go:147,281-358,699-719`; `internal/tools/deploy/deploy.go:450-465`; `main.go:129-137`; `docs/design/multicluster-compositions.md`; `docs/how-to/remote-target-rbac.yaml`.
- cdc: `main.go:177-187,303,312,322`; `internal/composition/composition.go:71,157,203,287,474,510-528,615,729,755`; `internal/chartinspector/chartinspector.go:55,92-162`; plumbing `helm/v3/client.go:35-76,173-183,272-299`, `client_getter.go:62-104`, `helm/helm.go:47`.
