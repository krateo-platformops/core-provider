---
type: Decision
title: "Design: automatic RBAC generation for apiRef RESTActions"
description: Auto-generate the Kubernetes RBAC that authorizes a CompositionDefinition's apiRef RESTAction reads, from snowplow's dispatch-free GET /rbac read-set.
resource: compositiondefinitions.core.krateo.io
tags: [design, rbac, apiref, snowplow]
status: implemented
timestamp: 2026-08-07T00:00:00Z
---

> **Status (re-verified 2026-08-07): implemented.** The engine consumes snowplow's
> `GET /rbac` via `go/core-provider/internal/tools/restactionrbac/` and writes the
> read-set RBAC in `internal/tools/deploy/restactionrbac.go` (422 → the
> `ApiRefRBACIncomplete` condition, never partial RBAC); the chart gates it with
> `apiRefRBAC.enabled` (default on) and provisions the engine's own identity
> (`templates/apiref-selfrbac.yaml`, runtime self allowlist mapping).

# Design: Automatic RBAC generation for `apiRef` RESTActions

> Status: **Draft for discussion** · Audience: core-provider + snowplow maintainers ·
> Scope: auto-generate the Kubernetes RBAC that authorizes a CompositionDefinition's `apiRef`
> RESTAction reads, the same way chart-inspector + rbacgen already auto-generate the CDC's
> chart RBAC. Written pre-migration: the forks it references are now the canonical `krateo-platformops` repos (the historical upstream org is dead).
>
> Goal: close the manual, silently-failing step where an operator must hand-author the RBAC for
> the per-composition group `krateo:cdc:<resource>-<apiVersion>` so that the resolved RESTAction
> can actually read the in-cluster objects it references.

---

## 0. TL;DR

1. **The gap.** When a CompositionDefinition declares `apiRef`, core-provider auto-provisions the
   *identity* (the projected token + the authn allowlist mapping → group
   `krateo:cdc:<resource>-<apiVersion>`), but **not** the *authorization*: the k8s RBAC that lets
   that group read the objects the RESTAction's `api[]` calls hit. Today that RBAC is
   **operator-authored** and, if missing, the projection **silently reads nothing**
   (`docs/gotchas.md`, `docs/how-to/apiref-status-projection-authn.md`).
2. **The proven pattern to copy.** For charts, core-provider already does exactly this:
   **chart-inspector** dry-run-renders the chart and returns the concrete resources
   (`[]Resource{Group,Version,Resource,Namespace}`); **rbacgen** turns each into a PolicyRule and
   binds it to the CDC SA (CDC `internal/chartinspector`, `internal/rbacgen`).
3. **The proposal (snowplow side now BUILT — PR #44).** A **dispatch-free** snowplow endpoint
   `GET /rbac` (snowplow already owns RESTAction resolution + discovery) returns the read-set the
   RESTAction's in-cluster stages need: `{restaction, readSet:[{group,version,resource,namespace,verb}]}`.
   core-provider consumes the `readSet` and builds a Role/ClusterRole **bound to the group**,
   alongside the authn mapping when `apiRef` is set, hashed into the digest and torn down on undeploy.
4. **The crucial difference from chart RBAC:** the generated RBAC binds to the **group**
   (`krateo:cdc:<resource>-<apiVersion>`), *not* the CDC ServiceAccount — because the reads run
   under snowplow's per-user clientconfig for that group, not the CDC SA (verified in snowplow
   `resolve.go:resolveStageEndpoint`).
5. **Per-row least-privilege verbs.** Unlike chart RBAC (generator hardcodes `["*"]`), snowplow
   emits the **verb per row** — `get` for a single object, `list` for a collection, `uaf.Verb` for a
   userAccessFilter stage — and core-provider grants exactly those. Precise least privilege, decided
   where the path is understood. Fail-loud is a `422` that refuses any partial read-set.

---

## 1. Where things are today (verified against code)

### 1.1 Chart RBAC is already auto-generated — the model

