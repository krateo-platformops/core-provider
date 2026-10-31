package rbacguard

import (
	"path/filepath"
	"testing"
)

// chartClusterRole and controllerRoot are relative to this package. Resolved via filepath so a
// move of this package fails loudly rather than silently scanning nothing — a guard that scans an
// empty tree passes, which is the failure mode this whole package exists to prevent.
const (
	chartClusterRole = "../../../../../helm/core-provider/templates/clusterrole.yaml"
	controllerRoot   = "../.."
)

// THE GUARD. Every Kubernetes resource the controller's code asks its own client for must be
// granted by the controller's own ClusterRole in the chart.
//
// #270 is why this exists: #263 added a kube.List on pods, the chart had no pods rule, and every
// CompositionDefinition version bump on 2.13.45 wedged. Found on a peer's cluster, as a
// "Timeout: failed waiting for *v1.Pod Informer to sync" that named the informer rather than the
// permission, because the informer swallows the 403 in its retry loop.
func TestControllerClusterRoleCoversEveryClientCall(t *testing.T) {
	rules, err := ParseClusterRole(filepath.Clean(chartClusterRole))
	if err != nil {
		t.Fatalf("reading the chart's ClusterRole: %v", err)
	}
	// CALIBRATION, not decoration. A parse that produced zero rules, or a scan that found zero
	// needs, would make this test pass while checking nothing — the shape that has produced three
	// false negatives in this project today.
	if len(rules) < 5 {
		t.Fatalf("parsed only %d rules from the chart; the parser is broken and a pass here would "+
			"be meaningless", len(rules))
	}

	needs, unmapped, err := Scan(filepath.Clean(controllerRoot))
	if err != nil {
		t.Fatalf("scanning the controller: %v", err)
	}
	if len(needs) < 4 {
		t.Fatalf("found only %d grants implied by client calls; the AST scan is not matching and a "+
			"pass here would be meaningless", len(needs))
	}
	t.Logf("calibration: %d rules parsed from the chart, %d grants implied by the code", len(rules), len(needs))

	var missing []string
	for _, k := range SortedKeys(needs) {
		n := needs[k]
		ok, conditional := Grants(rules, n.Group, n.Resource)
		if !ok {
			missing = append(missing, k)
			t.Errorf("NOT GRANTED: apiGroup %q resource %q — required by %s at %v\n"+
				"    The code calls the client for this and the chart's ClusterRole does not allow "+
				"it. At runtime the informer swallows the 403 and reports a sync timeout, so the "+
				"symptom will name the informer rather than this rule (#270).",
				n.Group, n.Resource, n.GoType, n.Sites)
			continue
		}
		if conditional {
			// Not a failure: it is granted. But it is granted only when a values flag is set, and
			// a reader comparing code to chart deserves to know which grants are values-dependent.
			t.Logf("granted CONDITIONALLY (inside a values gate): %s/%s, required by %s",
				n.Group, n.Resource, n.GoType)
		}
	}

	// The blind spot, reported rather than hidden. A client call whose object comes from a helper
	// return value is not tracked by this analysis, and a type with no mapping is not checked at
	// all — so the guard says how much it could not see.
	for _, k := range SortedKeys(unmapped) {
		t.Logf("UNMAPPED type (not checked — add it to TypeGrant if it is an API object): %s at %v",
			k, unmapped[k])
	}

	if len(missing) > 0 {
		t.Errorf("%d grant(s) required by the code and absent from the chart: %v", len(missing), missing)
	}
}

// The pods rule specifically, because it is the one that caused the outage and a general guard can
// regress without anyone noticing which case it stopped covering.
func TestPodsRuleIsPresent(t *testing.T) {
	rules, err := ParseClusterRole(filepath.Clean(chartClusterRole))
	if err != nil {
		t.Fatal(err)
	}
	ok, _ := Grants(rules, "", "pods")
	if !ok {
		t.Error("core/v1 pods is not granted. waitForControllerDrained LISTs pods before revoking " +
			"a retiring controller's ServiceAccount; without this rule every CompositionDefinition " +
			"version bump wedges at Synced=False (#270).")
	}

	// CONTROL: a resource the controller has no business touching must NOT be granted, or Grants
	// is returning true for everything and the assertion above means nothing.
	if bad, _ := Grants(rules, "", "nodes"); bad {
		t.Error("control: core/v1 nodes reports as granted. Either the chart grants far more than " +
			"it should, or Grants matches indiscriminately — both make this guard worthless")
	}
}
