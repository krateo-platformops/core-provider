---
type: Decision
title: "Design: an opt-in controller pool for the CDC, to reduce pod count"
description: Reopens #181's design with pod count as the goal - a named, opt-in pool for composition controllers, built in #181's step order; step 5 funded 2026-11-16 (pools of about 10), step 6 unfunded.
resource: ghcr.io/krateo-platformops/composition-dynamic-controller
tags: [design, scaling, cdc, pool]
status: approved
timestamp: 2026-11-08T00:00:00Z
---

# Design: an opt-in controller pool for the CDC, to reduce pod count

**Status:** design approved (Diego, 2026-11-08); steps 1–4 done; step 5 **funded** (Diego, 2026-11-16, pools of about 10; §5c); step 6 **unfunded**.
**Supersedes:** the closed design in PR #181, whose text argued from memory. This document keeps its
design and its step numbering and replaces its premise.

## 1. Goal

**Fewer composition-controller pods.** Today core-provider runs one composition-dynamic-controller
(cdc) Deployment per Kind × served version. A CompositionDefinition should be able to opt into a
**shared pool** instead, or keep its **dedicated** controller. Absent, nothing changes.

The goal is pod count, not memory. #181 argued for pooling on memory: about 95% of a controller's
memory looked like fixed per-process floor. #182 (2.13.27) then removed the CRD cache that made that
floor, and the floor is now a measured **20 MiB**. Pooling saves little memory now (§2.2). It still
removes pods, and everything that scales with pods.

## 2. Why pod count, measured

### 2.1 Almost every controller watches one composition

krateo-057, #245, cdc Deployments enumerated by container image and instances counted per GVR. The
composition total was checked against an independent count of 83:

| | |
|---|---|
| cdc controllers (Kind × served version) | 68 |
| watching exactly one composition | **64** |
| compositions in total | 83 |

So nearly every pod exists to reconcile a single object.

### 2.2 What scales with pods, not with work

- **Scheduling and reservations.** Every pod holds its requests and a slot against the node's pod
  limit: the 8.25 GiB and 3.3 cores reserved that #181 counted, whatever the controller is doing.
- **Per-controller scaffolding.** One ServiceAccount, ClusterRole/Role and binding set, ConfigMaps
  and a Service per Kind × version.
- **One cluster-wide CRD LIST/WATCH per process**: `dynamic.WatchCRDsAndInvalidate`,
  `go/composition-dynamic-controller/main.go:387`.

Memory, for contrast. #202's retake on 057 (2026-11-07, 2.13.72, 68 pods) measured a floor of
20.0 MiB, a median of 55.1 and a max of 116.5, total 3,935 MiB. Cost tracks reconcile activity,
not process count, so pooling recovers at most about 68 × 20 MiB. That is real, but secondary.

## 3. Design

### 3.1 API

```yaml
spec:
  controller:
    pool: frontend-pages   # optional; absent means a dedicated cdc, exactly as today
    workers: 4             # existing
    resyncInterval: "10m"  # existing
```

`pool` is an optional string on the existing `ControllerConfig`, validated as a DNS-1123 label by the
CRD schema. core-provider runs no admission webhook, so the schema is the only gate. **Absent renders
exactly what it renders today**, byte for byte.

**A name, not a boolean.** The pool's ServiceAccount becomes the subject of every member's derived
RBAC, so it holds the **union** of its members' permissions. #181 measured a whole-fleet union of 133
namespaced group/resource pairs against a median of 6 per composition. One global pool would widen
every member's identity about 22 times. Names let operators group Kinds with similar permissions.
"Dedicated" is simply a pool of one.

### 3.2 Membership: the pool watches, it is not told

A pool must not receive its member list as env. Every join or leave would rewrite its ConfigMap and
restart it, interrupting every other member. Instead the pool watches CompositionDefinitions. For
each one naming it, Ready, with known served versions, it starts one controller per GVR × version.
When a definition stops naming it, it cancels that controller.

### 3.3 What pooling costs, stated before it is recommended

- **Failure domain.** Today a crash stalls one Kind; pooled, it stalls every member. #99 (a goroutine
  leak that drove three controllers into an OOM loop) shows how a per-Kind defect becomes a
  pool-wide outage.
