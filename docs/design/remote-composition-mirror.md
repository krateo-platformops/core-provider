---
type: Decision
title: "Design: remote composition mirror — retire RemoteInstall"
description: Make the spoke instance a first-class hub Composition mirrored onto the spoke; collapse RemoteInstall into CompositionDefinition + Composition.
resource: compositiondefinitions.core.krateo.io
tags: [design, multicluster, mirror]
status: implemented
timestamp: 2026-08-07T00:00:00Z
---

> **Status (re-verified 2026-08-07): implemented.** The mirror shipped as the
> `compositionmirror` controller (#49–#55: spec down / status up, per-instance
> fan-out via `krateo.io/target`, dynamic per-Kind watch, ordered cross-cluster
> teardown) and `RemoteInstall` was retired in #56
> ([migration how-to](../how-to/migrate-off-remoteinstall.md)).

# Design: Remote composition mirror — retire `RemoteInstall`, make the spoke instance a first-class hub `Composition`

> Status: **Draft for discussion** · Audience: core-provider maintainers · Scope: replace the
> `RemoteInstall` kind (and the interim `CompositionDefinition.spec.instance` idea) with a
> first-class **hub `Composition`** that core-provider **mirrors onto the spoke**, layered on the
> shipped controller-projection model. Written pre-migration: the forks it references are now
> the canonical `krateo-platformops` repos (the historical upstream org is dead).
>
> Goal: make a remote composition **look and behave exactly like a local one** — you author a normal
> `Composition` CR on the hub; remoteness is a property of its *definition*, not of the instance —
> and in doing so collapse `RemoteInstall` back into `CompositionDefinition` + `Composition`.
>
> Supersedes: the `spec.instance`-blob fold (this doc's §2 explains why). Builds on:
> [`multicluster-compositions.md`](./multicluster-compositions.md) (projection, shipped),
> [`remote-installer-proposal.md`](./remote-installer-proposal.md) (`RemoteInstall`),
> [`remote-projection-self-seeding.md`](./remote-projection-self-seeding.md).

---

## 0. TL;DR

1. **The observation that started this.** The `Values` in `RemoteInstall` (and the `spec.instance`
   I floated to fold it into `CompositionDefinition`) **are the chart values, and the chart values
   *are* the composition's domain.** So that blob is a `Composition` in disguise. Burying it inside
   another object breaks the model's own type/instance separation.

2. **The model.** Keep the shipped split: `CompositionDefinition` = the **type** (chart) + *where its
   instances are realized* (`spec.deploy.targetRef`). Make the instance a **first-class hub
   `Composition`** (pure values, the desired state) that core-provider **reflects onto the spoke**,
   where the projected cdc renders it. Status flows back. Remoteness is invisible to the author.

3. **What changes.** (a) For a remote `CompositionDefinition`, generate the composition CRD **on the
   hub too**, so the desired `Composition` can be authored + validated there. (b) Add a hub-side
   **reflector** that mirrors hub-`Composition` `spec` → spoke and spoke `status` → hub. (c) A
   finalizer on the hub `Composition` deletes the spoke mirror on teardown (cross-cluster GC).

4. **What it retires.** `RemoteInstall` and the `spec.instance` blob. A `RemoteInstall` becomes
   exactly *a remote-targeted `CompositionDefinition` + a normal `Composition`.*

5. **The honest cost.** This adds a **hub-side desired-state CR + a reflector** on top of pure
   projection. Rendering stays spoke-local (projection unchanged), but the source of truth now lives
   on the hub. Two `Composition` copies exist (hub desired, spoke realized). That is the price of the
   transparency in (2); §5 weighs it against the alternatives.

---

## 1. Grounding — where we are (verified)

**Projection is shipped** (`multicluster-compositions.md`, decision locked). A
`CompositionDefinition.spec.deploy.targetRef` (`apis/compositiondefinitions/v1alpha1/types.go`,
`DeploymentTarget{ TargetRef *TargetReference }`; omitted ⇒ local) points at a cluster-scoped
`KubernetesTarget` (`spec.kubeconfigRef` → a spoke-kubeconfig Secret). On a remote CD,
`Connect()` (`internal/controllers/compositiondefinitions/compositiondefinitions.go:281-317`) swaps
the provisioning clients (`kube`/`dynamic`/`client`) to the **spoke** while keeping `mgmt` = hub for
the CR + status; `deploy.Deploy` then applies the **cdc Deployment + ConfigMap + Service + generated
CRD + SA/RBAC onto the spoke**. The cdc runs *on the spoke*, single-cluster by construction, using
the spoke's own ServiceAccount. The hub credential is a bootstrap-time concern only.

**`RemoteInstall` sits on top of that** (`remote-installer-proposal.md`,
`internal/controllers/remoteinstalls/remoteinstalls.go`). `RemoteInstallSpec{ TargetRef, Chart,
Values }`; the controller `Owns` a remote-targeted `CompositionDefinition`, and once that CD is
`Ready` it calls `applyInstance` to create/update the composition **instance on the spoke** from
`spec.Values`. Deleting the `RemoteInstall` GC's the CD (ownerRef), whose delete tears the spoke
down.

**Consequence today:** for a remote CD, the generated composition CRD exists **only on the spoke**
(the `Connect` swap). There is therefore **no hub CR to author** — which is precisely *why*
`RemoteInstall` has to carry the values as an opaque `RawExtension`, and why the interim
`spec.instance` idea did too.

---

## 2. Why the instance is a `Composition`, not a field

`RemoteInstall.spec.values` and the proposed `CompositionDefinition.spec.instance` are the **same
data**: the chart values, i.e. the exact contents of a `Composition.spec`. The model already has a
first-class object for that data — the generated `<Kind>.composition.krateo.io` CR. Encoding it as a
sub-field of another kind means:

- **It breaks type/instance separation.** `CompositionDefinition` is the *type*; a `Composition` is
  the *instance*. A remote install is one instance of a type — so it belongs in a `Composition`, not
  smeared into the definition.
- **`applyInstance` is a push, not a reconcile.** `remoteinstalls` `Setup` is
  `For(RemoteInstall).Owns(CompositionDefinition)` — it **never watches the instance**. It writes the
  spoke instance on `RemoteInstall`/CD events, but drift on the instance itself is invisible to it
  (only the spoke cdc heals the *release*, nobody heals *instance-spec vs desired*). A first-class
  hub `Composition` is a *watched CR* — a real loop.
- **It needs a bespoke cross-cluster GC.** ownerRefs don't cross the hub→spoke boundary, so
  `RemoteInstall` re-implements teardown. A hub `Composition` + finalizer gives the same guarantee in
  the model's own idiom.

So the refactor isn't "move a field" — it's "recognize the field was always a `Composition`, and
give it back its identity."

---

## 3. The model — a hub `Composition`, reflected to the spoke

Three layers, each keeping its natural job:

| layer | role | lives on |
|---|---|---|
| `CompositionDefinition` | the **type** (chart) + *where instances go* (`deploy.targetRef`) | hub |
| `Composition` (e.g. `Portal/portal`) | the **instance** = pure chart values, **desired state** | **hub** |
| reflection | mirror hub `spec` → spoke; read spoke `status` → hub | core-provider (hub) |
| projected `Composition` + Helm release | **realized state** (rendered by the spoke cdc) | spoke |

**Author identically to local.** You `kubectl apply` a `Portal` on the hub. Because its
`CompositionDefinition` carries `deploy.targetRef`, core-provider treats that hub CR as *desired
state for a spoke* and reflects it down; the projected spoke cdc renders the release; the spoke
status (including the charts' existing `spokeReadback*` endpoints) is read back into the hub CR's
status. The author never sees a second kind and never touches the spoke.

**Pure domain.** `Composition.spec` stays exactly the chart values — the target is on the *type*
(`CompositionDefinition.deploy`), never in the instance's `spec`. That is the direct expression of
"chart values *are* the composition domain."

The one required change to make a hub CR *authorable*: for a remote CD, **generate the composition
CRD on the hub as well as the spoke** (today it's spoke-only, §1). The hub copy is the schema the
desired `Composition` validates against; the spoke copy is what the spoke cdc watches.

---

## 4. API & reconcile shape

**API: nothing new on `CompositionDefinition`.** No `spec.instance`. The existing
`spec.deploy.targetRef` already expresses remoteness; we just let it drive *instance* reflection, not
only *cdc* projection.

**CRD generation (hub + spoke for remote).** In the remote branch, apply the generated CRD to the
hub (`mgmt`) in addition to the spoke (the `Connect`-swapped client). Open sub-decision (§5): whether
the hub copy is *served* (a real API you can `get`/`list`) or authoring-only.

**Reflector (new hub controller), per remote composition Kind:**
- **Watch** the hub `Composition` (drift-healed — the thing `RemoteInstall` couldn't do). The Kind is
  runtime-generated, so the watch is registered dynamically when the CRD is created — the one genuinely
  non-trivial mechanic; §7.
- **Down:** create-or-update the spoke `Composition` (`clusterkube.Remote(deploy.targetRef)` — the
  same client path `applyInstance` uses today) with `spec` = hub `spec`. The spoke cdc renders it
  (projection, unchanged).
- **Back:** read the spoke `Composition.status` → hub `Composition.status` (Ready/release-info),
  reusing the target-health + readback plumbing.
- **Teardown:** finalizer on the hub `Composition`; on delete, remove the spoke mirror first (whose
  own delete drops the release via the spoke cdc), then release the finalizer.

**Reuse, don't rebuild.** This is `remoteinstalls.applyInstance` promoted to a watched loop over a
real CR, plus status readback. `Connect`, `clusterkube.Remote`, `KubernetesTarget`, target-health,
and the spoke cdc are all unchanged.

**Local is a no-op.** With no `deploy.targetRef`, there is no reflector and no mirror — the hub
`Composition` is rendered locally by the local cdc, exactly as today. One code path, two placements.

---

## 5. Decisions / forks

1. **Where "which spoke" lives — per-CD vs per-instance.** Per-CD `deploy.targetRef` (recommended)
   keeps `Composition.spec` pure but sends *all* instances of a Kind to one spoke. Per-instance
   fan-out (same Portal type, different spoke per tenant) can't live in `spec` without polluting the
   values, so it is a `Composition` **annotation** (`krateo.io/target: <name>`) escape hatch.
   Default per-CD; the annotation overrides it per instance.

   **Implemented (fan-out).** The reflector resolves each hub `Composition`'s target from
   `krateo.io/target` (else the CD default), groups instances by resolved target, and reconciles each
   spoke with exactly the instances bound to it — mirror down + status back + a per-target GC (desired
   = that spoke's instances). **Retarget cleanup:** because a spoke an instance was *moved away from*
   is named by no current annotation, the reflector additionally sweeps every `KubernetesTarget` in
   the CD's namespace for orphaned mirrors (best-effort; GC is `Kind`+managed-label scoped, so it only
   ever removes this reflector's own mirrors of that Kind). So a retargeted-away orphan **is** now
   collected — at steady-state resync, and on CD teardown for every reachable spoke. The one residual
   is an orphan on a spoke that is *both* retargeted-away-from *and* permanently unreachable; a
   per-mirror finalizer would close even that (design §7).

2. **Mirror-CR vs direct-render.** *Recommended: mirror-CR* — reflect hub → a spoke `Composition`,
   let the spoke cdc render it. It reuses the entire projection stack and keeps the spoke
   self-healing. The alternative, *direct-render* (a hub reconciler renders the Helm release straight
   onto the spoke, no spoke cdc), is the **central remote-apply** model the multicluster design
   **explicitly rejected**; do not reopen it here.

3. **Hub CRD: served or authoring-only.** Served is the honest choice (you can `get/list` your
   desired Portals on the hub, status included) but means two live CRD versions to keep in lockstep
   across clusters (version skew risk — the same class of bug this codebase already fights, cf.
   `composition-version-management.md`). Authoring-only (validation schema, not a served API) is
   lighter but odd. Lean served; make hub↔spoke CRD-version reconciliation explicit.

4. **The tension to state plainly.** Pure projection wanted the *entire* instance lifecycle
   spoke-local (hub = bootstrap only). This model deliberately **moves the desired-state CR back to
   the hub** and adds an always-on hub reflector per remote instance. Rendering stays spoke-local;
   only intent + status cross the boundary. That is a real re-weighting of the projection decision —
   accepted here **for the transparency win** (same UX local/remote, a real reconcile loop, one fewer
   kind), but it should be an explicit, reviewed change to that decision, not a silent drift.

---

## 6. Migration from `RemoteInstall`

Mechanical, with a deprecation window:

- **Equivalence:** `RemoteInstall{ targetRef, chart, values }` ≡ `CompositionDefinition{ chart,
  deploy.targetRef } + Composition{ spec: values }`.
- **Shim (one release):** `remoteinstalls` stops calling `applyInstance` and instead reconciles *two*
  hub objects — the remote CD (as today) **and** a hub `Composition` from `spec.values` — mirroring
  their status up. The kind is marked deprecated; behavior is identical, so no data migration.
- **Remove (next release):** drop the `RemoteInstall` kind + controller; provide a one-shot converter
  (`RemoteInstall` → CD + `Composition`) for any that remain.

---

## 7. Open questions / risks

- **Dynamic watch of a runtime-generated Kind.** The reflector must start/stop watches as composition
  CRDs come and go on the hub. Feasible (informer per generated GVR, torn down on CD delete) but the
  fiddliest part; a v1 could fall back to resync-driven reflection (RemoteInstall parity) and add the
  watch as a fast-follow.
- **Spec ownership / drift on the mirror.** The hub reflector owns the spoke `Composition.spec`; an
  edit made directly on the spoke should be reverted from the hub. Define the conflict rule (hub
  wins) and whether the spoke mirror is labeled/annotated as reflector-managed to prevent foot-guns.
- **CRD version skew hub↔spoke.** If the hub serves the composition CRD, its served version(s) must
  track the spoke's as the chart version bumps. Reuse the version-management machinery; validate the
  skew case explicitly (it is exactly the class of wedge already seen in production).
- **Status fidelity.** How much of the spoke `Composition.status` (conditions, digest, version info,
  readback endpoints) is meaningful to project onto the hub, and how stale is acceptable.
- **Blast radius.** The reflector is a new always-on hub controller touching every remote
  composition. Size its concurrency + failure isolation so a single unreachable spoke can't stall the
  others (target-health already models reachability).

---

## Appendix — grounding (verified touchpoints)

- `RemoteInstallSpec{ TargetRef, Chart, Values }`, `DeploymentTarget{ TargetRef }`,
  `TargetReference{ Name }`, `KubernetesTargetSpec{ KubeconfigRef }` — `apis/compositiondefinitions/v1alpha1/types.go`.
- `remoteinstalls` controller: `Setup` = `For(RemoteInstall).Owns(CompositionDefinition)`;
  `applyInstance` = Get→Create-if-absent / else Update `spec`, **no instance watch** —
  `internal/controllers/remoteinstalls/remoteinstalls.go`.
- CD reconciler `Observe`/`Create`/`Update`/`Delete` (managed) —
  `internal/controllers/compositiondefinitions/compositiondefinitions.go:591/857/1027/1239`; the
  remote client swap at `~281-317`.
- Projection stack (`Connect`, `clusterkube.Remote`, `deploy.Deploy` onto the spoke, target health) —
  `remote-installer-proposal.md` §1, `multicluster-compositions.md` §3.
