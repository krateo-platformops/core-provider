---
type: Decision
title: "Design: composition controllers live in the management namespace, not the namespace they finalize"
description: Fixes #313's namespace-deletion deadlock by deploying every per-version cdc into core-provider's own namespace (and a KubernetesTarget's controllerNamespace on a spoke), with a stop-before-start migration.
resource: ghcr.io/krateo-platformops/composition-dynamic-controller
tags: [design, cdc, namespace, finalizer, migration, multicluster]
status: approved
timestamp: 2026-11-08T00:00:00Z
---

# Design: composition controllers live in the management namespace

**Status:** approved (Diego, 2026-11-08). The direction, the migration order and the spoke field were decided by Diego
on 2026-11-08 (#313). This document records them in one place and settles the rest from the code.

## 1. The defect

A per-version composition-dynamic-controller (cdc) is deployed into its CompositionDefinition's own
namespace. On main, seven `DeployOptions`/`UndeployOptions` literals pass `Namespace: cr.Namespace`
(five in `compositiondefinitions.go`, one each in `retire.go` and `controllerowner.go`), plus
`currentControllerNamespace` (`retire.go:71`). An earlier count of four was a grep that missed three
and counted `ResubjectOptions`, a different struct. The implementation gives all of them one function
that chooses the controller's namespace, so no site can keep the old rule. That
cdc is the only thing that removes `composition.krateo.io/finalizer` from its compositions. Deleting
the namespace deletes the cdc with everything else, so the compositions' finalizers are left with
nobody to clear them. Their CompositionDefinitions wait for them forever, reporting
`waiting for composition deletion`.

core-provider already refuses to remove a cdc while compositions exist (`deploy.go`,
`ErrCompositionStillExist`). But that ordering is enforced only on paths that go through
core-provider, and a namespace deletion goes through the namespace controller.

**Observed:** a0-openstack-d4 (#313's body: 35 definitions, 5 compositions, 0 pods, no progress), and
krateo-057 on 2026-11-05. There, deleting `demo-system` killed `eventsites-v0-1-0-controller` and
`meetupsites-v0-3-0-controller` at 06:43:26Z. `event-site` and `meetup-site` then waited until Diego
cleared the finalizers by hand, about 2h06m later. While `demo-system` was terminating, no
Kubernetes event could be recorded in it, so the incident left none.

## 2. The rule

**A controller that removes a finalizer must outlive the namespace whose objects carry it.**

- **Hub (local definitions):** every per-version cdc, with its ServiceAccount, ConfigMaps and
  Service, lives in the **management namespace**. That is the namespace core-provider itself runs
  in, passed explicitly by the chart through the downward API (`metadata.namespace`) as
  `CDC_NAMESPACE`. Today core-provider knows its namespace only implicitly, through
  controller-runtime's in-cluster fallback for leader election.
- **Spoke (remote definitions):** the cdc lives in `KubernetesTarget.spec.controllerNamespace`, a new
  optional DNS-1123 label defaulting to `krateo-system`. core-provider creates it on the spoke. A
  spoke that already runs its own Krateo sets another name to keep the two installations apart.

## 3. What follows from the code

### 3.1 Naming does not change, and cannot collide

There is exactly one cdc per Kind × version cluster-wide, owned by the oldest live local definition.
Followers deploy nothing (`controllerowner.go:30–49`). So `<resource>-<version>-controller` is already
unique among local controllers, and stays unique in one namespace.

**Ownership handover stops moving the controller.** Today, when the owner changes namespace (owner
deleted, next-oldest takes over), the cdc is retired in one namespace and redeployed in another. That
retire-and-redeploy is the leak surface of #327 and #385. In the management namespace, the
controller's name and location don't depend on which definition owns it.

### 3.2 RBAC

The cdc's RBAC is mostly cluster-scoped already (`assets/cdc/rbac/clusterrole.yaml`,
`clusterrolebinding.yaml`). Only the subject's namespace changes. The namespaced pieces,
`compositiondefinition-role` and `secret-role` (the passwordRef Secret), stay in the namespace whose
objects they grant. Their RoleBinding's subject becomes the ServiceAccount in the management
namespace, since RoleBinding subjects may name another namespace. `resubject.go`'s rewrite on a
version change follows the same subject.

### 3.3 Identity (authn, snowplow)

Settled with both owners, from their code (#313 comments):

- **Authorization is by group** `krateo:cdc:<saName>`, which carries no namespace. core-provider's
  RESTAction RBAC binds the group (`restactionrbac.go:139`), so no permission changes.
- **The authn mapping name is the identity:** the JWT username, the certificate CN, and the
  `<username>-clientconfig` storage key. It becomes `cdc-<managementNamespace>-<saName>`, derived from
  the ServiceAccount's new home so a handover never mints a new identity. Each identity changes exactly
  once, at migration.
- **Exactly one mapping per ServiceAccount**, or authn answers 403 `AmbiguousMapping`. The old mapping
  references the old SA and the new one the new SA, so the two never collide.
- **snowplow** takes the username from the verified JWT on every request and keys its caches by
  binding IDs, not names. Old and new identities share cells. Audit records show one controller
  under two names for the overlap window.

### 3.4 What stays where it is

Compositions, their helm releases and the definitions themselves don't move. The generated CRD is
cluster-scoped. Only the controller's own objects move.

## 4. Migration: stop-before-start

The cdc has **no leader election**: `LeaderElect` appears in 0 files under
`go/composition-dynamic-controller` (controls: 1 hit for `WatchCRDsAndInvalidate` in its main.go, 5
for `LeaderElect` in core-provider's). Two copies of one controller would both reconcile, so Diego
chose stop-before-start. For each local cdc found outside the management namespace:

1. **Scale the old Deployment to 0 and wait until it has no pods.** Events queue and nothing is lost.
   A deletion waiting on finalizers waits seconds longer.
2. **Create the new Deployment** and its objects in the management namespace, under the new authn
   mapping.
3. **Delete the old Deployment, its ServiceAccount, its namespaced objects and its old authn mapping.**
4. **Only then delete `cdc-<oldNs>-<sa>-clientconfig`.** Deleting it earlier makes any holder of the
   old JWT get 401 `session_credential_missing` for up to 24h (snowplow `userconfig.go:325`). Tell
   authn first, so its backfill counts don't read as a regression.

The migration is idempotent and resumable: each step is decided from what exists, not from a
recorded phase, so a core-provider restart mid-migration continues it. `reclaimleaked.go` must
recognise a controller in the definition's namespace as **migrating**, not leaked, so the two paths
can't race.

A spoke migrates the same way, against its target's `controllerNamespace`.

## 5. Input states the implementation must cover

| state | expected |
|---|---|
| fresh install | cdc created in the management namespace; never in the definition's namespace |
| existing cdc in the definition's namespace | migrated by §4; exactly one cdc for the Kind × version at every instant |
| core-provider restarted between §4 steps 1 and 2 | resumes; no second copy, no permanent zero |
| ownership handover (owner deleted, a sibling in another namespace takes over) | no controller change at all |
| definition's namespace deleted, compositions present | cdc survives, finalizes the compositions, namespace finishes deleting with **no manual finalizer removal** |
| definition deleted normally | today's order: compositions first, then the cdc (`ErrCompositionStillExist`) |
| remote target, `controllerNamespace` unset | spoke cdc in `krateo-system` on the spoke |
| remote target, `controllerNamespace: krateo-remote` | spoke cdc in `krateo-remote`; nothing in `krateo-system` |
| definition with an apiRef | new authn mapping only after the old SA is gone; never two mappings for one SA |

## 6. How it is proven

- **Kind integration test for the incident itself:** delete a namespace holding a definition and its
  compositions. Assert the namespace is gone within a bound, with no finalizer removed by the test.
  Against today's main it must fail (the control).
- **Migration test:** start from a cdc in the definition's namespace and upgrade. Assert that at no
  sampled instant do two cdc pods for one Kind × version run, and that the old clientconfig Secret
  outlives the old Deployment.
- **Handover test:** delete the owning definition while a sibling in another namespace remains.
  Assert the cdc Deployment's UID is unchanged.
- **Unit tests** for the naming, the subject rewrite and the spoke default.

## 7. Related, and not in scope

- **#396:** the "waiting for composition deletion" error. Once this lands, a wait with no available
  cdc is still the #313 shape and must stay loud.
- **The cdc pool** (`cdc-pooling.md`): a pool will live in the management namespace too. Its step 2
  (the ownership guard) would make a future start-before-stop migration possible. It isn't needed
  here.
- **#394:** the stale remote-seed CRD copy, separate.
