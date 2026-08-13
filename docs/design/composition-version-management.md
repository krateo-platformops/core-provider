---
type: Decision
title: "Composition version management & upgrade"
description: How one CompositionDefinition maps to a multi-version CRD and per-version controllers, how instances migrate on version bumps, and the upgradePolicy contract.
resource: compositiondefinitions.core.krateo.io
tags: [design, versioning, migration]
status: implemented
timestamp: 2026-08-07T00:00:00Z
---

> **Status (re-verified 2026-08-07): implemented**, including the later
> `spec.upgradePolicy` (`Automatic`/`Manual`/`Paused` + the
> `krateo.io/upgrade-to-version` annotation gate, `MigrationApproved` in
> `types.go`) layered on the owner-scoped migration described here.

# Composition version management & upgrade

**Status:** **implemented** — the #102 version-bump orphan fix has landed in the fork: owner-scoped
migration, write-through-the-new-endpoint, idempotent Observe-driven self-heal, reference-counted
version retirement, and served-version pruning are all in the code (see the file:line map in §5/§6).
This doc is the reference for that behavior; the per-section status notes below have been reconciled
to the code.
**Audience:** core-provider engine maintainers.
**Scope:** how one `CompositionDefinition` maps to a multi-version CRD + per-version dynamic
controllers, how composition instances are migrated when a definition's chart version changes,
and the multi-definition / multi-version cases Kubernetes lets us express.

---

## 1. The objects in play

```
CompositionDefinition (core.krateo.io/v1alpha1)        ← the "I want chart X@vN served as a CRD"
        │  spec.chart = { url|repo, version }              declaration. Reconciled by core-provider.
        │
        ▼  core-provider reconcile
generated CRD  <kind-plural>.composition.krateo.io     ← ONE CRD per (group, Kind). Carries
        │  spec.versions = [ v<chart-ver-1>, …, vacuum ]    MANY served versions + a vacuum store.
        │
        ▼  per served version
dynamic controller  <plural>-v<ver>-controller        ← composition-dynamic-controller (cdc),
        │  args: -group -version=v<ver> -resource          ONE Deployment per (CRD, version).
        │                                                   Watches instances LABEL-scoped to its
        ▼                                                   version (NOT by served apiVersion).
composition instances  (e.g. Installer/installer)      ← the user-facing CRs. Each is owned by
           labels:                                          exactly one CompositionDefinition and
             krateo.io/composition-version=v<ver>           reconciled by exactly one cdc.
             krateo.io/composition-definition-name=<cd>
             krateo.io/composition-definition-namespace=…
```

### Chart version → served CRD version

A chart version string becomes a CRD version by replacing `.` with `-` and prefixing `v`:
`0.2.128` → `v0-2-128`. (`internal/tools/crd`, `inst.apiVersion` on the installer side.)

---

## 2. The multi-version CRD model

Kubernetes lets a single CRD serve multiple versions. core-provider leans on this hard, with
three deliberate choices (`internal/tools/crd/crd.go`, `internal/tools/crd/generation`):

1. **Append, don't replace.** When a definition's chart version changes, the new version is
   **appended** to `spec.versions` (`generation.AppendVersion`) and marked
   `served: true, storage: false` (`generation.SetServedStorage(crd, newVer, true, false)`).
   Previously-served versions stay served. The CRD accumulates versions.

2. **A single permanent storage version: `vacuum`.** `generation.AppendVersion` injects/keeps a
   `vacuum` version with `served: false, storage: true` and
   `x-kubernetes-preserve-unknown-fields: true`. All instances are *stored* as `vacuum`, which
   losslessly holds any of the heterogeneous per-version schemas. Per-version served schemas are
   just views.

3. **`conversion: None`** (`setNoneConversion`). There is **no** conversion webhook. With `None`,
   the apiserver only relabels `apiVersion` between versions; it does not transform the body. This
   is safe here because the served versions are schema-passthrough and `vacuum` is the lossless
   store.

### Consequence: the apiVersion is *not* a reliable owner key

Because instances are stored as `vacuum` and `None`-converted on read, the served `apiVersion` of
an instance is not a stable identifier of "which version/controller owns this". So ownership is
carried by a **label**, not by apiVersion:

```
krateo.io/composition-version = <the served version the instance was last written through>
```

This label is stamped **in the apiserver** by a `MutatingAdmissionPolicy`
(`internal/tools/policy/policy.go`; shipped by the management chart as
`<release>-compositions-version-policy` + its binding). Its CEL mutation is, in effect:

```
labels["krateo.io/composition-version"] = request.requestKind.version
```

i.e. **the served version of the write endpoint the request came through.** Write an instance
through `…/v0-2-128/…` → it gets labelled `v0-2-128`. This is the single most important fact for
everything below.

> Requires the GA `MutatingAdmissionPolicy` API (`admissionregistration.k8s.io/v1`, Kubernetes
> ≥ 1.36). On older clusters the label is not stamped and the model degrades; the migration code
> also sets the label explicitly as a fallback.

