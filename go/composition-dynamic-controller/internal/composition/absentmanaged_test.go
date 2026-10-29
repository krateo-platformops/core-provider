package composition

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// #248. On krateo-057, 7 of 83 compositions had no `status.managed` key at all and reported
// Ready=True/Available over 506 objects their deployed Helm manifests show they render. The rollup
// answered "I have no information" with "everything is fine".
//
// The assertions below are deliberately shaped as a PRECONDITION rather than as a branch test.
// Frontend's framing, and it generalises: "absent managed is not a state from which Available is
// reachable" stays true as arms are added, where "absent managed yields ZeroEvidence" only tests
// the fix. That distinction is the lesson of #244 — a per-branch test asserts only the cases
// someone thought of.

func compositionWithStatus(status map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/v1-8-85",
		"kind":       "ObservabilityPage",
		"metadata":   map[string]any{"name": "observability-page", "namespace": "krateo-system"},
		"status":     status,
	}}
}

// THE PRECONDITION. Many status shapes, all with managed ABSENT, none of which may come out
// Available. Present-and-empty is a different state and is deliberately excluded -- see
// TestRollup_EmptyManagedIsStillAvailable below. The point of the table is that it is not one shape: the 7 real cases all carried
// digest, previousDigest, helmChartUrl, helmChartVersion and observedGeneration, so a fix keyed
// accidentally on "status is empty" would have passed a single-shape test and missed every one of
// them.
func TestRollup_AbsentManagedIsNeverAvailable(t *testing.T) {
	h := &handler{}
	for _, c := range []struct {
		name   string
		status map[string]any
	}{
		{"no status at all", map[string]any{}},
		{"managed absent, everything else present (the 7 real cases)", map[string]any{
			"digest":             "abc",
			"previousDigest":     "abd",
			"helmChartUrl":       "oci://ghcr.io/krateo-platformops/charts/observabilitypage",
			"helmChartVersion":   "1.8.85",
			"observedGeneration": int64(1),
		}},
		{"managed absent, a stale Ready=True already stamped", map[string]any{
			"digest": "abc",
			"conditions": []any{map[string]any{
				"type": "Ready", "status": "True", "reason": "Available",
				"message": "Composition is up-to-date",
			}},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			// managed is absent/empty, so the rollup returns before it touches the dynamic client.
			v := h.rollupManagedChildren(context.Background(), nil, compositionWithStatus(c.status))

			if v.reason == "Available" {
				t.Errorf("reason = Available with no recorded children. This is #248: three "+
					"consumers believe it — deps: ordering, the installer's readiness gate, and the "+
					"portal's ready bucket — and nothing repairs the field, so the green is "+
					"self-perpetuating. message=%q", v.message)
			}
			if v.ready {
				t.Errorf("ready = true with no recorded children (reason %q)", v.reason)
			}
			if v.reason != "ZeroEvidence" {
				t.Errorf("reason = %q, want ZeroEvidence: the state is an absence of evidence, and "+
					"ZeroEvidence is the reason whose entire job is to say so", v.reason)
			}
		})
	}
}

// Unknown, NOT False. The state is steady rather than transient — nothing repairs `managed`, so a
// composition in it stays there — and False would block every consumer gating on readiness
// indefinitely. That is the one direction that turns an honesty fix into an outage, and this file
// documents the same wedge for #72, #112 and #165.
func TestRollup_AbsentManagedIsUnknownNotFalse(t *testing.T) {
	h := &handler{}
	v := h.rollupManagedChildren(context.Background(), nil, compositionWithStatus(map[string]any{}))

	out := resolveReady(projectedHealth{}, "Composition is up-to-date", v)
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	if err := stampReady(mg, out, true); err != nil {
		t.Fatalf("stampReady: %v", err)
	}
	status, reason, _ := readyCondition(t, mg)

	if status != "Unknown" {
		t.Errorf("Ready status = %q, want Unknown. False would wedge every `deps:` edge and the "+
			"installer gate on a steady state that never clears", status)
	}
	if reason != "ZeroEvidence" {
		t.Errorf("Ready reason = %q, want ZeroEvidence", reason)
	}
}

