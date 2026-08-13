---
type: Decision
title: "Design: horizontal sharding for the composition-dynamic-controller"
description: The plan of record for sharding one high-cardinality composition Kind across CDC replicas — and the recorded decision NOT to build it yet.
resource: ghcr.io/krateo-platformops/composition-dynamic-controller
tags: [design, scaling, cdc]
status: implemented
timestamp: 2026-08-07T00:00:00Z
---

> **Status (re-verified 2026-08-07): the recorded decision is implemented — i.e.
> sharding is deliberately NOT built.** No sharding code exists in
> `go/composition-dynamic-controller/`; the doc itself recommends not building it
> for the motivating case (§2). If cardinality pressure returns, this remains the
> plan of record.

# Design: Horizontal sharding for the composition-dynamic-controller (CDC)

> Status: **Draft for discussion** · Audience: core-provider + composition-dynamic-controller +
> unstructured-runtime maintainers · Scope: split the reconciliation of a single high-cardinality
> composition Kind across multiple CDC replicas, safely, when one process can no longer keep up.
> Written pre-migration: the forks it references are now the canonical `krateo-platformops` repos (the historical upstream org is dead).
>
> Goal: define the *plan of record* for CDC sharding — the design, the concrete code touchpoints, the
> correctness-critical migration, and the residual risk — so the team can review the approach before
> any implementation. **This doc deliberately recommends NOT building it yet** for the case that
> motivated it; see §2.

---

## 0. TL;DR

1. **The shape today.** core-provider is a meta-operator: it watches `CompositionDefinition`s and
   provisions **one CDC Deployment per Kind × served version** (rendered from a single global
   template, `internal/controllers/compositiondefinitions/compositiondefinitions.go` →
   `CDCtemplateDeploymentPath`). Each CDC is **single-active**, watches its one GVR narrowed by a
   `krateo.io/composition-version` label selector, reconciles every instance, and drives **one Helm
   release per instance**. The only concurrency knob is `--workers` (default 1).