### Per-version, label-scoped controllers

For each served version, core-provider deploys a cdc Deployment (`internal/tools/deploy`,
`deploy.Deploy`) named `<plural>-v<ver>-controller`, started with `-version=v<ver>`. cdc builds its
informer's **label selector** from that arg (`cdc main.go`):

```
krateo.io/composition-version == v<ver>
```

So `installers-v0-2-128-controller` reconciles only instances **labelled** `v0-2-128` — regardless
of what served apiVersion they're read as. The label is the contract between the stamping policy
and the controller's watch.

---

## 3. Two CompositionDefinitions, same chart, different versions

Kubernetes allows multiple versions in one CRD, and nothing stops **two different
CompositionDefinitions from pointing at the same chart (same Kind) at two different versions**:

```
CompositionDefinition "foo-125"  spec.chart = mychart@0.2.125  ─┐
CompositionDefinition "foo-128"  spec.chart = mychart@0.2.128  ─┤
                                                                 ▼
        ONE shared CRD  foos.composition.krateo.io
          spec.versions = [ v0-2-125 (served), v0-2-128 (served), vacuum (storage) ]
                                                                 │
        ┌────────────────────────────────────────────┬──────────┘
        ▼                                              ▼
  foos-v0-2-125-controller                       foos-v0-2-128-controller
   (watches composition-version=v0-2-125)         (watches composition-version=v0-2-128)
```

Key properties of this case:

- **The CRD is shared.** Both definitions resolve to the same `(group, Kind)` → same CRD. Versions
  from both accumulate in `spec.versions`.
- **A per-version controller is shared across definitions.** `foos-v0-2-125-controller` reconciles
  **every** instance labelled `v0-2-125`, whether it belongs to `foo-125` or to a `foo-128`
  instance that has not yet been migrated. There is one controller per `(CRD, version)`, not per
  `(definition, version)`.
- **Ownership is per-definition, by label.** Each instance also carries
  `krateo.io/composition-definition-name` / `-namespace` / `-group` / `-resource` / `-version`
  identifying the *one* CompositionDefinition that owns it. So `foo-125`'s instances and `foo-128`'s
  instances are distinguishable even though they may transiently share a `composition-version`.
- **A served version is reference-counted across definitions.** `v0-2-125` is "live" as long as
  *any* CompositionDefinition references it. It must not be retired (controller undeployed, version
  un-served) just because *one* definition moved off it.

These two properties — *shared controller* and *per-definition ownership* — are what any
migration/retirement logic must respect.

---

## 4. The CompositionDefinition reconcile loop

core-provider uses the managed-reconciler triad (`internal/controllers/compositiondefinitions`):

- **Observe** — compute the desired CRD + cdc from `spec.chart`, hash it, compare to
  `status.Digest`. Returns `ResourceUpToDate=false` to trigger Create/Update when anything drifts.
- **Create** — first install of a version: generate CRD, deploy cdc, record status.
- **Update** — a version changed: append the new CRD version, deploy the new cdc, **migrate
  instances**, retire superseded versions, refresh status.

`status.Managed.VersionInfo` records the versions this definition has served over time
(`{Version, Served, Stored, Chart}`), plus `status.apiVersion` = the most recently reconciled
served version.

---

## 5. Version upgrade flow (one definition, `vA → vB`)

When `foo`'s `spec.chart.version` goes `0.2.125 → 0.2.128`:

1. **Observe** sees the rendered/deployed digest changed → `ResourceUpToDate=false`.
2. **Update**:
   1. Append `v0-2-128` to the CRD, set it `served:true, storage:false`; `vacuum` stays storage.
   2. `deploy.Deploy` the `foos-v0-2-128-controller` (label-scoped to `v0-2-128`).
   3. **Migrate this definition's instances** from `v0-2-125` → `v0-2-128`
      (`getters.UpdateCompositionsVersion`): re-stamp `composition-version` on the instances this
      definition owns.
   4. **Retire** `v0-2-125` (undeploy `foos-v0-2-125-controller`) — *only when no definition still
      references it*.
   5. `RefreshCompositionDefinitionStatus` advances `status.apiVersion` to `v0-2-128`.

The migration is what hands the instance from the old label-scoped controller to the new one.
Until an instance's `composition-version` label flips to `v0-2-128`, the **new** controller does
not select it (and if the **old** controller has been retired, nobody does → orphan).

### How migration must write (the subtle part)

The `composition-version` policy stamps **the write endpoint's served version**. Therefore the
re-stamp must be performed **through the target (`v0-2-128`) endpoint**:

```
list  via the v0-2-128 endpoint, selecting composition-version == v0-2-125   (find stragglers)
write via the v0-2-128 endpoint, setting composition-version  = v0-2-128     (policy agrees: stamps v0-2-128)
```

