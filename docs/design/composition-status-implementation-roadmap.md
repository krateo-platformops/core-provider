---
type: Decision
title: "Roadmap: composition status projection implementation"
description: The two-track sequencing plan (projection vs intra-service auth) for the composition status projection design.
resource: compositiondefinitions.core.krateo.io
tags: [design, roadmap, archive]
status: superseded-by:composition-status-projection.md
timestamp: 2026-08-07T00:00:00Z
---

> **Status (re-verified 2026-08-07): superseded — both tracks shipped.** Track A
> (`statusDataTemplate`) and Track B (authn serviceaccount strategy + `apiRef`)
> are live; the sequencing this doc plans is done. Kept as the record of how the
> work was cut.

# Implementation roadmap — Composition status projection

> Sequencing for the two design docs:
> - [`composition-status-projection.md`](./composition-status-projection.md) (core-provider)
> - [`authn/docs/design/kubernetes-intra-service-auth.md`](https://github.com/krateo-platformops/authn) (authn)
>
> Written pre-migration: the forks it references are now the canonical `krateo-platformops` repos (the historical upstream org is dead).

---

## 0. Shape of the work

Two independent value tracks that meet only at Phase 2:

- **Track A — projection** (the headline): declarative `statusDataTemplate` over **built-in
  sources** (`self`/`helm`). No new I/O, no new RBAC, no snowplow/authn. **Ships standalone.**
- **Track B — intra-service auth** (a platform capability): authn `serviceaccount` strategy.
  Independently useful; it is the **prerequisite** that lets Track A reach `apiRef`/RESTAction
  sources (Phase 2).

The critical insight for sequencing: **most of the value (Track A) has zero dependency on
the hard part (Track B).** Build A first; B unblocks the external/API extension later.

```
            ┌─────────────────────────────── Track A (projection) ───────────────────────────────┐
 P0 engine ─┬─► P1b CDC built-ins ─► (M1 ships) ─────────────────────────► P2 apiRef ─► P3 rollup/baseline
 (u-runtime)│      ▲                                                          ▲
            └─► P1c RDC converge     P1a core-provider API+schema ────────────┘
                                                                              │
            ┌──────── Track B (auth) ────────┐                                │
            └─► authn serviceaccount strategy ──────────────────────────────┘ (prereq for P2)
```

---

## 1. Workstreams

| # | Repo | Deliverable | Depends on | Unlocks / ships |
|---|---|---|---|---|
| **P0** | `unstructured-runtime` | `pkg/tools/statusprojection` — `Project(cr, resolved, mappings)` over `plumbing/jqutil` (`MaybeQuery`/`Eval`/`InferType`), output normalization, `SetObservedGeneration`. Unit tests; **tag a release**. | `plumbing/jqutil` (have) | P1b, P1c |
| **P1a** | `core-provider` | `CompositionDefinition.spec.statusDataTemplate` (+ `apiRef`/`ApiReference`); generate status-schema properties (precedence: `Schema`→`PreserveUnknownFields`→`Type`→simple-path inference→string); admission validation; ship mappings to the CDC via the existing **CDC ConfigMap**. (`apiRef` parsed but inert until P2.) | — (parallel with P0) | P1b |
| **P1b** | `composition-dynamic-controller` | Bump `unstructured-runtime`; call `Project` + `SetObservedGeneration` with `self`/`helm`; e2e (scalar/derived/object/array). | P0, P1a | **M1** |
| **P1c** | `rest-dynamic-controller` | Replace `populateStatusFields` with `Project` (`resolved["response"]=body`). **Coordinate with in-flight #40/#42** (jq makes #42's type work moot). | P0 | **M2** |
| **B1** | `authn` | `serviceaccount` login strategy — `ServiceAccount` CRD (`serviceaccount.authn.krateo.io`, = `basic.User` + `serviceAccountRef`); `TokenReview` validation; map → `jwtutil.CreateToken` + `signup.Do` (reused). authn SA gains `create tokenreviews`. | — (parallel) | **M3**, P2 |
| **B2** | `plumbing` *(optional, parallel)* | Lift snowplow's `apiRef`/`widgetDataTemplate` types + jqutil resolver into a shared module so snowplow/CDC/RDC share one impl. Reconcile snowplow `plumbing v0.6.2` vs forks' `v1.7.6`. | — | de-dups P2 |
| **P2** | `core-provider` + `composition-dynamic-controller` | CDC `internal/snowplow` client (copy of `internal/chartinspector`: `URL_SNOWPLOW` via ConfigMap, sync call in reconcile, composition identity as **request-extras**); authenticate via **authn SA-token exchange** (B1); project `.api`. **Local compositions only.** | **B1**, P1b, P1a | **M4** |
| **P3** | `composition-dynamic-controller` | kstatus readiness rollup (or a kube-API RESTAction rollup) + Helm `revision/state/lastDeployed` baseline fields. | P1b (rollup-via-apiRef also needs P2) | **M5** |
| **P4** | *(future)* | Remote-target **external-API** `apiRef`: target-local resolution (project resolver + authn into target) — built-ins + target-local readiness already work without it. | P2, multi-cluster | — |

> **snowplow: no change to `/call`.** It is reused as-is once the caller holds an
> authn-issued service JWT (B1). snowplow work is only the optional shared-types donation (B2).

---

## 2. Milestones (shippable increments)

- **M1 — Declarative status from built-ins.** P0 + P1a + P1b. `statusDataTemplate` over
  `self`/`helm` + `observedGeneration`. Derived URLs, IDs, chart metadata, hand-written
  readiness. **No I/O, no new RBAC, no snowplow/authn. Works for local AND remote targets.**
  *This is the headline capability and the main goal — everything after is extension.*
- **M2 — RDC convergence.** P1c. One engine across CDC + RDC; retires RDC's bespoke
  `populateStatusFields`/#40/#42.
- **M3 — Kubernetes intra-service auth.** B1. Any Krateo service authenticates with its SA
  token → scoped service JWT. Platform capability, valuable beyond this feature.
- **M4 — `apiRef` (local).** P2 (needs M3). In-cluster reads + external APIs via RESTAction,
  resolved by snowplow under an authn-issued scoped identity. CDC holds no credentials.
- **M5 — Readiness rollup + Helm baseline.** P3. Real `Ready` from managed-object health
  (target-local for remote targets); release metadata as baseline fields.

---

## 3. Critical path & parallelism

- **Critical path to M1 (the goal):** `P0 → P1a → P1b`. P0 and P1a run **in parallel** (the
  engine and the API/schema are independent); P1b joins them. Nothing here depends on authn,
  snowplow, or multi-cluster.
- **Parallelizable from day one:** B1 (authn) and B2 (plumbing shared types) have no
  dependency on Track A and can start immediately. B1 must land before P2.
- **P1c (RDC)** only needs P0 — can follow the engine independently of the core-provider
  work; gate the timing on the #40/#42 branches.
- **P2** is the first point both tracks meet: it needs **B1 (auth)** + **P1b (CDC populate)**
  + **P1a (apiRef config)**.

**Recommended order:** start **P0 + P1a + B1** concurrently → **P1b** (⇒ M1, the win) →
**P1c** (M2) and finish **B1** (M3) → **P2** (M4) → **P3** (M5). P4 (remote external-API)
is post-MVP.

---

## 4. Risks & notes

- **#40/#42 coordination (P1c).** RDC is already growing a bespoke populate loop on the fork.
  Decide: let them merge then refactor onto `Project`, or land the engine first. Either way
  #42's hand-rolled type conversion is *dropped* (jq/`InferType` gives it for free), not ported.
- **Engine dependency boundary (P0).** `statusprojection` importing `plumbing/jqutil` directly
  vs. an injected evaluator (keeps `unstructured-runtime` lighter). Decide at P0.
- **Status churn guardrail (P1a/P1b).** Arbitrary jq into `.status` can bloat objects / hot-loop
  reconciles. Bound projected output (size/depth) and discourage projecting volatile fields.
- **Security bounding lives in authn (B1).** Per-service least privilege = the
  `ServiceAccount` CR's `groups` → standard k8s RBAC. The exchange allowlist = the CR's
  existence. No RBAC authoring in the CDC or snowplow.
- **Multi-cluster (P4).** Resolution is cluster-local; remote targets get built-ins +
  target-local readiness now. Only remote external-API `apiRef` waits on target-local
  resolution (resolver + authn projected into the target), with a mgmt-side `KubernetesTarget`
  reach as fallback.
- **Backward compatibility.** No `statusDataTemplate` ⇒ status identical to today (M1 is
  additive). `apiRef` inert until P2.