| Step | Code |
|---|---|
| **Inspect**: dry-run render the chart for this composition → concrete resources | CDC `internal/chartinspector/chartinspector.go` — `Resources(Parameters) ([]Resource, error)`; `Resource{Group,Version,Resource,Name,Namespace}` |
| **Generate**: one PolicyRule per resource, cluster- vs namespace-scoped | CDC `internal/rbacgen/rbacgen.go:52` — `APIGroups:[group], Resources:[resource], Verbs:["*"]`; ClusterRole if `Namespace==""`, else namespaced Role |
| **Bind**: to the **CDC ServiceAccount** | `rbac.InitClusterRoleBinding(..., r.saName, r.saNamespace)` |

The shape is **inspect (resolve concretely) → enumerate GVRs → grant, bound to an identity**.

### 1.2 `apiRef` provisions identity but not authorization

When `spec.apiRef` is set, core-provider (`internal/tools/deploy`) provisions:

- the projected `authn`-audience token on the CDC Deployment (`deployment.yaml`), and
- the authn allowlist **mapping** granting group `krateo:cdc:<resource>-<apiVersion>`
  (`authnmapping.go` — `cdcGroup(saName) = "krateo:cdc:"+saName`).

It does **not** author the read-RBAC. The `cdcGroup` doc comment states the contract:
*"standard Kubernetes RBAC bound to this group scopes what the resolved RESTAction may read"* —
and leaves authoring it to the operator.

### 1.3 How the RESTAction's reads actually authenticate (snowplow)

`snowplow internal/resolvers/restactions/api/resolve.go:resolveStageEndpoint` dispatches each
`api[]` stage one of three ways:

| Stage kind | Dispatch identity | Needs group RBAC? |
|---|---|---|
| `userAccessFilter` (UAF) | snowplow's **own SA** (cluster-wide read), then in-process re-filter by the caller's access | the **filter** consults the caller's access; dispatch does not |
| non-UAF, **no `endpointRef`** | the **per-user clientconfig** = the group `krateo:cdc:<…>` identity | **YES — this is the target of this design** |
| non-UAF, **`endpointRef` set** | the referenced **Endpoint Secret** (server-url + token) | No — the Secret's credential governs |

So the auto-generated RBAC must cover exactly the **non-UAF, `endpointRef`-less** stages.

### 1.4 The RESTAction `api[]` shape (snowplow `apis/templates/v1/core.go`)

```go
type API struct {
    Name        string      // call id
    Path        string      // request URI path — a ${ jq } template, e.g.
                            //   ${ "/api/v1/namespaces/\(.namespace)/services?labelSelector=…" }
    Verb        *string     // HTTP method (GET if nil)
    EndpointRef *Reference  // nil ⇒ in-cluster kube API; set ⇒ external/explicit Endpoint
    DependsOn   *Dependency // resolution-order chains (one call's path can depend on another's result)
    // … Headers, Payload, Filter, UserAccessFilter, ContinueOnError …
}
```

`Path` and `Verb` are exactly what a kube API request carries, so the GVR + verb are derivable —
**once the `${ jq }` path is evaluated**, the same way chart-inspector must *render* the chart
before it can enumerate resources.

---

## 2. Problem & goals

**Problem.** Declaring `apiRef` is a two-step onboarding: (1) deploy the CompositionDefinition,
(2) hand-author RBAC binding `krateo:cdc:<resource>-<apiVersion>` to the reads the RESTAction
performs. Step 2 is undiscoverable, easy to forget, and fails **silently** (empty projection),
exactly the failure chart RBAC was automated away years ago.

**Goals.**
- **G1.** When `apiRef` is declared, core-provider auto-generates the Role/ClusterRole authorizing
  the RESTAction's in-cluster reads, bound to the per-composition group.
- **G2.** Least privilege — emit only the verbs/resources the `api[]` stages actually need, not `*`.
- **G3.** Reuse the chart-inspector/rbacgen pattern and lifecycle (digest-hashed, undeploy-cleaned).
- **G4.** Fail **loud**, not silent: surface a condition when RBAC can't be fully derived.
- **G5.** Backward compatible: no `apiRef` ⇒ no change; an existing operator-authored binding is
  left untouched (create-if-absent), mirroring the remote-policy projection.