// THE CONTROL ARM, and it is the one that matters most: a fix that made everything ZeroEvidence
// would satisfy every assertion above. A composition with recorded, healthy children must still be
// Available.
func TestRollup_RecordedHealthyChildrenAreStillAvailable(t *testing.T) {
	h := &handler{}
	healthy := namedKrateoCR("good", "True", "Available", true)
	mg := compositionWithStatus(map[string]any{})
	setManaged(mg, krateoRef("good"))

	v := h.rollupManagedChildren(context.Background(), fakeDynWith(healthy), mg)
	if v.reason == "ZeroEvidence" {
		t.Fatal("a composition WITH a recorded child was reported ZeroEvidence; the fix has " +
			"swallowed the healthy path, which would turn every composition Unknown")
	}
}

// A blueprint's own health projection is better evidence than the generic rollup, so a declared
// ready=true must still win over an absent managed list — otherwise this fix would newly break
// every composition that has a health RESTAction but no recorded children. resolveReady's
// ZeroEvidence arm already honours it; this pins that the two interact correctly.
func TestRollup_AbsentManagedButProjectedHealthyIsAvailable(t *testing.T) {
	h := &handler{}
	v := h.rollupManagedChildren(context.Background(), nil, compositionWithStatus(map[string]any{}))

	out := resolveReady(
		projectedHealth{present: true, ready: true, message: "api answered 200"},
		"Composition is up-to-date", v)

	if out.reason != "Available" {
		t.Errorf("reason = %q, want Available: the author's health check IS evidence, and it is "+
			"better evidence than an empty generic rollup", out.reason)
	}
}

// The other side of the distinction, and the reason this fix did not have to override anyone. A
// reconcile that decoded a release and rendered nothing records `managed: []`, which is evidence of
// an evaluation that found nothing -- legitimately Available. That was an earlier author's
// position; #248 did not reverse it, it made it expressible.
func TestRollup_EmptyManagedIsStillAvailable(t *testing.T) {
	h := &handler{}
	v := h.rollupManagedChildren(context.Background(), fakeDynWith(),
		compositionWithStatus(map[string]any{"managed": []any{}, "digest": "abc"}))

	if !v.ready || v.reason != "Available" {
		t.Errorf("present-and-empty managed gave ready=%v reason=%q, want Available. Collapsing it "+
			"into the absent case would report every chart that renders nothing as unevaluated",
			v.ready, v.reason)
	}
}

// ---- the WRITE side (#248) --------------------------------------------------------------------

// A reconcile that decoded a release and rendered nothing must RECORD that, as `managed: []`.
// Without this the absent/empty distinction the rollup now relies on cannot exist: an empty render
// would leave the key absent and be reported ZeroEvidence, which is the legitimate case being
// mislabelled.
func TestSetStatus_RecordsAnEmptyRenderExplicitly(t *testing.T) {
	h := &handler{}
	mg := compositionWithStatus(map[string]any{})

	if err := h.setStatus(context.Background(), mg, &statusManagerOpts{
		force: true, recordManaged: true, resources: nil,
		message: "Composition values updated", conditionType: ConditionTypeAvailable,
	}); err != nil {
		t.Fatalf("setStatus: %v", err)
	}

	v, found, err := unstructured.NestedSlice(mg.Object, "status", "managed")
	if err != nil {
		t.Fatalf("reading status.managed: %v", err)
	}
	if !found {
		t.Fatal("status.managed is ABSENT after a reconcile that recorded an empty render; the " +
			"rollup would read that as 'never evaluated' and report ZeroEvidence for a chart that " +
			"legitimately renders nothing")
	}
	if len(v) != 0 {
		t.Errorf("status.managed = %v, want an empty list", v)
	}
}