- **The RBAC union** (§3.1).
- **Noisy neighbours.** One member's slow chart delays the others. `workers` must be capped
  pool-wide.
- **Telemetry attribution** (step 3). Today one pod is one Kind: `krateo.io/composition-gvr` is a
  process-wide OTEL resource attribute (`main.go`), and `internal/metrics` holds one process-global
  object keyed by `deploymentName`. Pooled and unchanged, every member's signal would be attributed
  to the pool. Nothing errors; per-Kind numbers just become wrong. **That is why step 3 blocks any
  pool.**
- **The two-writer window when a Kind moves into or out of a pool.** The dedicated Deployment and
  the pool's controller would both reconcile the same releases. Steady state is safe; the move must
  be stop-before-start. Step 2 is the guard.

## 4. Steps (#181's numbering)

| # | step | changes pods? | status |
|---|---|---|---|
| 0 | profile the floor | no | **done**: #182 took it from about 88 MiB (inferred) to 19.5–20 MiB (measured) |
| 1 | `spec.controller.pool`, **inert**: field, validation, status surface; no render change | no | **cut** |
| 2 | **reconcile-time ownership guard**, inert: one reconciler per Kind × version, refusing a second | no | **done**: a Lease per Kind × version (Diego, 2026-11-14), below |
| 3 | **per-controller telemetry**: GVR as a per-controller metric/span/log attribute; metrics keyed per member | no | **done**: `krateo.composition.gvr` on every signal (below) |
| 4 | cdc **multi-GVR manager**, shipped hosting exactly one GVR: a pure refactor, proven by byte-identical rendered Deployments | no | **done**: `internal/manager` (below) |
| 5 | pool object set, membership watch, ref-counted lifecycle | **yes** | **funded** (Diego, 2026-11-16; design §5c) |
| 6 | migrate the idle cohort (page/portal and CRD-only Kinds), measure, stop | **yes** | **unfunded** |

Steps 1–3 add no failure domain and are each worth having alone. Step 4 is most of the work, and it
preserves behaviour. **No step before 5 removes a pod.** Funding step 5 is a separate decision, to
be taken with steps 1–4 landed and the sizing rule (pool-wide workers, memory per member) measured
rather than assumed.

## 5. How each step is proven

- **Step 1:** rendering with `pool` absent is byte-identical to today (golden test over the shipped
  chart); schema validation rejects non-DNS-1123 names.
- **Step 2:** a kind integration test where a second reconciler for the same Kind × version is refused.
  Built as an envtest (a real kube-apiserver is all it needs):
  `go/composition-dynamic-controller/internal/ownership/ownership_envtest_test.go` builds two
  controllers exactly as `main.go` does, and the second reconciles nothing for longer than a lease
  duration, then takes over once the first has drained and released.

### Step 2 as built