2. **The honest non-goal (read this first).** For the case that surfaced this — the GKE scale test
   with **~60,000 instances of one Kind** (`benchapps`) behind one CDC — sharding is **premature**.
   The measured wall is **throughput, not memory** (only ~2–3% of the 60k had a status, i.e. one
   worker never got through them; the average object is ~774 B, so the informer cache is ~0.1 GB).
   The fix is **`spec.controller.workers`** (the per-Kind config surface shipped as Step 0 —
   core-provider PR #43, chart PR #74): raise `--workers` and lengthen resync on that one Kind and a
   single pod should keep up. **Shard only if a single pod then saturates** (CPU / Helm concurrency /
   apiserver client QPS) **or you need blast-radius isolation** (one wedged Helm release must not
   stall the other tens of thousands).

3. **Why the framework doesn't help.** `controller-runtime` ships **leader election only** — an
   active/passive HA model (failover, not scale-out); it has no native active-active sharding, by
   explicit maintainer decision (kubernetes-sigs/controller-runtime #2576, #1456). The building block
   is arriving one layer down (KEP-5866 *Server-side Sharded List and Watch*, sig-api-machinery,
   alpha), but assignment + handover remain the operator's problem.

4. **The recommended design — fixed-K static sharding.** Assign each instance a stable shard by a
   **rendezvous hash of its name**, materialized as a `krateo.io/shard` label **stamped at admission**
   (extending the existing `composition-version` MutatingAdmissionPolicy). core-provider renders **K
   CDC Deployments per Kind**, each with a server-side `shard==i` label selector so its cache and LIST
   divide by K. A **reconcile-time guard in both CDC binaries** no-ops on objects that aren't this
   shard's. Because the bucket is a pure function of the immutable name, **assignments never move in
   steady state** — there is no drain/handover to get wrong.

5. **The crux (why this is hard, not just fan-out).** The CDC reconcile is **idempotent but NOT
   concurrency-safe**: its side effect is a Helm release with single-owner recovery (a release stuck
   `Pending*` past a grace period is rolled back, on the assumption of one reconciler), and reconcile
   **re-reads the object live by name** — so a shard filter at the informer/enqueue layer *does not
   fence the write*. Two live owners of one composition corrupt release history. Fixed-K avoids
   steady-state handover; **the migration (labeling 60k existing objects without a two-owner window)
   is the real cost**, and any later change to K is a planned, quiesced operation.

6. **Residual risk even done right.** A `>leaseDuration` crash/GC-pause can't be fully fenced
   (Kleppmann's lease-without-fencing gap); Kubernetes optimistic concurrency (`resourceVersion`) is
   only a partial fence and never guards the `helm upgrade` side effect. Tolerable *only* because K is
   fixed, the reconcile-time guard makes reconcile shard-authoritative, and K changes quiesce the
   affected shard.

---

## 1. Motivation

core-provider provisions a dedicated CDC per composition Kind (per served version). That is already a
form of sharding — **by Kind, across separate Deployments** — and it scales the platform well when
load is spread across many Kinds. It does **not** help a *single* hot Kind: that Kind's one CDC is a
single process, single-active, tuned only by `--workers` against one shared work queue.

The concrete trigger is the GKE scale test (`krateo-installer-test`): Kind `benchapps`
(`composition.krateo.io/v0-1-0`), **60,000 instances in one namespace**, one
`benchapps-v0-1-0-controller` at `--workers=1` (image `1.2.1`, `resources: {}`). Measured facts that
shape this design:

- **Throughput is the binding wall, and the cluster proves it.** With a 3-minute resync the controller
  re-enqueues all 60k (~333/s of demand); one worker delivers ~0.1–0.5/s (each reconcile = a live
  apiserver GET + a Helm 3-way diff + a chart-inspector round-trip + two status writes), so a full
  sweep is **33–83 hours**. Only **~2–3% of the 60k carry a status** — a direct etcd readout that the
  single worker never got through the population.
- **Memory is *not* the wall.** The average object is ~774 B (only ~2–3% carry the ~6.7 KB status);
  the informer cache is ~0.1 GB now, ~1–1.6 GB even at full reconcile coverage — survivable on one
  pod. **Sharding's best win (dividing cache/LIST by K) solves a wall this workload doesn't have.**

**Therefore the first move is not sharding.** It is `spec.controller.workers` (Step 0), which makes
`--workers` a per-Kind knob without touching the shared template or restarting the fleet. This
document exists for *after* that: when a single hot Kind saturates one pod even at high `--workers`,
or when blast-radius isolation across shards is an operational requirement.

---

## 2. Goals / Non-goals

**Goals**
- Split one Kind's instances across **K** CDC replicas so per-pod reconcile load, cache, and LIST
  scale down by ~K.
- Preserve the **single-writer-per-object** invariant at all times, including during rollout and K
  changes — never two live reconcilers driving the same Helm release.
- Reuse existing machinery: the per-Kind Deployment render (extended in Step 0), the server-side label
  selector the listwatcher already applies, and the admission-time label-stamping already deployed.

**Non-goals**
- **Not** the auto-rebalancing consistent-hash *ring* (Tim Ebert's `kube-controller-sharding` model).
  It is the state of the art and the eventual north star, but it is a heavyweight, generic system
  (external sharder + `ControllerRing` CRD + a mutating webhook on the admission hot path + ShardLease
  state machine) justified when sharding *arbitrary, un-cooperative* controllers. It also cannot fully
  fence the crash path either. See §9.
- **Not** a memory fix — memory is already fine for the motivating case (§1).
- **Not** running >1 replica of a CDC *without* sharding — that is unsafe today: both replicas watch
  the same GVR and double-drive every Helm release (§3.2).
- **Not** dynamic per-object rebalancing. K is fixed and changed rarely, as a deliberate operation.

---

## 3. Background & the constraints that force the design

### 3.1 The correctness core (from the ecosystem)

Every correct intra-Kind sharding scheme for a *stateful* reconciler needs four properties
(synthesized from Ebert's `kube-controller-sharding`, Argo CD, Flux, Knative — see the companion
"Controller sharding" brief for citations):

1. **Single-writer handover** — never two active replicas on one object, *including the rebalance
   window*.
2. **Consistent/rendezvous hashing** — assignment moves only ~K/N objects when N changes, not ~all
   (naive `hash mod N` reshuffles the whole population on any K change).
3. **Lease-based membership** — the live shard set is discovered and failures detected without a
   single contended leader lease.
4. **Server-side shard-filtered watches** — each shard watches/caches only its subset via a label
   selector pushed to the apiserver, or you gain throughput but every replica still caches everything.

Fixed-K static sharding (this design) satisfies (1) by never moving an assignment in steady state,
(2) by rendezvous-hashing the immutable name, and (4) by the selector the listwatcher already applies.
It deliberately does **not** implement (3)'s dynamic membership — with fixed K there is no ring to
recompute; the trade is that a K change is a manual, quiesced operation rather than automatic.

### 3.2 The three CDC facts that make this a *handover* problem, not a *hashing* problem

Verified against `composition-dynamic-controller` + `unstructured-runtime`:

- **The reconcile side effect is a Helm release with single-owner recovery.** The composition handler
  treats a release that has been `Pending*` longer than a grace period as "stuck (controller died
  mid-op)" and issues a `Rollback`. That heuristic *assumes one reconciler*: a second live owner
  holding a legitimately-pending upgrade gets it rolled out from under it → `Upgrade↔Rollback` thrash
  that **corrupts release history**. Concurrency here is not merely wasteful; it is destructive.
- **Reconcile re-reads the object live by name**, then drives Helm. So **a shard filter at the
  informer/enqueue layer does not fence the write** — a key already on the work queue survives a
  label change and is reconciled against the live object regardless. The fence must live at reconcile
  time (§4.3).
- **The substrate is already present.** The listwatcher applies its `LabelSelector` to **both** List
  and Watch (server-side), so a shard predicate divides the cache and LIST for free; and core-provider
  already stamps labels at admission via a `MutatingAdmissionPolicy` (`composition-version`, projected
  by `external.ensureCompositionVersionPolicy`, requires Kubernetes ≥ 1.36). core-provider hosts **no
  admission webhook**, so the CRD schema + this policy are the only cluster-side gates.

### 3.3 What controller-runtime and the CDC give you

- **controller-runtime:** leader election (active/passive) only; no native sharding (maintainer
  position: kubernetes-sigs/controller-runtime #2576 "focused on a single controller acting as a
  leader", #1456 "very much a 'no'"). Filtered caches (`cache.ByObject`/selectors) exist — the
  building block — but no assignment or handover.
- **The CDC / unstructured-runtime:** a single informer on one GVR narrowed by a label selector; a
  `--workers` pool over one shared priority queue; **no leader election, no sharding** in the runtime
  (leader election is referenced only as a future feature in comments).

---

## 4. Design: fixed-K static sharding

```
                         MutatingAdmissionPolicy (extended)
                         stamps krateo.io/shard = rendezvous(name) → 0..K-1  (create-if-absent)
                                         │
   CompositionDefinition ── core-provider ──renders K Deployments per Kind──┐
                                         │                                   ▼
                                   shard 0 CDC  (selector: version=… , shard=0)  → its 1/K of instances → Helm
                                   shard 1 CDC  (selector: version=… , shard=1)  → its 1/K of instances → Helm
                                     …                                        (each: reconcile-time guard)
                                   shard K-1 CDC(selector: version=… , shard=K-1)→ its 1/K of instances → Helm
```

### 4.1 Shard assignment — a label stamped at admission

- **Label:** `krateo.io/shard` = the shard index `0..K-1`, chosen by **rendezvous (HRW) hashing** of
  the object's `namespace/name`. Rendezvous over naive `mod K` so that a future K change moves only
  ~ΔK/K of objects instead of nearly all (relevant only to the K-change path, §6.2, but cheap to get
  right up front).
- **Who stamps it:** extend the existing `composition-version` `MutatingAdmissionPolicy` (managed by
  `external.ensureCompositionVersionPolicy`) to also set `krateo.io/shard` on CREATE — **create-if-
  absent** (`has(labels['krateo.io/shard']) ? existing : computed`).
- **The CEL problem:** admission CEL has **no hash function**, so it cannot compute a uniform bucket
  directly. Resolution: compute the bucket in **core-provider's Go** for the one-time backfill (§6),
  and have the CEL stamp the same value create-if-absent for *new* objects. If the name carries a
  parseable numeric key the CEL can derive the bucket from that, but rendezvous-in-Go is the general
  answer. **The distribution must be validated offline before cutover** (even, and matching between
  the Go backfill and the CEL).
- **Why create-if-absent is non-negotiable:** the policy matches CREATE *and* UPDATE, and the CDC
  writes an object's status ~twice per reconcile — so a policy that *recomputes* the bucket on every
  UPDATE would re-stamp it and **hop objects between shards mid-Helm**. The label is assigned once, at
  first write, and never changes for a fixed K.

### 4.2 Fan-out — K Deployments per Kind

- core-provider renders **K CDC Deployments** for the sharded Kind (suffix `-controller-<i>`), each
  passing `-shard=<i>`. This extends the Step-0 per-Kind render path (`internal/tools/deploy` +
  `internal/controllers/compositiondefinitions`) from one Deployment to a `0..K-1` loop; the per-shard
  name threads through the deploy / undeploy / lookup and config-hash logic.
- Each CDC adds `krateo.io/shard == <i>` to the selector it already builds (alongside the
  `composition-version` selector), so the apiserver returns — and the informer caches — only ~N/K
  objects. **This is the cache + LIST win, realized at the List/Watch boundary for free** because the
  listwatcher applies the selector server-side.
- K is a per-Kind setting. Natural home: extend Step 0's `spec.controller` with an optional
  `shards *int32` (guarded, see §10) so a hot Kind opts in without affecting others.

### 4.3 The reconcile-time guard (both binaries) — the actual fence

At the top of the composition handler's reconcile, **after** the live re-read of the object and
**before** any Helm call:

```go
if obj.GetLabels()["krateo.io/shard"] != myShard { return /* not mine: no-op */ }
```

Because it reads the freshly-GET'd object and gates the Helm side effect (not just the enqueue), it
**fences the actual write**. It closes the sub-second race where a key outlived a relabel. It does
**not** close a multi-second in-flight Helm op straddling a relabel — which is why K changes quiesce
(§6.2). The guard must exist in **every** binary that can select an object (the numbered shards *and*
the catch-all used during migration, §6.1).

### 4.4 Why fixed K (and not the ring)

An object's shard is a pure function of its immutable name, so **in steady state no object ever
moves** between shards. The entire drain→ack→reassign machinery of the ring exists only to make
*moves* safe; with fixed K there is nothing to hand over. You get properties (1), (2), (4) of §3.1
with a label, a selector, a render loop, and a guard — no external sharder, no CRD, no webhook on the
admission hot path beyond the label stamp already there.

---

## 5. Code touchpoints (per repo)

Concrete, mechanism-level (exact lines will shift; these are the functions/files that change):

**unstructured-runtime**
- `pkg/listwatcher` — no change needed for server-side filtering (already applies the selector to List
  + Watch); confirm the shard selector composes with the existing version selector.
- `pkg/controller` — surface the shard value so the handler can read it; the informer is built from
  the composed selector already.

**composition-dynamic-controller (CDC)**
- `main.go` — add a `-shard` flag (env `COMPOSITION_CONTROLLER_SHARD`); merge
  `krateo.io/shard == <shard>` into the label selector it builds for the controller.
- the composition handler (`internal/composition`) — the §4.3 reconcile-time guard, after the live
  GET, before the Helm call.

**core-provider**
- `internal/tools/deploy` + `internal/controllers/compositiondefinitions` — extend the per-Kind render
  from one Deployment to a `0..K-1` loop; thread `-shard=<i>` and the per-shard name through deploy /
  undeploy / lookup / config-hash. (Builds directly on Step 0's `spec.controller` render plumbing.)
- the `MutatingAdmissionPolicy` (`external.ensureCompositionVersionPolicy` + the policy asset) — add
  the create-if-absent `krateo.io/shard` mutation; project it to remote targets the same way the
  version policy is.
- a **backfill Job** (new) — paginated (`Limit`/`Continue`), label-if-absent, computing the *same*
  rendezvous bucket as the CEL; verifies per-bucket counts sum to the total before cutover.
- `apis/compositiondefinitions/v1alpha1` — optional `spec.controller.shards *int32` (min 1, sensible
  max), CRD-validated (no admission webhook exists, so the schema is the gate).

**core-provider-chart**
- the CDC Deployment asset — render the `-shard` arg + `shard==` selector per shard (extends the
  Step-0 template slots); ship/parameterize the extended admission policy and the backfill Job.

---

## 6. The migration — the correctness-critical part

Steady state is simple; **the danger is entirely in getting from "one unsharded CDC + 60k unlabeled
objects" to "K sharded CDCs" without a two-owner window.**

### 6.1 Trap A — the backfill/rollout window (fatal if sequenced naively)

Admission fires only on write, so the 60k **existing** objects have no `krateo.io/shard` label and
match **no** `shard==i` selector — at cutover they would silently stop being reconciled. The tempting
fix — keep the legacy unsharded CDC running "to cover the unlabeled ones" while the shards scale out —
is **worse**: the legacy selector is version-only and the shard selectors are `{version, shard==i}`,
so **every labeled object is in both informers**; both workers re-read it live and both drive
`helm upgrade`/rollback on the **same release** — a *guaranteed, whole-population* two-owner window
lasting minutes-to-hours.

**Safe sequence (mandatory order):**

1. **Guard first.** Ship the reconcile-time guard and roll the *existing* single CDC onto it,
   reconfigured with a **`krateo.io/shard DoesNotExist`** catch-all selector + catch-all guard
   semantics (reconcile only objects with no shard label). Selectors are now **disjoint by
   construction**: unlabeled → catch-all only; `shard==i` → shard i only. No object is ever in two
   informers.
2. **Deploy the create-if-absent admission policy** so *new* objects are born labeled.
3. **Run the paginated, label-if-absent backfill Job**, computing the same bucket as the CEL. Verify
   per-bucket counts sum to the total.
4. **Scale out the K numbered shards.** No overlap window, because the catch-all only ever selected
   `DoesNotExist`.
5. **Retire the catch-all** once coverage is 100% (its selector then matches nothing — a no-op).

**Two non-negotiables:** the guard exists in **both** binaries (catch-all: no-op if a shard label is
present; numbered: no-op if the label ≠ my shard), and the shard label is **create-if-absent
everywhere** (§4.1).

### 6.2 Trap B — changing K later

Because a shard's selector matches the *stored* label string (not a live hash), a `K: 6 → 8` bump is
either **inert** (existing objects keep their old labels; new pods select nothing → objects
unreconciled → Trap A again if new writes stamp buckets no pod selects) or, if you relabel, a
**corruption-prone** operation: the reconcile-time guard is check-then-act with a multi-second Helm
window it cannot fence, so an object relabeled from shard 3→7 mid-Helm can be picked up by pod 7 while
pod 3 is still upgrading its release.

**Safe K-change:** use rendezvous hashing (moves ~ΔK/K, not ~all); render + sync the new pods
**before** editing K in the policy (else new writes stamp buckets no pod selects); and **quiesce the
affected shards** (`--workers=0` / very long resync so no reconcile is in-flight) **during** the
relabel batch, then resume. This is an explicit pause→relabel→resume drain — which honestly confirms:
**growth forces a planned, quiesced migration.** Fixed-K buys zero *steady-state* handover, not zero
handover ever.

---

## 7. Residual risk (accept and document)

Even implemented correctly, fixed-K does **not** close the **involuntary** path. A replica in a
`>leaseDuration` GC/STW pause (or partitioned) can wake believing it still owns an object after work
was reassigned, and write concurrently with the new owner — the classic
[Kleppmann](https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html) lease-without-
fencing gap, shared by controller-runtime leader election and *every* lease scheme. Kubernetes
optimistic concurrency (`resourceVersion`) is only a **partial** fence: it rejects one stale object
write with a 409, but the paused owner then re-reads and can write again, and it **never guards the
`helm upgrade`** side effect at all.

For fixed-K this is bounded because: K never changes without an explicit quiesce; the §4.3 guard makes
reconcile shard-authoritative on the fresh read; and with a stateful Helm side effect the strongest
posture short of write-fencing is exactly this (single active owner per shard + loser-exits +
reconcile-time recheck). It should be **documented as accepted residual risk**, not papered over.

---

## 8. Rollout plan & effort

The ladder (each rung independently valuable; climb only as measured load forces you):

1. **Step 0 — per-Kind `spec.controller` (DONE, PRs #43/#74).** Makes `workers`/`resyncInterval`/
   `resources` per-Kind. Prerequisite for everything below.
2. **Tune + measure.** Set `spec.controller.workers` high on the hot Kind; confirm whether one pod
   keeps up. **Most "we need sharding" needs end here.**
3. **Fixed-K static sharding (this doc)** — build only if a pod saturates or blast-radius isolation is
   required. **Effort: Large**, spanning CDC + unstructured-runtime + core-provider + chart; the
   steady state is small, the migration + K-change machinery is the work and must be reviewed
   adversarially.
4. **Auto-rebalancing ring (future / north star)** — only if manual K management becomes the
   bottleneck; adopt/track Ebert's `kube-controller-sharding` rather than hand-rolling a ring.

A safe way to start §3 without the risky half: land the **CDC-side `-shard` selector + reconcile
guard** (inert until a shard label exists) first, then the admission stamping + backfill + fan-out as
a second, gated change.

---

## 9. Alternatives considered

- **Auto-rebalancing consistent-hash ring (Ebert `kube-controller-sharding`).** Per-shard ShardLeases
  + a sharder + a mutating webhook stamping shard labels + a **drain-label handover** (old owner acks
  by removing labels before reassignment). Correct handover for the *planned* rebalance path and
  generic across controllers — the north star. Rejected as the MVP: heavyweight (new controller, CRD,
  admission-hot-path webhook), and it still can't fence the crash path. Fixed-K captures most of the
  benefit for a self-owned, per-Kind controller with far less machinery.
- **Flux-style static labels, fully manual.** One Deployment per shard, operator-assigned labels, no
  hashing, no auto-rebalance. This design is essentially the automated-assignment version of it
  (admission stamps the label instead of a human), which is necessary at 60k.
- **StatefulSet ordinal + `hash mod N`.** Rejected: `--shard-total` baked into the pod template means
  a K change is a one-pod-at-a-time `RollingUpdate` running mixed N for minutes, and `mod N` reshuffles
  ~all keys — a guaranteed whole-population two-owner Helm-corruption window during the rollout.
- **Leader-election-for-failover (no sharding).** Complementary, not an alternative: it makes each
  shard (or the single CDC) individually HA (one active, standbys idle). Worth adding to the runtime
  regardless, but it buys availability, not throughput.

---

## 10. Open questions

- **K selection & surface.** Fixed cluster-wide default, or per-Kind `spec.controller.shards`? How is
  a good K chosen (target per-pod reconcile rate / blast-radius, *not* the memory model, which fits on
  one pod)?
- **`replicas` interaction.** Step 0 deliberately withholds `spec.controller.replicas` (unsafe >1
  without sharding/LE). Once sharding exists, does `replicas` become "replicas *per shard*" (each
  needing its own leader election for HA)? That reintroduces the fencing question per shard.
- **Hashing.** Rendezvous (HRW) vs consistent-hashing-with-bounded-loads (Argo's choice) — HRW is
  simpler and sufficient for fixed K; revisit if K churns.
- **Backfill ownership.** A core-provider-managed one-shot Job vs an operator-run tool; how is
  completion verified and cutover gated?
- **Metadata-only cache.** Not a drop-in mitigation here: the informer's `UpdateFunc` distinguishes
  spec-change vs status-only via a diff on the cached **spec**, so stripping spec breaks
  HighPriority-update detection. A status+managedFields-stripping transform is the safe cache-shrink
  (orthogonal to sharding).
- **Kubernetes version floor.** The admission-policy path already requires **K8s ≥ 1.36** (the
  `composition-version` policy); the shard stamp inherits that. KEP-5866 (server-side shard range)
  may later make the filtering cheaper/native.

---

## 11. References

- Companion artifact — *Controller Sharding & the Krateo CDC* (literature + the code/cluster-grounded
  analysis this doc distills): controller-runtime status, Ebert's `kube-controller-sharding`, Argo/Flux/
  Knative, the correctness core, the measured 60k case.
- controller-runtime maintainer position: kubernetes-sigs/controller-runtime #2576, #1456; abandoned
  PR #921. Server-side building block: KEP-5866 (sig-api-machinery).
- Reference research: `github.com/timebertt/kubernetes-controller-sharding`.
- Fencing: Kleppmann, *How to do distributed locking* (2016).
- Step 0 (prerequisite, shipped): core-provider PR #43 (`spec.controller`), chart PR #74.
- Related in-repo: `docs/design/composition-version-management.md` (the served-version /
  MutatingAdmissionPolicy model this builds on), `docs/design/multicluster-compositions.md`.
