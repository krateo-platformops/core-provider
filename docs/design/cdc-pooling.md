---
type: Decision
title: "Design: controller pooling for the composition-dynamic-controller"
description: Let a CompositionDefinition declare whether its Kind gets a dedicated CDC or joins a shared pool — and why the pool is named rather than a boolean.
resource: ghcr.io/krateo-platformops/composition-dynamic-controller
tags: [design, scaling, cdc, rbac]
status: proposed
timestamp: 2026-10-22T00:00:00Z
---

# Design: controller pooling for the CDC (vertical consolidation)

> Status: **Proposed — no decision taken** · Audience: core-provider +
> composition-dynamic-controller + unstructured-runtime maintainers · Scope: let several composition
> Kinds share one CDC process, declared per CompositionDefinition, to stop paying one pod per Kind.
>
> This is the **mirror** of [[cdc-sharding.md]]. That doc splits **one Kind across many pods**
> (horizontal, for a hot Kind). This one collapses **many Kinds into one pod** (vertical, for a wide
> fleet). They are orthogonal, composable, and share one API surface (`spec.controller`). Read
> §3.2 of the sharding doc first: its three CDC facts are the constraints both designs obey.

---

> **Update (2026-10-22), before reading the rest:** the design's own step 0 — profile the floor —
> was run, and **83% of an idle controller's heap is a cluster-wide CRD cache that nothing reads**
> (core-provider#182, §1.5). The floor this proposal is built on is not irreducible. Fixing that is
> local and risk-free and roughly **halves** what pooling would recover, so #182 lands first and
> the fleet is re-measured before the pool lifecycle (step 5) is funded. The rest of the design
> stands as written; its headline saving does not.

## 0. TL;DR

1. **The shape today.** core-provider provisions **one CDC Deployment per Kind × served version**,
   each single-replica, each watching one GVR narrowed by a `krateo.io/composition-version`
   equality selector. The Deployment, ConfigMap, ServiceAccount, baseline ClusterRole(+Binding) and
   Role(+Binding) are all rendered per (resource, version).

2. **The cost is per-process, and the fleet is paying it for almost no work.** Measured on
   krateo-057 (2026-10-22, §1): **66 CDC Deployments serving 83 composition instances** — 64 Kinds
   have exactly **one** instance. The settled per-pod cost is **~88 MiB**; the marginal cost of an
   instance *inside* an already-running process is **~4–6 MiB**. So a *process* costs roughly what
   **15–20 instances** cost, and ~95% of the fleet's CDC memory is process floor rather than work.

   **Put sharply: a controller with nothing to do costs the same as one doing real work.** Four
   CRD-only controllers run at **1 millicore** while holding 136–213 MiB each, and `portals` is 1m
   CPU for 153 MiB. The idle cohort — page/portal and CRD-only Kinds — is **26 pods and 3 968 MiB,
   ~38% of all controller memory**, reconciling compositions that are static once installed (§1.3).

3. **The proposal.** Add `spec.controller.pool` — an optional **name**. Absent means today's
   dedicated Deployment, byte-identical (the default never changes). Set, the Kind is served by a
   shared pool CDC that hosts **one controller per member GVR inside one process**.

4. **Why a name and not `shared: true`.** The pool's ServiceAccount accumulates the **union of its
   members' derived RBAC**. A boolean hides that choice; a name forces an operator to draw the
   boundary. Measured (§4.4): a composition needs a **median of 6** (group/resource) pairs, the
   whole-fleet union is **133**, and one existing composition already holds **67**. Pool membership
   *is* a blast-radius decision, so the API should make the operator spell it.

5. **Pooling does not touch the invariant sharding risks.** Sharding's danger is two reconcilers on
   one Helm release — destructive, because the handler rolls back a release it believes was orphaned
   ([[cdc-sharding.md]] §3.2). Pooling keeps **exactly one reconciler per instance** in steady
   state. The hard parts move elsewhere: **membership without restarts**, **isolation**, and a
   **join window** that does inherit the two-writer hazard (§6.1).

6. **The blocking code change.** The CDC binary is hard-wired to one GVR: `main.go` builds a single
   controller from one `schema.GroupVersionResource` and blocks on `Run`. A pooled CDC needs a
   runtime manager that starts and stops per-GVR controllers, plus per-GVR versions of five things
   that are process-wide env today (§2).

---

## 1. Motivation — the measurement, not the aesthetic

All figures from **krateo-057**, 2026-10-22, 4 nodes × (8 CPU / 32 GiB).

**A caveat that is itself a finding.** The cluster was mid-churn when first sampled: 24 of 101
controller pods were under 8 minutes old and composition instances were being recreated (one portal
instance was 82 s old). A first pass read "18 CDCs with zero instances" — those Kinds' instances
simply had not been recreated yet. **Cohorting by pod age is mandatory here**, and it reversed the
conclusion (see the floor/work split below).

### 1.1 Pods versus work

| | count |
|---|---|
| CDC Deployments (`-group=composition.krateo.io`) | **66** |
| composition instances, cluster-wide | **83** |
| Kinds with exactly 1 instance | **64** |
| busiest Kind (`BuilderPublish`) | **17** instances, in **one** pod |

### 1.2 Where the memory goes

| cohort | pods | total | mean | max |
|---|---|---|---|---|
| **settled** (14–15 h old) | 77 | **6 804 MiB** | **88.4 MiB** | 232 MiB |
| **fresh** (< 8 min, doing install/upgrade work) | 24 | 3 479 MiB | 145.0 MiB | 324 MiB |

Two numbers do the whole argument:

- **The settled floor is ~88 MiB and is nearly independent of instance count.** The 17-instance
  `builderpublishes` pod sits at **158 MiB** — about **70 MiB above floor for 17 instances, ~4 MiB
  each**.
- **Active Helm work costs ~+60 MiB, transiently.** That is the entire gap between the settled and
  fresh cohorts: chart fetch, render and 3-way diff allocate, then GC returns it. It is *not* a
  per-instance residency cost — which is why the naive reading of the first (churning) sample,
  "an instance costs 60 MiB", was wrong, and why the sizing rule in §4.2 is about **concurrency**,
  not population.

**Reserved** (`cdc.resources`: `50m` / `128Mi`, limit `512Mi`): 66 × 128 MiB = **8.25 GiB** and
**3.3 cores** held against the scheduler for 83 instances.

### 1.3 Cost is uncorrelated with work — the pods spawned for nothing

The floor argument has a sharper form: because a controller's footprint is set by the process rather
than by what it has to do, **the clearest waste is the controllers that have nothing to do.**

The seven settled CRD-only controllers — serving charts whose content is CustomResourceDefinitions,
applied once and then static — hold **1 079 MiB between them, and four run at 1 millicore**:

| controller | CPU | memory |
|---|---|---|
| `gitprovidercrds-v0-15-6` | 1m | 136 MiB |
| `oasgenprovidercrds-v0-28-1` | 1m | 144 MiB |
| `authncrds-v0-27-3` | 1m | 147 MiB |
| `kagentcrds-v0-1-6` | 1m | 213 MiB |

`portals` is the same shape: **1m CPU for 153 MiB**. Nothing is wrong with these controllers — they
are idle because their compositions are static — and they are charged a whole process regardless.

Grouped by that shape, the idle cohort is **26 of the 101 controller pods and 3 968 MiB, about 38%
of all controller memory**: 16 page/portal pods (2 456 MiB) and 10 CRD-only pods (1 512 MiB).

**The nuance that keeps this honest.** It is not that those *Kinds* are useless. Three of the same
seven CRD-only controllers do real work (14m, 26m and 60m CPU), and a static composition still needs
a watch so drift and version bumps are caught — deleting the controller is not the answer. What is
wrong is that **the watch costs a pod.** Pooling keeps every watch and stops paying a process for
each one.

### 1.4 The scaffolding, and the watches

Per-(resource, version) objects, cluster-wide today: **434** ClusterRoles, **423**
ClusterRoleBindings, **146** ServiceAccounts, **291** ConfigMaps, **47** Services — and **156 pods
in `krateo-system`** against a 110-pods-per-node default.

Each CDC also runs a **cluster-wide CustomResourceDefinition LIST/WATCH**
(`dynamic.WatchCRDsAndInvalidate`, main.go) to invalidate its discovery cache. That is **66
cluster-wide CRD watches** for one cluster's worth of CRDs — a cost that collapses to one per pool.

### 1.5 The floor is not irreducible — and that weakens this proposal

Step 0 of §8 was "profile the floor before building anything". It was run, and it found that most
of the floor is a single wasted cache, which materially reduces what pooling is worth.

Heap profile of `authncrds-v0-27-3-controller` (idle: 1m CPU, 16 h uptime, one instance), taken
through its own `localhost:6060`:

```
Type: inuse_space — 72.33 MiB total, 63 goroutines (no leak)

watch.(*StreamWatcher).receive → rest/watch.(*Decoder).Decode
  → unstructuredJSONScheme.Decode → json.(*decodeState).objectInterface
                                                    60.23 MiB   83.28%
unstructured-runtime/pkg/controller.(*Controller).handleObserve
                                                     4.54 MiB    6.27%
```

`WatchCRDsAndInvalidate` builds a **dynamic** informer over `customresourcedefinitions`, which
caches every object in full as `*unstructured.Unstructured`. Its handler reads none of them —
`AddFunc`/`DeleteFunc` ignore the object, `UpdateFunc` touches only `GetResourceVersion()`. The
cluster holds **540 CRDs, 38 MiB of JSON**, which inflates to ~60 MiB of decoded maps. Every CDC
does this independently: **~3.6 GiB fleet-wide to be told that a CRD changed** (core-provider#182).

The fix is a metadata-only informer, is local to one function, and adds no failure domain.

**What that does to the case for pooling.** With the floor at ~45 MiB RSS instead of ~147, the
fleet's recoverable memory falls from roughly 5.5 GiB to roughly 2.8 GiB. Pooling is still worth
something — it collapses 66 CRD watches to one per pool, removes ~1 300 RBAC objects and the
scaffolding, and the floor it shares is a real floor — but **the headline number this design was
built on halves**, and the honest position is that #182 lands first and the fleet is re-measured
before step 5 is funded.

Two further caveats stand regardless. Not all of the remainder is recoverable, and the fleet is
*not* idle: `builderpublishes` was measured at **1 827m CPU**. Pooling converts per-pod floor into
shared floor; it does not reduce work.

---

## 2. What is actually per-GVR — the inventory that decides feasibility

A pooled process must supply, per member, everything the dedicated pod gets from its own env and
pod spec. Verified against `main.go`, `assets/cdc/configmap.yaml`, `assets/cdc/deployment.yaml`:

| per-GVR today | lives in | after pooling |
|---|---|---|
| `-group` / `-version` / `-resource` | Deployment args | per-controller — trivial |
| `krateo.io/composition-version` equality selector | built in `main.go` from `-version` | per-controller — trivial |
| `-workers`, `-resync-interval` | args (`spec.controller`) | per-controller, but **capped pool-wide** (§4.2) |
| `resources` | container spec (`spec.controller`) | **pool-wide only** |
| `COMPOSITION_CONTROLLER_SA_NAME` / `_SA_NAMESPACE` | ConfigMap | **pool-wide — the crux** (§4.4) |
| `COMPOSITION_CONTROLLER_STATUS_DATA_TEMPLATE` | ConfigMap | read from the CompositionDefinition |
| `COMPOSITION_CONTROLLER_API_REF_NAME/_NAMESPACE/_EXTRAS` | ConfigMap | read from the definition; **identity is pool-wide** (§4.4) |
| projected SA token, audience `authn` | pod volume, rendered only when `apiRef` is set | **pool-wide** — one token, one identity |
| `DEPLOYMENT_NAME`, `OTEL_RESOURCE_ATTRIBUTES` (`krateo.io/composition-gvr`) | ConfigMap / set in `main.go` | **per-controller attributes** (§4.5) |

**The free win already in the code.** The chart is *not* process state. `archive.Dynamic` resolves
the owning CompositionDefinition **per reconcile, from the instance's own
`krateo.io/composition-definition-{name,namespace}` labels**, then reads `spec.chart.*` off it
(`internal/tools/archive/getter.go`). The `-chart` flag (`archive.Static`) is the only baked-in
path and core-provider does not use it. So the single most awkward thing to make per-member —
"which chart does this object want?" — is **already per-instance and dynamic**. Four of the five
remaining ConfigMap values are readable from the same object the getter already fetches.

That leaves exactly one irreducibly pool-wide thing: **identity** (the ServiceAccount, and the
authn token minted from it). Everything else is mechanical.

---

## 3. Design

### 3.1 API surface

```yaml
apiVersion: core.krateo.io/v1alpha1
kind: CompositionDefinition
spec:
  controller:
    pool: frontend-pages     # optional. absent ⇒ a dedicated CDC (today's behaviour)
    workers: 4               # existing
    resyncInterval: "10m"     # existing
```

`pool` is an optional `string` on the existing `ControllerConfig`, DNS-1123-label-validated by the
CRD schema (core-provider runs **no admission webhook**, so the schema is the only gate — the same
constraint that shaped `resyncInterval`'s string typing). Absent is the default and renders exactly
what it renders today, so no existing definition changes or restarts.

A boolean (`shared: true`) is rejected deliberately: see §4.4. `Dedicated` is recoverable as
"its own pool", so the named form is strictly more expressive.

### 3.2 The pool object set

Owned by core-provider, keyed by **(pool name, namespace)** instead of (resource, version):

```
cdc-pool-<name>            Deployment   (1 replica, as today — no leader election exists)
cdc-pool-<name>            ServiceAccount
cdc-pool-<name>            ClusterRole + ClusterRoleBinding   (the static 12-rule baseline)
cdc-pool-<name>            Role + RoleBinding                 (jobs + services, namespaced)
cdc-pool-<name>-configmap  ConfigMap    (pool-wide settings only)
```

Per pooled Kind, core-provider still does everything *except* render a workload: it generates and
applies the composition CRD, keeps the version policy, and owns the derived-RBAC subject. It
**ref-counts** pool membership so the pool is torn down when its last member leaves.

### 3.3 Membership discovery — the pool watches, it is not told

The pool must **not** receive its member list as env. An env list means every join or leave rewrites
the ConfigMap and **restarts the pool**, interrupting every other Kind it serves — turning a routine
"install one new blueprint" into a fleet-wide blip. That single property is what makes the naive
version of this design worse than what we have.

Instead the pool **watches `compositiondefinitions.core.krateo.io`** and derives its own membership:
for each definition whose `spec.controller.pool` equals its own name, that is `Ready`, and whose
served versions are known, it starts one controller per (GVR, served version); when a definition
stops naming it, it cancels that controller's context and stops its informer.

This needs no new RBAC: `core.krateo.io/compositiondefinitions` `get,list,watch` is **already in the
baseline ClusterRole** every CDC holds, because the archive getter already lists them.

It also inverts a responsibility worth stating plainly: **the pooled CDC becomes a small
meta-controller.** core-provider remains the only writer of CRDs, RBAC and policy; the pool decides
only which of its own informers are running.

### 3.4 Lifecycle inside the pool

```
CompositionDefinition watch
        │  spec.controller.pool == me, Ready, servedVersions=[v1-8-83, …]
        ▼
   manager.Ensure(gvr, cfg)  ──▶ goroutine: builder.Build(gvr, selector{version==v}) → Run(ctx_i)
   manager.Remove(gvr)       ──▶ cancel(ctx_i), wait for in-flight reconciles to drain
```

The drain on removal is the existing `-graceful-shutdown-timeout` semantics applied per controller
rather than per process. A member must not be cancelled mid-Helm-operation: the handler's
stuck-release recovery treats a `Pending*` release past a grace period as orphaned and rolls it
back, so an abrupt cancel manufactures exactly the state [[cdc-sharding.md]] §3.2 warns about —
this is also why the chart deliberately ships **no liveness probe** (issue #57).

---

## 4. What pooling costs — stated before it is recommended

### 4.1 Failure domain

Today a crash, OOM or wedge stalls **one** Kind. Pooled, it stalls **every member**. There is no
leader election in `unstructured-runtime` (hence `replicas` is withheld from `spec.controller`), so
the pool is a single point of failure for its members, with no standby.

Issue **#99** is the cautionary precedent: the fleet grew monotonically into its 256 MiB limit and
three controllers entered an OOMKill cycle (132 restarts on one). Root cause was a goroutine leak —
every reconcile leaked a Helm `DiskCache` cleanup goroutine. **A leak that was survivable per-Kind
becomes a pool-wide outage**, and the current `values.yaml` comment already records the matching
sizing trap: sizing a request from observed RSS *bakes the leak in*.

### 4.2 Sizing, and why `workers` must be capped pool-wide

From §1.2: floor ≈ 88 MiB; a concurrent Helm operation ≈ +60 MiB transient.

```
peak RSS(pool) ≈ 88 MiB + (concurrent Helm operations × ~60 MiB) + (instances × ~5 MiB)
```

The dangerous term is the middle one, and it is **not** bounded by the number of instances — it is
bounded by **workers summed across members**. If each member keeps its own `workers: 4`, a 10-member
pool peaks at 88 + 40×60 ≈ **2.5 GiB**. So the pool needs a **pool-wide concurrency cap**, with each
member's `workers` acting as a *share* of it rather than an independent allowance. Without that cap
pooling trades 66 predictable 88 MiB pods for a handful of unpredictable multi-gigabyte ones.

With a cap of, say, 4 concurrent operations: ≈ 88 + 240 + (20 × 5) ≈ **430 MiB** per pool. Four
pools ≈ **1.7 GiB** against today's 6.8–10 GiB — a **~5×** reduction, and ~60 fewer pods.

### 4.3 Noisy neighbours

Measured, not hypothetical: `builderpublishes` at **1 827m CPU** against a fleet mean of ~39m. A
pool containing it absorbs that spike into its members' reconcile latency, and its memory
allocations into their shared heap. Pools should group Kinds with *similar* load profiles; the hot
Kind is a natural `Dedicated`, and is also the natural candidate for the *other* design
([[cdc-sharding.md]]).

### 4.4 The RBAC union — the real reason the pool is named

The pool's ServiceAccount is the subject of every member's derived RBAC. Measured today:

| | value |
|---|---|
| derived `-krateo-rbac` ClusterRoles / Roles | 84 / 78 |
| namespaced (group/resource) pairs, **median per composition** | **6** |
| namespaced pairs, **widest single composition** (`installer-platform`) | **67** |
| namespaced pairs, **whole-fleet union** | **133** |
| cluster-scoped pairs, whole-fleet union | 10 |

A single global pool would hold **133** where the median Kind needs **6** — a ~22× widening. That is
the cost that must not be hidden behind a boolean.

Three facts make it smaller than it first looks, and they are the reason this is a tunable choice
rather than a disqualifier:

- **63 of the 133 pairs are `composition.krateo.io/*`**, and the static baseline ClusterRole already
  grants `composition.krateo.io` `resources: ["*"] verbs: ["*"]` to **every CDC in the cluster**.
  The per-Kind isolation people assume exists at the baseline layer **does not** — it exists only in
  the derived layer. Pooling widens the derived union; it changes the baseline not at all.
- **Of the 10 cluster-scoped pairs, 4 are already in the baseline** (`customresourcedefinitions`,
  `clusterroles`, `clusterrolebindings`, `namespaces`). The genuinely new cluster-scoped surface is
  6 pairs, concentrated in 3 compositions (`nightly-review`, `gcp-configconnector`,
  `agentgateway-controller`) — all obvious `Dedicated` candidates.
- **One SA already holds 67 of the 133.** The widest blast radius in the fleet today is half the
  union, by a design nobody objected to.

So: **a pool is a trust boundary, and `pool:` is how an operator declares one.** Group the 15
page/frontend Kinds — whose derived rules are near-identical widget reads — and the union barely
moves. Put `installer-platform` in with them and it doubles. The API should make that visible at
the point of decision, which a boolean cannot.

**Derived RBAC is already additive onto one SA.** `rbacgen` names its objects
`<release>-krateo-rbac` **per composition instance**, with the CDC's own SA as subject, so an SA
serving *n* instances of one Kind already holds the union of *n* rule sets. Pooling extends an
existing union across Kinds; it introduces no new mechanism.

**Join and leave are identity rotations** — the pooled member's subject moves from
`<plural>-<version>` to `cdc-pool-<name>`. That is exactly what `deploy.ResubjectDerivedRBAC`
(core-provider#166) was built for, and it must be reused rather than reimplemented: bindings naming
a deleted ServiceAccount are **not an error**, they silently grant nothing.

### 4.5 apiRef identity

A member with `spec.apiRef` resolves a RESTAction through snowplow under an authn-issued JWT minted
from its pod's projected token. Pooled, that token is the **pool's** — so all `apiRef` members share
one authn identity, and snowplow's `/rbac` read-sets union onto it. Recommendation: **a definition
with `apiRef` set defaults to `Dedicated`** and may only join a pool that explicitly opts in, so
merging API identities is never a side effect of a memory optimisation.

### 4.6 Rollout coupling

A CDC image bump restarts every member of a pool at once, where today restarts are per-Kind and
independent. Since the cdc tag tracks the chart `appVersion`, **every** core-provider release does
this. Mitigation is sequencing (restart pools one at a time) and accepting a short, simultaneous
pause per pool — acceptable because reconciles are level-triggered, but it must be said out loud.

### 4.7 The observability trap

Today "one pod = one Kind" does the attribution work: `DEPLOYMENT_NAME` identifies metrics and
`main.go` sets `krateo.io/composition-gvr` **process-wide** in `OTEL_RESOURCE_ATTRIBUTES`, shared by
the metrics, trace and log resources. `internal/metrics` holds a **process-global singleton** keyed
by `deploymentName`.

Pooled and unchanged, all three keep working and **attribute every member's signal to the pool**.
Nothing errors; dashboards stay green; per-Kind numbers become quietly meaningless. This is the
failure mode that returns a clean, confident, wrong answer, so it is a **blocking prerequisite**:
GVR must move from a process-level resource attribute to a per-controller metric/span/log
attribute, and the metrics singleton must be keyed per member, **before** the first pool runs.

---

## 5. Code touchpoints

**composition-dynamic-controller**
- `main.go` — split into bootstrap (config, telemetry, event recorder, RESTMapper, one shared CRD
  watch) and a **manager** hosting *n* controllers. Keep the single-GVR path as the degenerate case.
- new `internal/pool` — the CompositionDefinition watch, membership reconciliation,
  `Ensure`/`Remove` with per-member contexts and drain.
- `internal/composition` — `HandlerOptions` currently carries `SaName`/`SaNamespace`/
  `StatusDataTemplate`/`APIResolver` as *process* config; these become per-member (the first two
  pool-wide, the last two resolved from the definition).
- `internal/metrics`, `telemetry` — per-controller attribution (§4.7); drop the global singleton's
  process-wide key.
- a **pool-wide concurrency limiter** shared by all members' worker pools (§4.2).

**core-provider**
- `apis/compositiondefinitions/v1alpha1` — `ControllerConfig.Pool *string` + CRD validation.
- `internal/tools/deploy` — a pool-keyed object set beside the per-GVR one; `Deploy`/`Lookup`/
  `Undeploy` branch on pool membership and ref-count the pool.
- `internal/controllers/compositiondefinitions` — the three `deploy.Deploy` sites (Create,
  Observe-lookup, Update) and three `deploy.Undeploy` sites choose dedicated vs pool; the retirement
  branch already calls `ResubjectDerivedRBAC` and must do so on pool join/leave too.
- status — surface the serving pool on the CompositionDefinition so `kubectl get` answers "which
  process runs my Kind?" without reading a Deployment.

**core-provider chart**
- `assets/cdc/` — a pool variant of the Deployment/ConfigMap/RBAC templates, keyed by pool name.

**unstructured-runtime**
- confirm `builder.Build` + `Run` are safe *n*-per-process (no package-level singletons), and that
  per-controller cancellation stops informers cleanly.

---

## 6. Migration

### 6.1 The one place pooling inherits the two-writer hazard

Moving an existing Kind into a pool means the dedicated Deployment and the pool's new controller
both select the same objects with the **same** version selector. Both re-read live and both drive
`helm upgrade`/`rollback` on the **same release** — the whole-population corruption window of
[[cdc-sharding.md]] §6.1, reached by a different route.

Steady state is safe (one reconciler per instance); **only the join/leave transition is not.**
Mandatory sequence, stop before start:

1. **Scale the dedicated Deployment to 0** and wait for its drain.
2. **Resubject** the member's derived RBAC to the pool SA (`ResubjectDerivedRBAC`).
3. **Register** the member — the pool's definition watch starts its controller.
4. **Confirm started** — the pool reports the member as serving (a status or metric the pool must
   expose; without it step 5 is a guess).
5. **Delete** the dedicated Deployment, ConfigMap, SA and RBAC.

Leave is the reverse. Step 4 is not optional: deleting the dedicated set before the pool is
demonstrably serving leaves the Kind unreconciled, and doing it in the other order leaves two
owners.

**A guard, not a sequence, is the real protection.** As in sharding, the fence must sit at reconcile
time, after the live read and before the Helm call: a CDC should no-op on an object whose owning
definition does not name *this* process (pool name, or dedicated identity). Ship that guard first —
inert until pooling exists — so a mis-sequenced migration degrades to "nobody reconciles" rather
than "two reconcilers corrupt the release".

### 6.2 Where to start

**Pick the first pool by idleness, not by chart family.** The selection criterion falls straight out
of §1.3: move the Kinds whose controllers are paying the floor and doing nothing with it. Two
cohorts qualify and they are adjacent:

| cohort | Kinds | pods | memory | why it qualifies |
|---|---|---|---|---|
| page / portal | 15 | 16 | 2 456 MiB | near-identical `widgets.templates.krateo.io/*` reads |
| CRD-only | 10 | 10 | 1 512 MiB | one `customresourcedefinitions` grant the baseline already carries |
| **together** | **25** | **26** | **3 968 MiB** | **~38% of all controller memory** |

Both cohorts hold one instance each, carry **no `apiRef` and no `statusDataTemplate`** (verified
2026-10-22, so none of §4.5's identity merging applies), and have nothing to reconcile once
installed. Their derived unions barely exceed the baseline every CDC already holds, which makes them
simultaneously the cheapest Kinds to move and the most expensive ones to leave.

One measurement caveat: the 16 page/portal pods had all restarted ~6 minutes before sampling, so
2 456 MiB is their working figure; at the settled floor the cohort is ~1.3 GiB. The CRD-only figure
is from settled pods.

Explicitly **not** first: `installer-platform` (67 pairs), `builderpublishes` (17 instances,
1 827m CPU), and anything carrying `apiRef`.

---

## 7. Open questions

- **Pool-wide concurrency.** Is the cap a hard semaphore around Helm operations, or `workers`
  rewritten as shares? A semaphore is simpler and bounds the term that actually matters (§4.2).
- **Who sizes a pool?** `spec.controller.resources` is per-definition today, but resources are
  pool-wide. Does the *pool* need its own config object once more than `resources` is pool-scoped —
  and if so, is that still `spec.controller`, or a `CDCPool` CRD?
- **Should core-provider refuse a dangerous pool?** It can compute the prospective union from
  chart-inspector before admitting a member and reject or warn when membership widens the pool past
  a threshold. Attractive — and it makes the trust boundary enforced rather than advisory — but
  chart-inspector is then on the admission path for a CompositionDefinition write.
- **Interaction with sharding.** `pool` and `shards` are mutually exclusive per Kind (a sharded Kind
  wants isolation by construction). Worth a CEL rule on the CRD rather than a doc sentence.
- **Remote targets.** A projected spoke runs its own CDCs ([[remote-projection-self-seeding.md]]).
  Is a pool projected per spoke, and does pool identity survive projection? Untested — there is no
  spoke on 057 to try it on.
- **Does the floor itself come down?** — **answered, yes** (§1.5, core-provider#182): a
  metadata-only CRD informer recovers most of it, orthogonally to pooling and with far less risk.
  What remains open is how much of the *residual* ~45 MiB is reachable.
  [[cdc-sharding.md]] §10 notes the catch — the informer's `UpdateFunc` diffs the cached **spec**,
  so stripping spec breaks HighPriority-update detection.

---

## 8. Recommendation

**Build it, in the order below, and only as far as the measurements justify.**

0. **Profile the 88 MiB floor** — **DONE (2026-10-22), and it changed the answer.** 83% of an idle
   controller's heap is a CRD cache nothing reads (§1.5, core-provider#182). **Fix that first, then
   re-measure the fleet**: it is local, adds no failure domain, and roughly halves what pooling
   would recover. Steps 1–4 below remain worth doing on their own merits; step 5 should not be
   funded until the post-#182 numbers are in.
1. **`spec.controller.pool`, inert** — field, validation, status surface. No render change.
2. **The reconcile-time ownership guard** (§6.1), inert. Ship before anything can mis-sequence.
3. **Per-controller telemetry attribution** (§4.7). A blocking prerequisite, independently correct.
4. **CDC multi-GVR manager** (§5), shipped hosting exactly one GVR — a pure refactor, provable by
   rendering byte-identical Deployments.
5. **The pool object set + membership watch + ref-counted lifecycle** in core-provider.
6. **Migrate the idle cohort** — the page/portal and CRD-only Kinds (§6.2), 26 pods and 3 968 MiB
   between them — then measure, and stop there until a second pool is justified.

Steps 0–3 are independently valuable and carry no new failure domain. Step 4 is the bulk of the
work and is behaviour-preserving. Only steps 5–6 change the failure model, and by then the sizing
rule (§4.2) is measured rather than assumed.