A `coordination.k8s.io` Lease per Kind × version, `<version>.<plural>.<group>`, in the controller's
namespace. It names the Kind × version, never a Deployment or a pod, so a dedicated controller and a
pool's controller compete for the same Lease; that is what makes the move "stop-before-start"
without coordination between them. The mechanism is unstructured-runtime's `controller.Ownership`
(#87): the informer lists while waiting, so readiness keeps meaning "can see its resource" and a
rolling update is not deadlocked; workers start only on the Lease; the Lease is released after the
drain; losing it stops writes at once. core-provider renders the name once
(`templates.ControllerLeaseName`) into both the controller's ConfigMap and its Role. Step 5's pool
takes one such Lease per member it hosts.
- **Step 3:** unit and kind tests only. Two controllers in one process must produce two distinctly
  attributed metric streams. There is no production observation planned. One would need a read on a
  real cluster after a release, which is Diego's call each time.
- **Step 4:** byte-identical rendered Deployments before and after, plus the existing integration suite.

### Step 4 as built

`go/composition-dynamic-controller/internal/manager` splits the process from its controllers.
`Process` is what every member shares (REST config, event recorder, chart getter, REST mapper and its
CRD watch, the saturation governor, retry and resync settings, and the two process servers: one
metrics server for the shared registry, one health endpoint). `Member` is one Kind x version: its
label selector, ownership Lease, status projections, apiRef resolver, audit recorder, handler and
metrics/span view. main.go builds the Process from its flags and exactly one Member, so nothing a
cluster sees changes: no chart or ConfigMap change, the same controller options, the same probes.
Proven by unit tests (no member and a duplicate refused; one controller per member; readiness names
the member that is not ready) and an envtest hosting two Kinds in one process (Ready only once both
have listed, a ready member beside an unserved one keeps the process not-ready and names it, clean
stop). Left to step 5 and stated as provisional in the package: /readyz needs every member, and a
member that fails (a lost Lease included) stops the process.

### Step 3 as built

`krateo.composition.gvr` = `<group>/<version>/<plural>` on every metric point, reconcile root span
and log record of every cdc, dedicated ones included (agreed with observability, so their queries
migrate once). Mechanism: unstructured-runtime v1.20.0's `telemetry.Metrics.With` (a per-controller
view over shared instruments, with its own in-flight count) and `builder.WithSpanAttributes`; the
cdc's own metrics view (`metrics.Metrics.With`) rides in each handler entry's `ctx`
(`metrics.NewContext`/`FromContext`), so step 4's multi-GVR manager gives each hosted controller its
view by giving it its own handler. Process-level signals stay process-level: CPU saturation, and the
resource attributes (`krateo.io/composition-gvr`, `service.instance.id`), which a pool will set to the
pool. Proven by two views in one process recording distinct streams, in both the runtime and the cdc.

## 5b. Sizing, measured (2026-11-15, krateo-057, read-only)

Measured before funding step 5, as §4 requires, not assumed.

**Data.** observability's ClickHouse, last 24h, one row per cdc Deployment:
- memory: container working set, kubeletstats 60 s samples, so a peak under 60 s is not seen;
- CPU: p95 and max;
- reconciles and in-flight: from the runtime's own metrics.

Joined to the 73 cdc Deployments running on 057 at measurement time (70 matched), with instance
counts per Kind × version taken from the cluster. Point snapshot (`kubectl top`) for the process
floor.

| | today |
|---|---|
| cdc pods | 73, one per Kind × version |
| memory, 24h max per pod | median 69 MiB, p90 83, max 160 (installers) |
| memory requested / limit | 128Mi / 512Mi each: 9,344 MiB requested, 2.4x the snapshot's actual use |
| process floor (a cdc with 0 instances) | 22 MiB (3 of 3, snapshot) |
| instances per controller | 69 of 73 have exactly one; 3 have none; 1 has 6-20 |
| concurrency | `in_flight_max` <= 1 for every controller over 24h: none ever ran two reconciles at once |
| CPU, 24h p95 | median 48m; 21 controllers at >= 100m, 12 at >= 250m |

**Findings.**
1. **Worker demand is negligible.** No controller needed a second worker in 24h, so pool-wide
   workers sized to about 2 per pool cover the idle cohort.
2. **The page/portal Kinds are not idle.** §4 step 6 named them the idle cohort. Measured, the ten
   frontend pages run 141-537m CPU p95, and only 4 of the 17 page/portal Kinds are under 100m. The
   idle cohort is the provider and CRD-only Kinds, minus kagentcrds (462m) and frontendcrds (358m).
3. **The busy controllers stay dedicated**, among them installers (1.2 cores p95, 138-185 MiB),
   agentgatewaycontrollers (2.5 cores p95, 160 MiB) and builderpublishes (921m).
4. **Pooling saves the process floor, not the working set.** A member keeps its instance's
   helm-render working set, about 31 MiB above the floor for one instance. What is shared is the
   ~22 MiB floor per member, plus the per-pod scheduling overhead.

**Model: the idle cohort, CPU p95 < 100m, 49 controllers.** Each member beyond the first stops
paying the floor. Per-pod maxima are summed, which assumes their peaks coincide, so this is
conservative.

| pools of | pods (from 49) | peak memory sum | requests at 1.5x peak |
|---|---|---|---|
| 5 | 10 | 2,300 MiB (-27%) | ~3,450 MiB (vs 6,272 today) |
| 10 | 5 | 2,190 MiB (-31%) | ~3,285 MiB |
| 20 | 3 | 2,146 MiB (-32%) | ~3,219 MiB |

