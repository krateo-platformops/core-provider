---
type: Usage
title: core-provider — values-schema inference
description: What happens to a chart that ships no values.schema.json, how a CompositionDefinition opts in to an inferred schema (krateo.io/values-schema: infer) or uses the values-schema CLI instead, the inference rules (v1), how to tell a schema was inferred, and the known limitations.
resource: https://github.com/krateo-platformops/core-provider/issues/568
tags: [values-schema, inference, compositiondefinition]
timestamp: 2026-11-22T00:00:00Z
---

# Values-schema inference

core-provider types a composition CRD from the chart's root `values.schema.json`. This page
covers charts that ship **no** such file: what happens by default, how to opt in to an
inferred schema, the inference rules, how to tell a schema was inferred, and what inference
cannot do. Design and decisions: [#568](https://github.com/krateo-platformops/core-provider/issues/568).

## When it applies

- **Default: refused.** A chart with no root `values.schema.json` reads
  `Ready=False/ValuesSchemaMissing`. Nothing is applied, and the message names the chart and the
  opt-in: "…has no values.schema.json at its root; core-provider needs one to type the
  composition CRD (a wrapper chart can add one, or annotate the definition
  krateo.io/values-schema: infer to infer one from values.yaml)…".[^missing]
- **Opt in** by annotating the **CompositionDefinition** (not the chart):

  ```yaml
  metadata:
    annotations:
      krateo.io/values-schema: infer
  ```

  The schema is then inferred from the chart's `values.yaml` files. Available from
  core-provider **2.13.117**.[^optin]
- **An authored schema always wins.** If the chart ships `values.schema.json`, it's used and the
  annotation is ignored.[^specschema]
- **Any other annotation value is not an opt-in.** The definition stays `ValuesSchemaMissing`,
  and the message says "its krateo.io/values-schema annotation is "<value>", which is not an
  opt-in: the only accepted value is "infer"". Surrounding whitespace is trimmed before the
  comparison.[^optin]

## CLI or in-cluster opt-in

| | `values-schema infer` (CLI) | `krateo.io/values-schema: infer` (opt-in) |
|---|---|---|
| Where it runs | your machine, once | core-provider, on every reconcile |
| Output | a `values.schema.json` you review and **commit**, in the chart or in a wrapper chart | an in-memory schema; nothing is committed |
| Constraints | you can add them before committing (enums, bounds, `required`) | none; the schema is typed-open |
| What the definition reads | `ValuesSchemaAuthored=True/Authored` | `ValuesSchemaAuthored=False/InferredFromValues` |

Install the CLI from plumbing (**v1.100.0** or later); it versions in lockstep with the
inference library:[^cli]

```sh
go install github.com/krateo-platformops/plumbing/cmd/values-schema@<plumbing version>
values-schema infer SOURCE [--version V] [--out FILE] [--report FILE]
values-schema infer SOURCE [--version V] --wrapper DIR --chart-version V [--name N] [--repository R] [--report FILE]
```

`SOURCE` is a chart directory, a `.tgz`, or `oci://host/path/name` with `--version`. A
directory's dependencies must be vendored (`helm dependency build`).

**Choose the CLI** when you want constraints, or a schema that only changes when someone commits
a change. **Choose the opt-in** for a chart you consume as-is, where a typed portal form and CRD
with the chart's own defaults are enough.

## The rules (rules v1)

Copied from plumbing `valuesschema` (v1.104.0, the version core-provider 2.13.123+
ships):[^rules]

```
values.yaml             inferred                                       why
non-empty map           {type: object, properties, additionalProperties: true}   D3 typed-open
{}                      {type: object}                                  free-form map
null / ~                {}                                              untyped (D7: no hints)
bool                    {type: boolean, default}                        D4 default
int, string             {x-kubernetes-int-or-string: true, default}      D5: "25%" and 1 both pass
float                   {default}                                       type: number becomes string in crdgen's caller
[]                      {type: array, default}
list of one kind        {type: array, items: <kind>, default}
mixed list              {type: array, items: {}, default}
list of maps            items = merge of every element's keys, typed-open
key `global`            {type: object}, at the top of EVERY chart       the cdc merges its own keys in;
                        that writes one; at the root ALWAYS, first      helm pushes the parent's down
dependency (name|alias) its own values.yaml merged under the parent's block, inferred open;
                        its own values.schema.json used instead when it has one (D8), with
                        local $refs rebased onto its subtree, objects that leave
                        additionalProperties unstated opened (helm treats them as open), and
                        its coalesced values overlaid as defaults
library chart dep       {type: object}                                  helm adds the key anyway
root                    open, plus x-krateo-values-schema: {source: inferred, rules}
```

- **Defaults sit on leaves only** (scalars and lists). crdgen stamps `default: {}` on each
  optional parent whose subtree holds a default, so a leaf default still applies when the whole
  map is omitted.[^rules]
- **No constraints are inferred:** no enum, bound, pattern or `required`. Every map is open, so
  the schema never refuses a value the chart itself accepts.[^rules]
- **Closing specific maps:** a chart may list dotted value paths in the Chart.yaml annotation
  `krateo.io/values-schema-closed` (for example `image,resources.limits`). Those maps get
  `additionalProperties: false`. A path that names no map is an error, so a typo can't silently
  leave a node open. It's the only annotation inference reads.[^rules]

## How to tell a schema was inferred

- **On the CompositionDefinition**, condition type `ValuesSchemaAuthored`:[^conds]

  | status | reason | when |
  |---|---|---|
  | `True` | `Authored` | the chart ships `values.schema.json` at its root |
  | `False` | `InferredFromValues` | inferred; the message gives the rules version and the paths that accept any value |
  | `Unknown` | `OwnedByAnotherDefinition` | a follower; another definition generates the CRD |

  The paths in that message name a list's items (`x[]`) where a form shows the list (`x`). Mapped
  that way they are exactly the CRD's accepts-any-value fields, empty lists included (reported since
  plumbing v1.105.0; before it, an empty list's items accepted any value with no finding).[^untyped]

  An inferred definition also reads `SchemaFullyEnforced=Unknown/SchemaInferred`: with no authored
  constraints, "all enforced" can't be claimed. **Ready is unaffected:** an inferred definition
  reads Available.[^conds]
- **In the schema itself**, at the root: `x-krateo-values-schema: {source: inferred, rules: "1"}`
  (the key is `valuesschema.MarkerKey`; `rules` is `RulesVersion`). The portal's form schema
  carries it.[^marker]
  - **core-provider owns the marker.** It removes a root `x-krateo-values-schema` from an
    **authored** schema before anything reads it, at the one seam (`chart.SpecSchema`) that feeds the
    CRD, its generation cache, the default-contradiction gate, `SpecSchemaCurrent` and the JSON-schema
    ConfigMap. So the marker is present **if and only if** `ValuesSchemaAuthored=False`, and a chart
    cannot forge it. Only the root keyword is removed: a property of that name under `properties` is
    the author's and stays. The rest of the schema keeps its authoring order and literals.[^owned]

## Limitations and risks

Phrased as of 2026-11-22.

- **Decimal defaults were lost before core-provider's bump to plumbing v1.105.0.** `ratio: 0.5` is
  inferred as `{default: 0.5}`, but crdgen dropped the default of an untyped node, so the CRD had
  none (integer, string and list defaults survived). Fixed in plumbing v1.105.0; measured on a real
  apiserver, an instance omitting `ratio` and `tuning` now reads back `ratio: 0.5` and
  `tuning.ratio: 0.5`.
  [#613](https://github.com/krateo-platformops/core-provider/issues/613)
- **A version migration can leave values the new version refuses.** If a new chart version changes a
  field's *kind* (say a boolean becomes integer-or-string), migrating an instance succeeds: the
  apiserver's validation ratcheting admits a write that leaves the field unchanged. The instance then
  holds a value the new version refuses on create, and **every** instance created under the old
  version is affected when the old schema defaulted that field, not only those that set it. This
  applies to every composition, not only inferred ones. Since the release carrying #614, the
  definition reports it: `MigratedValuesValid=False/ValuesRefusedByNewVersion` names the instances
  and fields (see [api.md](api.md#migratedvaluesvalid)).
  [#614](https://github.com/krateo-platformops/core-provider/issues/614)
- **Integer↔string changes are absorbed.** Rules v1 types integers and strings as
  `x-kubernetes-int-or-string`. Measured: kyverno 3.7.0 → 3.8.0 refuses nothing; for example
  `reportsServer.readinessTimeout` 300 → `"300s"` only moves the default (#612).
- **Silent widening.** The CRD accepts any key under every map not closed with `krateo.io/values-schema-closed`. `ValuesSchemaAuthored=False` (and
  the portal banner, when it ships) is the only signal.
- **Quantities are strings.** Keys such as `cpu`/`memory` written as strings in values.yaml infer as
  int-or-string, so both `1` and `"500m"` pass. A key whose default is a decimal is untyped.
- **No helm validation at the root:** with an inferred schema, `ValuesSchemaViolation` doesn't fire. From the #568 analysis; not measured.
- **`key: null`** (deleting a chart default) may be dropped by the apiserver on a non-nullable node.
  Reasoned, not measured.
- **Re-inference within a chart version is safe.** For the same chart version, a regenerated schema
  replaces the served one only if it's provably looser (`SpecSchemaCurrent=False/LooserSchemaPending`,
  then written). Anything else is kept and reported as
  `SpecSchemaCurrent=False/GeneratedSchemaNotLooser` until the chart version changes. Within one
  `RulesVersion`, rules may only loosen.[^stable]

[^missing]: `go/core-provider/internal/controllers/compositiondefinitions/schemacontradiction.go:51,55`; reason `ValuesSchemaMissing`, `internal/cdreason/cdreason.go:48,141`.
[^optin]: `go/core-provider/internal/tools/chart/specschema.go:32-33` (`ValuesSchemaAnnotation`, `ValuesSchemaInfer`), `:54-63` (`OptIn`, `strings.TrimSpace` at `:59`); unknown-value message `schemacontradiction.go:53`. First release with the opt-in: 2.13.117 (#596, ad52f36).
[^specschema]: `go/core-provider/internal/tools/chart/specschema.go:67-81` (`SpecSchema`: the authored file is returned first, inference runs only on `fs.ErrNotExist` with the opt-in).
[^cli]: plumbing `cmd/values-schema/main.go:7-22` (v1.104.0); first released in plumbing v1.100.0 (#219).
[^rules]: plumbing `valuesschema/valuesschema.go:16-37` (rules table), `:39-43` (defaults), `:9-14` (no constraints, open), `:45-48` and `:66-68` (`ClosedAnnotation = "krateo.io/values-schema-closed"`), v1.104.0.
[^conds]: `go/core-provider/internal/cdreason/cdreason.go:175` (type), `:220-225` (the three reasons), `:195` (`SchemaInferred`); messages `compositiondefinitions/valuesschemaauthored.go:24,32,39`.
[^marker]: plumbing `valuesschema/valuesschema.go:57-64` (`RulesVersion = "1"`, `MarkerKey = "x-krateo-values-schema"`), v1.104.0.
[^owned]: core-provider `internal/tools/chart/marker.go` (`stripRootMarker`), called from `chart.SpecSchema`'s authored branch (`internal/tools/chart/specschema.go`); tests `marker_test.go`, `marker_cache_568_test.go`, `marker_568_envtest_test.go` (#568 child 7).
[^untyped]: measured on a real apiserver by `untypedpaths_568_envtest_test.go` at plumbing v1.105.0: report `[empty[] lists[] mixedKinds[] nested.deep nothing ratio]`, stored CRD's any-value fields `[empty lists mixedKinds nested.deep nothing ratio]`; equal under the `x[]` to `x` mapping. At v1.104.0 the report lacked `empty[]`.
[^stable]: `go/core-provider/internal/cdreason/cdreason.go:200,202` (`LooserSchemaPending`, `GeneratedSchemaNotLooser`); #534; plumbing `valuesschema/valuesschema.go:57-60`.
