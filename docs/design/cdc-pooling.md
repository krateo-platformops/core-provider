---
type: Decision
title: "Design: an opt-in controller pool for the CDC, to reduce pod count"
description: Reopens #181's design with pod count as the goal - a named, opt-in pool for composition controllers, built in #181's step order with steps 5-6 unfunded.
resource: ghcr.io/krateo-platformops/composition-dynamic-controller
tags: [design, scaling, cdc, pool]
status: approved
timestamp: 2026-11-08T00:00:00Z
---

# Design: an opt-in controller pool for the CDC, to reduce pod count

**Status:** design approved (Diego, 2026-11-08); steps 1–3 cut, step 4 next, steps 5–6 **unfunded**.
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
| 4 | cdc **multi-GVR manager**, shipped hosting exactly one GVR: a pure refactor, proven by byte-identical rendered Deployments | no | next, after 1–3 |
| 5 | pool object set, membership watch, ref-counted lifecycle | **yes** | **unfunded** |
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

## 6. Not in scope

Scale-to-zero (provider-runtime#39) removes controllers whose Kind has **no** instances. That was 2
of 68 composition controllers on #245's count, so for the cdc fleet the pool is the lever and
scale-to-zero is not. They cut pods in different places, and neither substitutes for the other.