The memory saving flattens past about 10 members per pool, while the failure domain (§3.3) grows
linearly. Requested memory halves at any pool size, mostly because 128Mi per idle cdc is about twice
its peak, not because of pooling as such: right-sizing the dedicated request is a separate,
cheaper lever.

**Not measured, and a caveat on each.**
- Memory peaks under 60 s.
- Shared caches a pool would add savings from (discovery, the helm chart cache).
- The RBAC union per pool (§3.1). A pool's ServiceAccount holds the union of its members'
  permissions, so pools must group Kinds with similar permissions, and that grouping is not
  derived from these numbers.
- The runtime's reconcile-duration histogram uses millisecond-style default bucket bounds on a
  seconds metric, so its p95 is unusable here (reported by observability; an unstructured-runtime
  fix).

## 5c. Step 5 design (funded by Diego 2026-11-16: pools of about 10)

Step 5 is the first step that removes pods. Everything below is opt-in. A definition without
`spec.controller.pool` renders exactly what it renders today. Step 6, moving the idle cohort on
krateo-057, is a separate roll that waits for Diego's go.

### Scope
Local definitions only, for the same reason as `scaleToZero`. A remote definition's controller runs
on its spoke, where a pool would span clusters. A CEL rule on `spec.controller` refuses `pool` on a
remote definition. `pool` and `scaleToZero` are mutually exclusive. A pool's process is shared, so
one member scaling to zero would stop the others.

### Objects
A pool `<p>` lives in the management namespace (`controllerNamespace`, krateo-system), beside the
dedicated controllers it replaces:

| object | name | holds |
|---|---|---|
| Deployment | `cdc-pool-<p>` | one cdc process in pool mode |
| ServiceAccount | `cdc-pool-<p>` | the subject of every member's derived RBAC, hence the union (§3.1) |
| ConfigMap | `cdc-pool-<p>-configmap` | process-level env: pool-wide workers, OTEL, saturation |
| ConfigMap | `cdc-pool-<p>-members` | the membership index: one key per member, value = its env ConfigMap name |
| Service | `cdc-pool-<p>` | metrics, when enabled |

Each member keeps its per-Kind × version objects under today's names: its env ConfigMap
`<resource>-<version>-configmap`, its derived RBAC, its ownership Lease grant and its RESTAction RBAC.
Only their subject changes, from the member's ServiceAccount to the pool's. A member gets **no**
Deployment, ServiceAccount or Service of its own.

### Membership: the pool watches an index, it is not told (refines §3.2)
§3.2 had the pool watch CompositionDefinitions. Built that way, the cdc would have to re-derive each
member's configuration (chart inspection, status projections, apiRef), which core-provider already
derives and renders as the member's env ConfigMap. So the pool consumes core-provider's output, not
its input:

- core-provider writes `cdc-pool-<p>-members` as a pure function of the live member env ConfigMaps
  (below), recomputed on every member's write. There is no increment or decrement to drift.
- The pool **watches** that one ConfigMap by name, and reads each member's env ConfigMap. A key
  added starts that member. A key removed stops it (drain, then release its Lease). A member's env
  ConfigMap changing restarts that member only. Nothing restarts the process.
- **Least privilege:** the pool's Role grants `get`/`watch` on its index and `get` on each member's
  env ConfigMap, by `resourceNames`. Watching by label would need a namespace-wide ConfigMap read in
  krateo-system, which RBAC cannot narrow by label. core-provider rewrites the Role as members join
  and leave.

### Membership, as built (#652)
A member is **a live env ConfigMap** (`<resource>-<version>-configmap`) labelled `krateo.io/cdc-pool:
<p>`, and the index is computed from one LIST of them. Deploy writes that ConfigMap before its
controller runs, and Undeploy removes it after the controller has stopped, so it says exactly what the
index must: which Kind x versions have a controller due in this pool. Neither of the other two sources
can:

