package rbacguard

import (
	"path/filepath"
	"testing"
)

// #393. provider-runtime's scaletozero helper, which core-provider calls, gets and patches the
// controller Deployment's SCALE subresource and lists the Kind's instances cluster-wide. The AST
// guard above cannot see those calls: they are made inside provider-runtime, not by this repo's
// code. So the grants are asserted here by name.
//
// The subresource is the trap. "apps/deployments: *" does NOT grant deployments/scale; RBAC matches
// a subresource only by its own name or "*". Without the rule the helper's GET of the scale fails
// with 403, and by its contract that changes nothing: the controller simply never scales.
func TestControllerClusterRoleGrantsTheScaleToZeroHelper(t *testing.T) {
	rules, err := ParseClusterRole(filepath.Clean(chartClusterRole))
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) < 5 {
		t.Fatalf("parsed only %d rules; the parser is broken", len(rules))
	}
	for _, need := range []struct{ group, resource, verb string }{
		{"apps", "deployments/scale", "get"},
		{"apps", "deployments/scale", "patch"},
		{"composition.krateo.io", "fireworksapps", "list"},  // any composition Kind: the helper lists it metadata-only
		{"composition.krateo.io", "fireworksapps", "watch"}, // the wake watch
	} {
		if ok, _ := Grants(rules, need.group, need.resource, need.verb); !ok {
			t.Errorf("NOT GRANTED: %s %s %s, which the scale-to-zero reconciler needs (#393)", need.verb, need.group, need.resource)
		}
	}
	// Control: the matcher must not treat the parent resource as granting the subresource, or the
	// first two needs above would pass on a chart without the scale rule.
	if ok, _ := Grants([]Rule{{Groups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"*"}}}, "apps", "deployments/scale", "get"); ok {
		t.Error("control: a deployments-only rule must not grant deployments/scale")
	}
}