Writing the relabel through the **old** (`v0-2-125`) endpoint is self-defeating: the policy
re-stamps `v0-2-125` on the very same request, silently undoing the relabel. (This was the root of
#102 — see §6.)

### Multi-definition correctness (the part §3 forces)

In the shared-CRD case the migration **must be scoped to the owning definition** and retirement
**must be reference-counted**:

- **Migrate only own instances.** Select
  `composition-version == vA AND composition-definition-name == <this CD> AND
  composition-definition-namespace == <this CD ns>`. Otherwise a bump of `foo-128` would steal
  `foo-125`'s legitimately-`v0-2-125` instances.
- **Retire vA only at refcount 0.** Undeploy `foos-vA-controller` / drop the served `vA` version
  only when **no** CompositionDefinition (across all of them) still references `vA`. A shared
  controller is load-bearing for every definition still on that version.

> **Status: all landed.** Write-through-the-new-endpoint + idempotent Observe-driven re-drive
> (`getters.UpdateCompositionsVersion`, `compositiondefinitions.go:1003-1019`), **definition-name
> scoping** (`getters.GetOwnedCompositionsByVersionLabel` / `compositionSelector`), and
> **reference-counted retirement + served-version pruning**
> (`compositiondefinitions.go:versionReferencedByAnotherDefinition:375`,
> `prunableServedVersions:401`, `pruneStaleServedVersions:437`) are all implemented.

---

## 6. The bug this design fixes (#102)

**Symptom.** After an installer umbrella bump `0.2.125 → 0.2.128`, the live `installer` instance
stayed labelled `composition-version=v0-2-125`, while only `installers-v0-2-128-controller`
remained (the `v0-2-125` controller had been retired). Nothing reconciled the umbrella → it froze
at its handoff state (one cluster mid-rollout never emitted its workload chain; another sat
`Ready=False` on a dying-controller error). Confirmed on k8s 1.36, core-provider 2.1.0, with the
`compositions-version-policy` present and active.

**Root cause — two compounding defects, both in core-provider:**

1. **Migration wrote through the *old* endpoint.** `UpdateCompositionsVersion` listed *and*
   updated via the old GVR. The active policy then stamped `composition-version = v0-2-125` (the
   old endpoint's version) right back onto the instance, clobbering the intended relabel. Net: the
   label never moved.
2. **Migration was fire-once.** It ran only in the `Update` path, treated "0 instances found" as
   success, and `RefreshCompositionDefinitionStatus` advanced `status.apiVersion` unconditionally.
   Once advanced, `Observe` reported "up to date" forever and `Update` (hence migration) never ran
   again — so any instance left on the old label was stranded permanently.

**Fix.**

- **Write through the new endpoint** (§5 "How migration must write"): list via the current served
  GVR selecting the *old* version label, and update via the current served GVR — so the policy
  stamps the new version and agrees with the explicit relabel. (Also sets the label explicitly to
  cover policy-less clusters.)
- **Make it idempotent and self-healing**: `Observe` re-drives migration. While any instance still
  carries a previous served version's label, `Observe` returns `ResourceUpToDate=false`, so
  `Update` re-runs the re-stamp until none remain. Stragglers from a racey transition — or an
  instance re-written through an old endpoint after status advanced — are reconciled instead of
  orphaned.
- **(For §3 — done)** the migration is scoped to the owning definition and version retirement is
  reference-counted (`versionReferencedByAnotherDefinition` / `prunableServedVersions`).

cdc needs no change — it correctly reconciles whatever the `composition-version` label selects; the
defect and the fix are entirely in core-provider's definition reconcile.

---

## 7. Invariants & open questions

**Invariants the design should preserve**

- Every composition instance is reconciled by exactly one cdc at all times — there is never a
  window where its `composition-version` label points at a retired controller.
- `composition-version` always equals a *currently served* version of the CRD.
- A served version + its controller exist as long as ≥ 1 CompositionDefinition references it.
- Migration only ever moves instances **within one owning definition's lineage**.

**Resolved**

1. **Retirement refcount source of truth → listing.** "Is `vA` still referenced?" is computed by
   listing CompositionDefinitions resolving to this `(group, Kind)` and checking their current
   versions — simpler and stateless. Implemented: `versionReferencedByAnotherDefinition`.
3. **vacuum & served-version pruning → implemented.** Refcount-0 served versions whose label no
   instance carries are removed from `spec.versions` (`prunableServedVersions` / `pruneStaleServedVersions`
   / `crdutils.RemoveStaleVersions`), bounding the straggler scan and CRD size.
5. **chart/binary policy-name skew → reconciled.** Both the chart (management cluster) and the
   binary's remote projection now use the fixed singleton name **`krateo-composition-version`**.

**Open**

2. **Down-version / rollback.** `0.2.128 → 0.2.125`: `v0-2-125` is already served; migration is
   symmetric (re-stamp through the `v0-2-125` endpoint). The code path exists; **add an explicit
   test** that `AppendVersion` is a no-op when the version already exists and storage stays `vacuum`.
4. **Policy absence fallback.** On clusters without the `MutatingAdmissionPolicy`, the explicit
   label-set in the migration is the only stamping path (it exists as a fallback). Remaining work is
   to **document** this as a supported-but-degraded mode.