- A definition's **status** is written only after the reconcile that deploys has returned. During a
  bump it still names the old version, and after a Create it names none. An index computed from it
  re-listed the old version and dropped the new one (#652).
- Its **spec** names the version it wants, not the older ones still running beside it (`upgradePolicy`
  Manual or Paused keeps the old version's instances, and so its controller).

Every version from its first deploy to its retirement is therefore a member: a new version from its
bump, a definition created in a pool from its Create, an old version as long as it is kept, a paused
definition's, and a definition whose status was never written because core-provider stopped first. Only
the Kind x version's owner deploys, so a follower's own `pool` still changes nothing.

**Writes are serialised and never replayed.** Writes to one pool take one lock in the process, and the
index is updated at the resourceVersion of the LIST it was computed from. A refused write starts again
from a new LIST. A member is granted its env ConfigMap before the index names it.

**Leaving is stop-before-revoke**, the pool's form of #263: a retired version, a definition being
deleted, one that stops naming `<p>` or names another pool. The env ConfigMap is annotated
`krateo.io/cdc-pool-draining` (a restarted core-provider still knows), the key leaves the index, and
core-provider waits until the controller's ownership Lease has been unheld for 3s, for at most 60s.
Only then is the ConfigMap, the RBAC or the Lease grant removed or given to another identity. A Lease
whose record does not change for its own duration belongs to a process that died, and counts as unheld:
client-go's rule, measured on core-provider's clock from when it first saw the record, so no two clocks
are compared. **At the bound nothing is revoked**: the Update or Delete fails, naming the Lease's holder
in `Synced`, and is retried. Retirement still precedes the instances' migration, so between a key's
leaving and the relabelling they have no controller, exactly as with a dedicated one.

**Observe compares.** A pool is in no definition's digest (a member's digest stops at its own objects),
so an entry lost from the index, or a release that changed only the pool's image or `cdc.pool` sizing,
stayed as it was until some member's next Update. Every pass of a member now compares its own entry and
the mark on its env ConfigMap, that no other pool lists it, and the pool's index, Role, RoleBinding,
ServiceAccount, process ConfigMap and Deployment (through a server dry run, like a dedicated one), and
reports not up to date with what differs. Update repairs all of it.

### Lifecycle, ref-counted by recomputation
On each reconcile of a definition naming `<p>`, core-provider applies the pool objects (idempotent),
labels and binds the member, and recomputes the index. A definition that stops naming `<p>`, or is
deleted, is removed from the index. When no env ConfigMap is left in it, a draining one included, the
**last leaver** deletes the pool's objects: the Deployment first, its pods waited for, then the RBAC,
and the ConfigMaps an unfinished deletion is found by last. A failed list removes nothing (the rule the
teardown paths already follow).

### Cutover, stop-before-start, by step 2's Lease
Joining a pool and leaving a dedicated controller are one reconcile. core-provider adds the member
to the index and then deletes its dedicated Deployment (a controller deployed before step 2, which
holds no Lease, is deleted first). The pool's member starts its informer at once
(so readiness keeps meaning "can see its resource"), but its workers start only on the Lease. The
dedicated controller releases the Lease after its drain. There is never a second writer, and
nothing else needs to coordinate. Leaving a pool is the mirror image, waiting for the Lease as above.

### Failure isolation, which step 4 left provisional
Step 4 stops the whole process when a member fails, a lost Lease included. In a pool that would
turn one member's fault into an outage for all of them (§3.3), so pool mode changes it:
- A member that fails, or loses its Lease, **is stopped and restarted alone**, with backoff. The
  others keep running.
- Only process-level failures stop the process: the REST config, the metrics or health server,
  the shared informer cache.
- `/readyz` stays "every member has synced". `/healthz` stays process-only (#333). A member that keeps
  failing is reported on its own definition as `ControllerActive=False`, with a reason naming the
  member, so the pool's pod is not killed for it.

### Sizing and workers
Workers are capped **pool-wide**: the pool's `workers` is the process total, and no member takes
more than its own `spec.controller.workers`. The default pool request comes from §5b: the members'
summed peak × 1.5, so ~660 MiB for 10 idle members, against 1,280 MiB requested by ten dedicated
controllers today.

**Where the numbers live (Diego, 2026-11-17).** `spec.controller.pool` is only a name, so a pool is
sized by core-provider chart values, set per cluster in installer's top-level `core-provider:` block
when a cluster differs from the default:

| value | meaning | default |
|---|---|---|
| `cdc.pool.default.resources` | the pool Deployment's resources. It REPLACES `cdc.resources` as a whole block, like `spec.controller.resources` for a dedicated controller | requests 100m / 660Mi, limits.memory 1Gi, no limits.cpu |
| `cdc.pool.default.workers` | the pool's TOTAL concurrent reconciles across its members | 10 |
| `cdc.pool.overrides.<name>.{resources,workers}` | one pool's size. It deep-MERGES over the default, so a partial override keeps the default's other keys | none |

installer#1012 (rule 4 of check-cdc-resources-contract) encodes this contract, including that the
shipped default and a partial override pass.

**Two worker counts, never one key.** The pool total reaches the cdc as
`COMPOSITION_CONTROLLER_POOL_WORKERS`. A member's own count stays its `spec.controller.workers`,
else the controller default. They have different denominators and never share a name.

**How the total is enforced (core-provider#561).** The members share ONE governor built to be shared
(unstructured-runtime v1.25.0, `ratelimit.Options.Shared`), whose ceiling is the pool total, enforced
with saturation off too and lowered by it when on. A member's worker takes its slot only once it has
popped an item. Taking it before the pop, which a single controller does, would let an idle member
hold capacity a busy member needs. The governor's CPU sampler runs once for the process, not once per
member. `manager.Pool.Run` refuses any other governor. The cost: a high-priority item that arrives
while a member's worker holds an item waits for that worker. That wait is bounded by the member's own
worker count, in passes. **A member defaults to the fleet default, 10 on the chart (Diego,
2026-11-17).** That's `cdc.env` `COMPOSITION_CONTROLLER_WORKERS`, the same value a dedicated
controller gets, so a busy member can use the whole pool total while the others are idle, which is
the common case for the idle cohort a pool is for. The bound is therefore about 10 passes, unless a
definition sets its own `spec.controller.workers`.

**The projected authn token is always mounted (Diego, 2026-11-17).** A pool pod runs as the pool's
ServiceAccount and mounts the authn-audience token whether or not any member has an apiRef.
Mounting it only while one does would change the pod template, and so restart the whole pool, when
the first such member joins or the last leaves. The token grants nothing by itself: authn acts only
on a ServiceAccount it has a mapping for.

### How step 5 is proven
- **Unit:** the index is a pure function of the definitions (join, leave, rename, delete, a failed
  list removes nothing); the Role's `resourceNames` track the index; the CEL rules refuse a remote
  pool and `pool` with `scaleToZero`.
- **Envtest, core-provider (#652):** `pool_652_envtest_test.go`, through the real reconciler, with an
  invariant replayed over every persisted state: a controller is in at most one index; a version with
  an instance has a Deployment or exactly one index entry, unless it is being retired; whatever an
  index names has its env ConfigMap, its Lease grant and the pool's permission to read it.
- **Envtest:** a pool hosting two members; a member added and removed at runtime without the process
  restarting; a member that loses its Lease is restarted alone while the other keeps reconciling.
- **Integration (kind):** a definition moves dedicated → pool → dedicated, and stop-before-start
  holds (sampled, as #313's migration test does). The last leaver deletes the pool.
- **Golden:** a definition without `pool` renders byte-identical objects (step 1's test, kept).

### Build order
1. core-provider: the pool objects, the index, member RBAC bound to the pool's ServiceAccount,
   dedicated cutover, CEL rules. Inert until a definition sets `pool`.
2. cdc: pool mode (the index watch; members added, removed and restarted at runtime; per-member
   failure isolation).
3. Envtest and integration, then a release.

Step 6, the 057 roll, follows on Diego's go.

## 6. Not in scope

Scale-to-zero (provider-runtime#39) removes controllers whose Kind has **no** instances. That was 2
of 68 composition controllers on #245's count, so for the cdc fleet the pool is the lever and
scale-to-zero is not. They cut pods in different places, and neither substitutes for the other.