// THE DANGEROUS DIRECTION. Observe passes resources: nil and must NOT touch the list — it is the
// phase that runs on every resync, so wiping there would drive every composition on the fleet to
// ZeroEvidence within one tick. This is the mirror of #248 and strictly worse than it.
func TestSetStatus_ObserveDoesNotWipeManaged(t *testing.T) {
	h := &handler{}
	existing := []any{
		map[string]any{"apiVersion": "v1", "resource": "configmaps", "name": "cm", "namespace": "ns"},
	}
	mg := compositionWithStatus(map[string]any{"managed": existing})

	if err := h.setStatus(context.Background(), mg, &statusManagerOpts{
		force: true, resources: nil, // recordManaged deliberately left false, as Observe leaves it
		message: "Composition is up-to-date", conditionType: ConditionTypeAvailable,
	}); err != nil {
		t.Fatalf("setStatus: %v", err)
	}

	v, found, _ := unstructured.NestedSlice(mg.Object, "status", "managed")
	if !found || len(v) != 1 {
		t.Fatalf("Observe overwrote status.managed (found=%v len=%d). Every composition would go "+
			"ZeroEvidence on the next resync", found, len(v))
	}
}

// M4 from mutation: `var managed []interface{}` instead of `[]interface{}{}` survived every
// assertion above, because a typed nil slice satisfies NestedSlice's type assertion in-process --
// found=true, len=0, indistinguishable from an empty list. The difference only appears after a
// round trip through the apiserver, where a nil slice marshals to `null`, and `null` read back is
// NOT a slice. So assert the SERIALISED form, which is what the cluster actually stores.
func TestSetStatus_EmptyRenderSerialisesAsListNotNull(t *testing.T) {
	h := &handler{}
	mg := compositionWithStatus(map[string]any{})
	if err := h.setStatus(context.Background(), mg, &statusManagerOpts{
		force: true, recordManaged: true, resources: nil,
		message: "Composition values updated", conditionType: ConditionTypeAvailable,
	}); err != nil {
		t.Fatalf("setStatus: %v", err)
	}

	b, err := json.Marshal(mg.Object["status"])
	if err != nil {
		t.Fatalf("marshalling status: %v", err)
	}
	s := string(b)
	if strings.Contains(s, `"managed":null`) {
		t.Errorf(`status serialises "managed":null. Stored on the cluster that reads back as a `+
			`non-slice, so NestedSlice fails and the rollup sees ABSENT -- reporting ZeroEvidence `+
			`for a chart that legitimately renders nothing. Want "managed":[]. status=%s`, s)
	}
	if !strings.Contains(s, `"managed":[]`) {
		t.Errorf(`status does not serialise "managed":[]: %s`, s)
	}
}

// M2 from mutation: flipping `recordManaged: true` to false at the real call sites broke nothing,
// because every test above supplies the flag itself. That is the #232 shape again -- the decision
// covered, the call site not -- so this asserts the CO-OCCURRENCE syntactically: any
// statusManagerOpts literal that passes decoded resources must also set recordManaged.
//
// A source walk can prove this one, because the claim is syntactic (two fields in one literal)
// rather than about reachability -- which is the distinction an earlier guard of mine got wrong.
// The non-emptiness checks below are the control that stops it passing by finding nothing.
func TestSetStatusCallSitesWithResourcesAlsoRecordManaged(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "composition.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing composition.go: %v", err)
	}

	var literals, withResources, alsoRecord int
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "statusManagerOpts" {
			return true
		}
		literals++
		var hasRes, hasRecord bool
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			k, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			// `resources: nil` is Observe declining to record; only a non-nil value counts.
			if k.Name == "resources" {
				if id, isNil := kv.Value.(*ast.Ident); !isNil || id.Name != "nil" {
					hasRes = true
				}
			}
			if k.Name == "recordManaged" {
				if id, ok := kv.Value.(*ast.Ident); ok && id.Name == "true" {
					hasRecord = true
				}
			}
		}
		if hasRes {
			withResources++
			if hasRecord {
				alsoRecord++
			}
		}
		return true
	})

	if literals == 0 {
		t.Fatal("found no statusManagerOpts literals in composition.go — the walk is not finding " +
			"what it thinks it is, so a mismatch could never be detected")
	}
	if withResources == 0 {
		t.Fatal("found no statusManagerOpts literal passing non-nil resources — same problem, " +
			"other side: this guard would pass while asserting nothing")
	}
	if alsoRecord != withResources {
		t.Errorf("%d of %d statusManagerOpts literals that pass decoded resources set "+
			"recordManaged: true. A call site that decodes a release and does not record it leaves "+
			"status.managed absent, which the rollup reads as 'never evaluated' (#248).",
			alsoRecord, withResources)
	}
}