**Non-goals.** External (`endpointRef`) credentials — those are Endpoint Secrets, out of scope.
Authoring RBAC for `userAccessFilter` dispatch (snowplow-SA cluster read) — separate concern.
Changing snowplow's resolution semantics.

---

## 3. The model

**A new "resolve-for-RBAC" inspection, analogous to chart-inspector's `/resources`.**

snowplow already (a) resolves RESTActions, (b) evaluates the `${ jq }` paths against the
composition context, and (c) has a dynamic discovery client (`internal/dynamic/client.go:Discover`
→ `[]GroupVersionResource`). It is the natural home: it is the only component that can turn a
templated `api[]` path into a concrete GVR. So we add a **dry-run/inspect mode** that resolves the
RESTAction **without dispatching** and returns the required permissions.

```
core-provider (Deploy, when apiRef set)
        │  GET /rbac?apiRefName=…&apiRefNamespace=…&extras=<json>
        ▼
snowplow  ── for each api[] stage (dispatch-free) ──┐
        │   • UAF stage          → emit one row per plural, verb = uaf.Verb
        │   • EndpointRef stage   → external, omit
        │   • in-cluster GET      → jq-eval path → URI → discovery → emit row (get|list)
        └────────────────────────────────────────────┘
        │  200 ← { restaction, readSet: [ {group,version,resource,namespace?,name?,verb}, … ] }
        │  422 ← if ANY stage unresolvable (names them; refuses a partial read-set)
        ▼
core-provider  consume readSet → Role/ClusterRole bound to group krateo:cdc:<res>-<ver>
        │  apply (create-if-absent) · hash into status.digest · delete on Undeploy
```

### 3.1 The inspection contract — as BUILT in snowplow (PR #44)

> **Status: implemented.** snowplow ships `GET /rbac` (`internal/handlers/rbac.go`,
> `internal/resolvers/restactions/api/inspect.go`). The endpoint is **dispatch-free** and runs under
> snowplow's own SA. The contract below is what was built — it **diverges from chart-inspector's
> bare `[]Resource`** (it adds a per-row `verb` and a `{restaction, readSet}` wrapper), so
> core-provider does *not* reuse the chart-inspector decoder/`rbacgen` verbatim — but the divergence
> buys **precise per-row least-privilege** and an explicit fail-loud, which is the better trade.
> snowplow side: `snowplow/docs/restaction-rbac-endpoint-design.md`.

**`200` body** — a wrapper with a canonical (deduped, sorted) `readSet`; each row carries its verb:

```go
type rbacResponse struct {
    RESTAction restActionRef `json:"restaction"`   // { name, namespace }
    ReadSet    []Resource    `json:"readSet"`
}
type Resource struct {
    Group     string `json:"group"`
    Version   string `json:"version"`
    Resource  string `json:"resource"`             // plural, post-discovery
    Namespace string `json:"namespace,omitempty"`  // "" ⇒ cluster-scoped
    Name      string `json:"name,omitempty"`       // currently always "" (resource-granularity grant)
    Verb      string `json:"verb"`                  // per row — see §3.2
}
```

**Fail-loud = `422`.** If **any** stage can't be enumerated (residual `${…}`, upstream/`dependsOn`,
non-kube path), snowplow returns **`422` + an error string naming every unresolvable stage** and
**no** `readSet` — it refuses to return a partial read-set that would silently under-grant. core-
provider maps that `422` to the `ApiRefRBACIncomplete` condition. (`400` missing params, `500`
structural failure.) An empty `readSet` (every stage external/discovery) is a legitimate `200` `[]`.

**Request:** `GET /rbac?apiRefName=…&apiRefNamespace=…&extras=<url-encoded JSON>` with
`Authorization: Bearer <authn service JWT>` — `extras` is the same per-instance context the `/call`
path takes; snowplow loads the RESTAction fresh from the informer cache each call.

