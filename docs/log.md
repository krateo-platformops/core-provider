---
type: Log
title: core-provider — log
description: Curated chronological history of core-provider — notable changes, decisions and incidents; release notes stay in GitHub Releases.
resource: oci://ghcr.io/krateo-platformops/charts/core-provider
tags: [history, changelog]
timestamp: 2026-08-07T00:00:00Z
---

# Log

Curated history — the moves that shaped the component, newest last within each era.
Release notes stay in GitHub Releases; the design record behind most entries is
under [docs/design/](./design).

## Origins

core-provider began in the upstream Krateo org as the KCO engine, with the
composition-dynamic-controller, chart-inspector and the Helm chart as **separate
repos**. The consolidation audit
([archive](./audit/core-provider-consolidation-plan.md)) mapped fork reality against
the upstream roadmap and set the direction for everything below.

## 2.0.0 — de-webhooked + multicluster (2026-06-14)

- Dropped all admission/conversion webhooks: generated CRDs use `None` conversion;
  the `krateo.io/composition-version` label moved to an in-apiserver
  `MutatingAdmissionPolicy` → **Kubernetes >= 1.36 floor**
  ([design](./design/multicluster-compositions.md), conversion note).
- **Local + remote deployment**: `spec.deploy.targetRef` → `KubernetesTarget` →
  kubeconfig Secret; controller projection puts the generated CRD + RBAC + CDC on
  the spoke while CR/secrets/status stay on the hub.

## 2.3.0 — composition status projection (2026-06-20)

- `spec.statusDataTemplate` (`${ jq }` mappings under `.status`) + `spec.apiRef`
  (RESTAction resolved via snowplow, exposed as `.api`), with status-schema
  injection and the authn allowlist handshake
  ([design](./design/composition-status-projection.md)).

## The remote line — Path A to the mirror (2026-07)

- **Path A self-seeding** (#32–#34): the projected CDC made self-sufficient on the
  spoke — shadow definition, chart-inspector projection, the admission policy,
  reachability + ref-counted teardown
  ([design](./design/remote-projection-self-seeding.md)).
- **KubernetesTarget grew a controller** (#35): periodic reachability probing,
  status (Healthy/Down, k8s version, credential resourceVersion); accepted
  token+server(+ca.crt) Secret shapes for ESO-minted SA tokens (#36); full spoke
  teardown on delete (#37).
- **KubernetesTarget migrated cluster-scoped → namespaced** (2026-07-14) — resolved
  in the referencing object's own namespace, creatable through snowplow's
  force-namespaced write path.
- **Remote composition mirror** (#49–#55, 2026-07-21/24): hub `Composition`s
  mirrored onto spokes (spec down, status up, per-instance fan-out via
  `krateo.io/target`, ordered cross-cluster teardown)
  ([design](./design/remote-composition-mirror.md)).
- **`RemoteInstall` retired** (#56, 2026-07-22) — it was a shim over the mirror
  model ([migration how-to](./how-to/migrate-off-remoteinstall.md)).

## Version management & RBAC automation (2026-07)

- **`spec.upgradePolicy`** (Automatic/Manual/Paused, 2026-07-10): per-definition
  control of instance migration on chart-version bumps, on top of the multi-version
  CRD + `vacuum`-storage model
  ([design](./design/composition-version-management.md)).
- **`apiRef` RBAC auto-generation**: the read-set of the referenced RESTAction
  (snowplow `GET /rbac`) is turned into RBAC for the per-composition group —
  closing the silently-failing manual grant step
  ([design](./design/apiref-rbac-generation.md)).

## Independence + monorepo fold (2026-08-03)

- Go module identity migrated to `krateo-platformops` (full independence from the
  upstream org).
- **Engine fold**: core-provider, composition-dynamic-controller and
  chart-inspector became one monorepo (`go/` modules, `helm/`, 3-image build
  matrix); CI made monorepo-aware (#64); the cross-repo CRD publish job dropped
  (#65); `global.imageRegistry` added for air-gapped installs.

## The version-lock era (2.12.x, 2026-08)

- **Observe made side-effect-free** (#67): the external-create recovery self-heals
  instead of wedging.
- **CDC + chart-inspector images track `appVersion`** (#68): a CDC fix built by a
  release now deploys with that release — the 2.12.3-chart-shipping-cdc-2.12.2 gap
  cannot recur.
- 2026-08-07: adopted the Krateo Documentation Standard (this bundle); the legacy
  `docs/{architecture,behavior,gotchas}.md` re-verified against the monorepo tree
  and moved under `docs/internals/`.
