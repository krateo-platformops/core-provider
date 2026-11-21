package composition

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/plumbing/managementpolicy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// childComposition is a child composition (portals) carrying one Ready condition.
func childComposition(name, status, reason string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/v0-1-0", "kind": "Portal",
		"metadata": map[string]any{"name": name, "namespace": "ns"},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": status, "reason": reason},
		}},
	}}
}

// #110 D4 / #597: how a parent reads a child whose policy withholds a verb. CreateWithheldByPolicy is
// FAILED (a deliberate refusal that only a human clears; converging would wait forever), and the parent
// names the reason beside the child; UpdateWithheldByPolicy is HEALTHY. CONTROLS: Creating stays
// converging, and NoCreateVerbDeclared stays failed with no reason named, so the new arm is distinct.
func TestRollup_WithheldByPolicyChildren(t *testing.T) {
	ref := func(name string) map[string]any {
		return map[string]any{"apiVersion": "composition.krateo.io/v0-1-0", "resource": "portals", "name": name, "namespace": "ns"}
	}
	for _, tc := range []struct {
		status, reason string
		want           childState
		wantDetail     string
	}{
		{"False", "CreateWithheldByPolicy", childFailed, "CreateWithheldByPolicy"},
		{"True", "UpdateWithheldByPolicy", childHealthy, ""},
		{"False", "Creating", childConverging, ""},
		{"False", "NoCreateVerbDeclared", childFailed, ""},
	} {
		ev := krateoEval(childComposition("c", tc.status, tc.reason))
		if ev.state != tc.want || ev.detail != tc.wantDetail {
			t.Errorf("Ready=%s/%s: state %d detail %q; want %d %q", tc.status, tc.reason, ev.state, ev.detail, tc.want, tc.wantDetail)
		}
	}

	h := &handler{}
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	setManaged(mg, ref("withheld"), ref("ok"))
	v := h.rollupManagedChildren(context.Background(), fakeDynWith(
		childComposition("withheld", "False", "CreateWithheldByPolicy"),
		childComposition("ok", "True", "UpdateWithheldByPolicy"),
	), mg)
	if v.ready || v.reason != "Unavailable" {
		t.Fatalf("a child withheld from create must fail the parent: ready=%v reason=%s", v.ready, v.reason)
	}
	if len(v.failing) != 1 || v.failing[0] != "portals/ns/withheld (CreateWithheldByPolicy)" || !strings.Contains(v.message, "(CreateWithheldByPolicy)") {
		t.Errorf("the parent must name the child AND the reason: failing=%v message=%q", v.failing, v.message)
	}
}

// Diego, 2026-11-21: a withheld update replaces only the healthy verdict; every other outcome keeps its
// reason and says the update is withheld. withheld == "" changes nothing.
func TestWithUpdateWithheld(t *testing.T) {
	const w = "krateo.io/management-policy \"observe\" withholds update"
	if got := withUpdateWithheld(readyOutcome{reason: "Available", message: "m"}, w); got.reason != "UpdateWithheldByPolicy" || got.message != w {
		t.Errorf("Available: %+v; want UpdateWithheldByPolicy with the withheld message", got)
	}
	for _, r := range []string{"Creating", "Unavailable", "HooksMayNotHaveRun", "AvailablePartialCoverage", "ZeroEvidence"} {
		got := withUpdateWithheld(readyOutcome{reason: r, message: "rollup says"}, w)
		if got.reason != r || !strings.HasPrefix(got.message, "rollup says") || !strings.Contains(got.message, w) {
			t.Errorf("%s: %+v; want the reason kept and the message to add the withheld update", r, got)
		}
	}
	if got := withUpdateWithheld(readyOutcome{reason: "Available", message: "m"}, ""); got.reason != "Available" || got.message != "m" {
		t.Errorf("no withheld update must change nothing: %+v", got)
	}
}

func TestUpdateWithheldMessage(t *testing.T) {
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	mg.SetAnnotations(map[string]string{"krateo.io/management-policy": "observe"})
	d := managementpolicy.For(mg.GetAnnotations(), nil)

	for _, res := range []*helmconfig.ReconcileResult{nil, {WriteWithheld: true}} {
		if got := updateWithheldMessage(mg, d, res, "0.1.0", "0.1.0"); got != "" {
			t.Errorf("nothing would change: %q; want no message (the composition is simply Available)", got)
		}
	}
	got := updateWithheldMessage(mg, d, &helmconfig.ReconcileResult{WouldChange: true, DesiredStateChange: "chart version 0.1.0 -> 0.2.0"}, "0.1.0", "0.2.0")
	for _, want := range []string{`"observe"`, "installed chart 0.1.0", "wants chart 0.2.0", "not applied"} {
		if !strings.Contains(got, want) {
			t.Errorf("desired-state change: %q lacks %q", got, want)
		}
	}
	got = updateWithheldMessage(mg, d, &helmconfig.ReconcileResult{WouldChange: true, WouldPatch: 2,
		WouldPatchObjects: []string{"v1/ConfigMap/ns/a"}, WouldPatchObjectsOmitted: 1, WouldCreateObjects: []string{"v1/Secret/ns/s"}}, "0.1.0", "0.1.0")
	for _, want := range []string{"drift is not corrected", "would create v1/Secret/ns/s", "would patch v1/ConfigMap/ns/a and 1 more"} {
		if !strings.Contains(got, want) {
			t.Errorf("drift: %q lacks %q", got, want)
		}
	}
	if !driftUnassessable(fmt.Errorf("x: %w", helmconfig.ErrDryRunRefused)) || !driftUnassessable(fmt.Errorf("x: %w", helmconfig.ErrStoredManifestRepairWithheld)) {
		t.Error("the two WriteNothing sentinels must read as drift that could not be assessed")
	}
	if driftUnassessable(errors.New("boom")) || driftUnassessable(fmt.Errorf("x: %w", helmconfig.ErrWriteNothingConflict)) {
		t.Error("CONTROL: any other error, a WriteNothing conflict included, is a real failure")
	}
}