**Authentication (verified live).** `/rbac` is gated by the **same JWT middleware as `/call`** — an
unauthenticated call gets `401 "missing authorization header"`. So core-provider must authenticate
the way the CDC already does for `apiRef`: present its **own projected `authn`-audience SA token**
to authn's `/serviceaccount/login`, receive a service JWT, and `Bearer` it to `/rbac`
(`internal/tools/authn` mirrors the CDC's `internal/authn`; the JWT is cached to expiry). This
imposes two **deployment** prerequisites on core-provider's pod (chart, not code), the same two it
already provisions for the CDC: a **projected `authn`-audience token** mount, and core-provider's
ServiceAccount on **authn's allowlist** so the exchange is permitted.

### 3.2 What snowplow puts in each `readSet` row (verb included)

Unlike chart RBAC (verbs are the generator's `["*"]`), **the verb is decided by snowplow per row**
and core-provider consumes it as-is:

| `api[]` stage | Row(s) emitted |
|---|---|
| `userAccessFilter` (UAF) | **emitted** — one row per resolved plural, `verb = uaf.Verb` verbatim (the refilter consults the group's access, so it needs the grant) |
| `EndpointRef != nil` (non-UAF) | **omitted** — external; the Endpoint Secret governs, not RBAC |
| in-cluster, **single object** (`/…/services/<name>`) | `{gvr, namespace, verb:"get"}` |
| in-cluster, **collection** (`/…/services?labelSelector=…`) | `{gvr, namespace, verb:"list"}` — the dominant status-projection case |
| bare group-discovery path (`/apis/<g>/<v>`) | no row (anonymous-readable catalogue) |

> ⚠️ **Known gap to fix before merge (PR #44).** `inspectInClusterStage` currently hardcodes
> `verb:"get"` for *every* resource path — it discards the `name` that `cache.ParseAPIServerPathToDep`
> already returns. A **collection** GET is a Kubernetes **LIST** (RBAC verb `list`), so label-selector
> reads would be granted `get` and **403 at the first `/call`**. Fix: `verb := "list"; if name != "" { verb = "get" }`.
> The table above reflects the *intended* behaviour.

core-provider's generator therefore groups the rows **by `verb`** into `PolicyRule`s (resources
sharing a verb collapse into one rule), cluster- vs namespace-scoped by `Namespace`. This is a small
adaptation of `rbacgen` (which emits a single `["*"]` rule per resource), not a verbatim reuse.

### 3.3 Templated paths — what matters for RBAC is the GVR, not the object

`api[].Path` is an **arbitrary `${ jq }` program** evaluated against the resolved root (inline
`apiRef.extras` + per-instance context + prior `dependsOn` results). **Any segment can be
templated** — not just names: the namespace, the group/version/resource positions themselves, or
the whole path sourced from a variable or a previous call's result. String-parsing the template is
therefore wrong; the inspection must **evaluate the jq** (this is precisely why snowplow, not
core-provider, owns it — it has the resolver and the jq context).

The saving grace that makes this tractable: **RBAC is granted at
`(group, version, resource, verb, namespace)` granularity — never per object name or label
selector.** So most templating is *irrelevant* to RBAC:

| Templated segment | Matters for RBAC? | Handling |
|---|---|---|
| object **name**, **labelSelector**, query string | **No** — `list services` covers any name/selector | ignore |
| **namespace** | scope only | own-ns → namespaced Role; cross-ns / cluster-wide LIST → ClusterRole |
| **group / version / resource** (the GVR) | **Yes — this *is* the grant** | must evaluate (below) |
| whole path from a **variable / prior result** | Yes, and not statically knowable | fail-loud (§3.4) |

So the inspection evaluates the path only accurately enough to read off the **GVR + namespace** —
names and selectors are noise. **Where the GVR-determining values come from decides *when* RBAC can
be generated** (and is the crux of the design):

- **GVR from literals or static `apiRef.extras`** → fully resolvable at **CompositionDefinition
  deploy time** (core-provider), once per composition *type*. **This is the recommended scope** —
  the common case (literal `/api/v1/.../services`, only name/namespace/selector templated per
  instance) lands here.
- **GVR templated from per-instance context** (e.g. `\(.spec.targetKind)`) → resolvable only at
  **composition reconcile**, with the instance in hand — exactly when the CDC already calls snowplow
  to resolve the apiRef. The per-type group Role then accumulates the **union** across instances
  (§3.4, open Q §8.6).
- **GVR from a `dependsOn` result** → unknowable without dispatching → **fail-loud** (§3.4).

The inspection is strictly **dispatch-free** (evaluate path → discovery → GVR; never perform the
read), so it needs no pre-existing RBAC and breaks the chicken-and-egg of "resolution needs the
RBAC we're trying to derive from resolution."

### 3.4 Failure modes — fail loud (G4)

- **`dependsOn` chains**: a later stage's path can interpolate an earlier stage's *result*, which
  isn't available without dispatching. → derive what is statically resolvable; for the rest, emit a
  **condition** on the CompositionDefinition (`ApiRefRBACIncomplete`) listing the unresolved stages,
  rather than silently under-permissioning.
- **GVR templated from per-instance data**: not resolvable at deploy time. → either **constrain**
  (recommended scope §3.3: require GVR-determining segments to come from literals / static
  `apiRef.extras`) and emit `ApiRefRBACIncomplete` if violated, or **defer to reconcile-time union**
  (open Q §8.6): the CDC merges each instance's resolved GVR into the per-type group Role.
- **Unparseable / non-kube path**: skip + record (likely an external call missing its `endpointRef`).
- **Discovery miss** (GVR not served): record; do not invent a grant.

The policy is **never silently broad and never silently empty** — the inverse of today's failure.

### 3.5 The inspection identity is NOT the granted principal (chart-inspector precedent)

A subtlety worth stating explicitly, because chart-inspector already establishes it: **the identity
that performs the inspection is distinct from the identity the generated RBAC is *for*.**

chart-inspector renders under **its own ServiceAccount** — `rest.InClusterConfig()` (or an explicit
`--kubeconfig`), set once at startup (`chart-inspector main.go`). The caller (CDC) sends **no token**;
the handler does **no impersonation and reads no `Authorization` header**; the composition identity
in the query string is **input data** (which composition to render), not an auth identity. Under
that single SA it `GET`s the CompositionDefinition and **server-dry-run installs** the chart
(`DryRun: DryRunServer`) to enumerate resources. The RBAC it informs is then written by core-provider
**for a different principal — the CDC ServiceAccount.**

The RESTAction design inherits the same separation:

| | Inspection identity (who resolves) | Granted principal (who the RBAC is for) |
|---|---|---|
| Chart (today) | chart-inspector's own SA — central, trusted oracle | the **CDC ServiceAccount** |
| RESTAction (this design) | snowplow's own SA — **dispatch-free** resolution + discovery | the **group** `krateo:cdc:<resource>-<apiVersion>` |

So snowplow's `/rbac` endpoint resolves under snowplow's own identity (it already holds the
discovery client and the resolver), and **never needs the group's permissions to compute them** —
it derives the GVRs without performing the reads (§3.3). This is what lets RBAC be generated
*before* the first successful resolution, exactly as chart RBAC is generated before the CDC ever
manages a chart resource.

---

## 4. Where it slots into core-provider

Mirror the **authn-mapping** lifecycle exactly (`internal/tools/deploy/authnmapping.go`), which is
already apiRef-conditional, digest-hashed, and undeploy-cleaned:

**Config (implemented).** `CORE_PROVIDER_SNOWPLOW_URL` (snowplow base URL) and
`CORE_PROVIDER_AUTHN_URL` (authn base URL, for the JWT exchange — §3.1), both env-driven package
vars plumbed into `DeployOptions` (mirroring `COMPOSITION_AUTHN_NAMESPACE`). Plus the two **chart**
prerequisites on core-provider's own pod: a projected `authn`-audience token mount and an authn
allowlist mapping for core-provider's ServiceAccount.

**Timing (decided by §3.3).** For the recommended scope — GVR resolvable from literals / static
`apiRef.extras` — generation is **deploy-time, in core-provider, once per type**, as below. Only
when a RESTAction templates its *GVR* from per-instance data does generation need to move to the
**CDC at reconcile** (union into the group Role, like chart RBAC via chart-inspector); that path is
deferred (open Q §8.6). Either way the inspection is dispatch-free and the subject is the group.

1. In `Deploy` (`deploy.go`), **when `apiRef` is set** and after the authn mapping:
   `GET snowplow /rbac?apiRefName=…&apiRefNamespace=…&extras=…` (static `apiRef.extras` context).
   On `200`, decode the `{restaction, readSet}` body; on `422`, set the `ApiRefRBACIncomplete`
   condition and stop (don't write partial RBAC).
2. Turn `readSet` into RBAC: **group the rows by `verb`** into `PolicyRule`s (resources sharing a
   verb → one rule), cluster-scoped (`ClusterRole`) when `Namespace==""` else a namespaced `Role`,
   and bind the **subject to the group** `krateo:cdc:<resource>-<apiVersion>` (Kind: Group). This is
   a small adaptation of `rbacgen` (which emits one `["*"]` rule per resource bound to the SA) — it
   reads the per-row verb instead of hardcoding, and binds a Group instead of the SA.
3. **Apply create-if-absent** (leave an operator's hand-authored binding untouched, per G5 and the
   remote-policy-projection precedent), and **hash into `status.digest`** so Observe/Lookup agree
   and a RESTAction change re-renders.
4. **Undeploy** removes it, tolerating not-found (as `authnmapping` does, `deploy.go`).

**Watch the referenced RESTAction.** `apiRef` points at a RESTAction object core-provider doesn't
currently watch. Add a watch (like the existing Secret / KubernetesTarget watches,
`compositiondefinitions.go`) so editing the RESTAction's `api[]` re-reconciles and regenerates RBAC
promptly — otherwise the grant drifts from the calls.

---

## 5. Security considerations

- **This grants privileges.** Auto-generating RBAC from an author-supplied RESTAction means
  core-provider escalates the group's access to whatever the RESTAction declares. The **same
  privilege-escalation guardrails as chart RBAC apply**: core-provider's own identity needs
  `bind`/`escalate` on `rbac.authorization.k8s.io` to create a Role exceeding its own
  (`docs/how-to/remote-target-credentials.md` already documents this for the remote case).
- **Bound to reads.** Defaulting verbs to `get`/`list`/`watch` (and flagging any write) keeps the
  blast radius to read-only, which is the status-projection contract.
- **Bounding policy (open Q §8).** Consider an allowlist of grantable groups/resources, or
  requiring `apiRef.extras`-declared intent, so a hostile RESTAction can't request
  `secrets`/`*` cluster-wide and have core-provider rubber-stamp it.
- **Per-composition isolation is preserved**: the grant targets the single group
  `krateo:cdc:<resource>-<apiVersion>`, so one composition type's RESTAction RBAC never widens
  another's — exactly the isolation the owner-scoped identity already gives.
- **Trust concentration in the inspector (§3.5) — access-controlled (verified live).** The
  inspection runs under snowplow's own identity and resolves *any* RESTAction — the same
  central-oracle trust chart-inspector carries. That makes `/rbac` a privileged surface, so it
  **must be access-controlled**, and it **is**: verified against the deployed endpoint, `/rbac` is
  gated by the same JWT middleware as `/call` (an unauthenticated call returns `401`). core-provider
  authenticates with an authn-issued service JWT (§3.1). So the endpoint does *not* inherit an
  unauthenticated posture — the earlier "unverified" caveat is closed. (chart-inspector's
  `/resources`, by contrast, is called with no token; its server-side auth posture remains its own
  concern, separate from `/rbac`.)

---

## 6. Phased plan

1. **Phase 0 — snowplow `GET /rbac` (inspect-only). ✅ DONE (PR #44).** Emits
   `{restaction, readSet:[{…,verb}]}`; UAF emitted, `endpointRef` omitted; dispatch-free; `422` on
   unresolvable. **Pre-merge fix:** the in-cluster verb is hardcoded `get` — must be `list` for
   collection paths (§3.2 ⚠️).
2. **Phase 1 — core-provider generation.** Adapt `rbacgen` to consume the `readSet` (group rows by
   `verb` → `PolicyRule`s; **Group** subject) and handle the `422` → `ApiRefRBACIncomplete`. Wire into
   `Deploy`/`Undeploy` alongside `authnmapping`; group-bound Role/ClusterRole; digest-hash;
   create-if-absent.
3. **Phase 2 — RESTAction watch + condition.** Watch the referenced RESTAction; surface
   `ApiRefRBACIncomplete` for unresolved (`dependsOn`) stages.
4. **Phase 3 — least-privilege hardening + bounding policy.** Narrow verbs; optional grantable
   allowlist; metrics on generated grants.

Phase 0+1 deliver the headline (no more manual binding for the common single-stage in-cluster read).

---

## 7. Alternatives considered

- **Generate from static path parsing only (no jq eval).** Fails on templated namespaces and any
  computed path; under-permissions silently. Rejected — must resolve with context, as chart-inspector
  renders with values.
- **A separate `restaction-inspector` service** (true chart-inspector twin). Clean symmetry, but it
  would have to re-implement snowplow's resolver + discovery. Rejected in favour of a snowplow
  endpoint that reuses them.
- **Byte-identical chart-inspector `[]Resource` (verb-less, reuse `rbacgen` verbatim).** Considered
  and initially chosen, but the built endpoint (PR #44) **deliberately added a per-row `verb` and a
  `{restaction, readSet}` wrapper** — the small loss of verbatim reuse buys *precise* per-row
  least-privilege (`get` vs `list` vs `uaf.Verb`) and an explicit `422` fail-loud. Net better; the
  generator adapts `rbacgen` rather than reusing it unchanged.
- **core-provider parses the RESTAction itself.** core-provider has no RESTAction resolver, no jq
  context, no discovery against the target. Rejected.
- **Keep it manual but validate.** Only detect a missing binding and warn (no generation). Cheaper,
  but leaves the toil; a useful *fallback* (the §3.4 condition) rather than the goal.
- **Grant `verbs:["*"]` like chart RBAC.** Simplest (zero generator change), but RESTAction reads are
  read-only and author-supplied. Rejected for **per-row verbs from snowplow** (PR #44): the inspector
  knows whether each stage is a `get`/`list`/`uaf.Verb`, so it emits the exact verb and core-provider
  grants precisely that — tighter than both `*` and a uniform read set.

---

## 8. Open questions

1. **UAF stages. ✅ RESOLVED (PR #44).** The build **emits** UAF rows (one per plural, `uaf.Verb`
   verbatim) — the in-process re-filter does consult the group's access, so the grant is required.
   No longer open.
2. **`dependsOn` resolvability.** How much of a chained path can be resolved at inspect time? Define
   the boundary between "resolved → grant" and "unresolved → condition".
3. **Namespace scope.** When a path's namespace is the composition's own, a namespaced Role suffices;
   when it's templated to something else (or a label-selector LIST cluster-wide), is a ClusterRole
   acceptable, or should it require explicit author intent?
4. **Bounding hostile RESTActions** (§5) — allowlist, intent declaration, or operator approval gate?
5. **Multi-cluster.** For remote targets the reads run target-side; the group RBAC must be created in
   the **target** (like the version policy projection), and snowplow/authn must resolve there —
   ties into the deferred remote-`apiRef` work (`composition-status-projection.md` §11 / `multicluster-compositions.md`).
6. **Per-instance-GVR union & GC.** If a RESTAction templates its GVR from per-instance data
   (§3.3), the per-type group Role is the union of what all live instances need. Accumulate-only is
   safe but leaky (a removed instance's grant lingers); precise GC needs tracking which instance
   contributed which rule. Decide: constrain GVRs to static context (no union needed) vs. support
   reconcile-time union with reference-counted cleanup.
7. **Drift vs digest churn.** Watching the RESTAction regenerates RBAC on every edit; ensure the
   permission set is canonicalised (sorted) so an unchanged RESTAction doesn't flip the digest.
